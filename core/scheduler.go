package core

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/beevik/ntp"
	"github.com/rs/zerolog/log"
)

// Well-known NTP sources; median offset reduces impact of a bad/stratum peer.
var ntpServers = []string{
	"time.google.com",
	"time.cloudflare.com",
	"pool.ntp.org",
	"time.aws.com",
	"time.apple.com",
}

const (
	ntpTimeout        = 3 * time.Second
	minNTPSamples     = 3
	maxOffsetSpread   = 100 * time.Millisecond
)

// Scheduler provides NTP- and server-adjusted timing and precision wait.
type Scheduler struct {
	NTPOffset    time.Duration
	ServerOffset time.Duration
	lastNTPSamples  int
	lastNTPSpread   time.Duration
	lastSrvSamples  int
	lastSrvSpread   time.Duration
}

type ntpSample struct {
	server string
	offset time.Duration
	rtt    time.Duration
	err    error
}

// SyncNTP queries multiple NTP servers and applies the median clock offset.
func SyncNTP() (*Scheduler, error) {
	s := &Scheduler{}
	if err := s.resyncNTP(); err != nil {
		return nil, err
	}
	return s, nil
}

// SyncServerClock probes the reservation API Date header to measure server clock skew.
func (s *Scheduler) SyncServerClock(ctx context.Context, client *http.Client, probeURL string, configure ConfigureClockProbe) error {
	return s.resyncServerClock(ctx, client, probeURL, configure)
}

// TotalOffset is NTP + server skew applied in Now().
func (s *Scheduler) TotalOffset() time.Duration {
	return s.NTPOffset + s.ServerOffset
}

func (s *Scheduler) resyncNTP() error {
	offset, samples, spread, err := medianClockOffset()
	if err != nil {
		return err
	}
	s.NTPOffset = offset
	s.lastNTPSamples = samples
	s.lastNTPSpread = spread

	ev := log.Info().
		Dur("ntp_offset_ms", offset).
		Int("samples", samples).
		Dur("spread_ms", spread)
	if spread > maxOffsetSpread {
		ev = ev.Str("warning", "high NTP spread between servers; check chrony/system clock")
	}
	ev.Msg("ntp sync complete")
	return nil
}

func (s *Scheduler) resyncServerClock(ctx context.Context, client *http.Client, probeURL string, configure ConfigureClockProbe) error {
	offset, samples, spread, err := measureServerClockOffset(ctx, client, probeURL, s.NTPOffset, configure)
	if err != nil {
		return err
	}
	s.ServerOffset = offset
	s.lastSrvSamples = samples
	s.lastSrvSpread = spread

	ev := log.Info().
		Str("probe_url", probeURL).
		Dur("server_offset_ms", offset).
		Int("samples", samples).
		Dur("spread_ms", spread).
		Dur("total_offset_ms", s.TotalOffset())
	if spread > maxOffsetSpread {
		ev = ev.Str("warning", "high server clock spread; re-probe recommended")
	}
	ev.Msg("reservation server clock sync complete")
	return nil
}

func medianClockOffset() (time.Duration, int, time.Duration, error) {
	opts := ntp.QueryOptions{Timeout: ntpTimeout, Version: 4}
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results []ntpSample
	)

	for _, server := range ntpServers {
		server := server
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			resp, err := ntp.QueryWithOptions(server, opts)
			rtt := time.Since(start)

			sample := ntpSample{server: server, rtt: rtt, err: err}
			if err == nil {
				sample.offset = resp.ClockOffset
			}

			mu.Lock()
			results = append(results, sample)
			mu.Unlock()

			if err != nil {
				log.Debug().Err(err).Str("server", server).Msg("ntp query failed")
			} else {
				log.Debug().
					Str("server", server).
					Dur("offset_ms", sample.offset).
					Dur("rtt_ms", rtt).
					Msg("ntp sample")
			}
		}()
	}
	wg.Wait()

	var offsets []time.Duration
	for _, r := range results {
		if r.err == nil {
			offsets = append(offsets, r.offset)
		}
	}

	if len(offsets) < minNTPSamples {
		return 0, len(offsets), 0, fmt.Errorf("ntp: got %d/%d server responses", len(offsets), minNTPSamples)
	}

	sort.Slice(offsets, func(i, j int) bool { return offsets[i] < offsets[j] })
	median := offsets[len(offsets)/2]
	spread := offsets[len(offsets)-1] - offsets[0]
	return median, len(offsets), spread, nil
}

func (s *Scheduler) Now() time.Time {
	return time.Now().Add(s.TotalOffset())
}

// WaitUntil sleeps until target-3s, re-syncs clocks, spin-waits the final 3 seconds,
// and optionally sends a HEAD request during the spin window to pre-warm TLS.
func (s *Scheduler) WaitUntil(
	ctx context.Context,
	target time.Time,
	client *http.Client,
	probeURL string,
	configure ConfigureClockProbe,
	skipAPIProbes bool,
) error {
	now := s.Now()
	spinStart := target.Add(-3 * time.Second)

	if now.Before(spinStart) {
		sleepDur := spinStart.Sub(now)
		log.Info().
			Time("target", target).
			Dur("sleep", sleepDur).
			Msg("sleeping until spin window")
		timer := time.NewTimer(sleepDur)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}

	// Re-sync both clocks immediately before the critical window.
	if err := s.resyncNTP(); err != nil {
		log.Warn().Err(err).Msg("pre-drop ntp resync failed; using prior offset")
	}
	if !skipAPIProbes {
		if err := s.resyncServerClock(ctx, client, probeURL, configure); err != nil {
			log.Warn().Err(err).Msg("pre-drop server clock resync failed; using prior offset")
		}
		if probeURL != "" && client != nil {
			go s.prewarm(ctx, client, probeURL, configure)
		}
	}

	log.Info().Msg("entering spin-wait for final 3 seconds")
	for s.Now().Before(target) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}

	drift := s.Now().Sub(target)
	log.Info().
		Time("target", target).
		Time("actual", s.Now()).
		Dur("drift_ms", drift).
		Dur("ntp_offset_ms", s.NTPOffset).
		Dur("server_offset_ms", s.ServerOffset).
		Dur("total_offset_ms", s.TotalOffset()).
		Int("ntp_samples", s.lastNTPSamples).
		Int("server_samples", s.lastSrvSamples).
		Msg("drop time reached")
	return nil
}

func (s *Scheduler) prewarm(ctx context.Context, client *http.Client, url string, configure ConfigureClockProbe) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		log.Warn().Err(err).Str("url", url).Msg("prewarm request build failed")
		return
	}
	if configure != nil {
		configure(req)
	}
	start := time.Now()
	resp, err := client.Do(req)
	latency := time.Since(start)
	if err != nil {
		log.Warn().Err(err).Str("url", url).Int64("latency_ms", latency.Milliseconds()).Msg("prewarm failed")
		return
	}
	resp.Body.Close()
	log.Info().Str("url", url).Int("status", resp.StatusCode).Int64("latency_ms", latency.Milliseconds()).Msg("connection pre-warmed")
}
