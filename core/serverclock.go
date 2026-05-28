package core

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

const (
	serverClockProbes     = 3
	minServerClockSamples = 2
)

// ConfigureClockProbe optionally sets auth headers on clock probe requests.
type ConfigureClockProbe func(req *http.Request)

// measureServerClockOffset estimates how far ahead/behind the reservation API clock
// is relative to NTP-adjusted local time, using the HTTP Date response header.
//
// offset is added after NTPOffset in Now(): reservation_time ≈ time.Now() + NTPOffset + ServerOffset
func measureServerClockOffset(
	ctx context.Context,
	client *http.Client,
	probeURL string,
	ntpOffset time.Duration,
	configure ConfigureClockProbe,
) (time.Duration, int, time.Duration, error) {
	if probeURL == "" {
		return 0, 0, 0, fmt.Errorf("empty clock probe URL")
	}

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		good     []time.Duration
		fallback []time.Duration
	)

	for i := 0; i < serverClockProbes; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			t0 := time.Now()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
			if err != nil {
				return
			}
			if configure != nil {
				configure(req)
			}

			resp, err := client.Do(req)
			t1 := time.Now()
			if err != nil {
				log.Debug().Err(err).Str("url", probeURL).Msg("server clock probe failed")
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()

			dateHdr := resp.Header.Get("Date")
			if dateHdr == "" {
				log.Debug().Str("url", probeURL).Msg("server clock probe missing Date header")
				return
			}
			serverTime, err := http.ParseTime(dateHdr)
			if err != nil {
				log.Debug().Err(err).Str("date", dateHdr).Msg("server clock probe bad Date header")
				return
			}

			rtt := t1.Sub(t0)
			mid := t0.Add(rtt / 2)
			// Cristian's algorithm: server clock at RTT midpoint vs NTP-corrected local midpoint.
			localNTPCorrected := mid.Add(ntpOffset)
			offset := serverTime.Sub(localNTPCorrected)

			ok := resp.StatusCode > 0 && resp.StatusCode < 400
			mu.Lock()
			if ok {
				good = append(good, offset)
			} else {
				fallback = append(fallback, offset)
			}
			mu.Unlock()

			log.Debug().
				Str("url", probeURL).
				Dur("offset_ms", offset).
				Dur("rtt_ms", rtt).
				Str("server_date", serverTime.Format(time.RFC3339)).
				Int("status", resp.StatusCode).
				Bool("ok", ok).
				Msg("server clock sample")
		}()
	}
	wg.Wait()

	offsets := good
	if len(offsets) < minServerClockSamples {
		offsets = append(offsets, fallback...)
	}
	if len(offsets) < minServerClockSamples {
		return 0, len(offsets), 0, fmt.Errorf("server clock: got %d/%d probe responses with Date", len(offsets), minServerClockSamples)
	}

	sort.Slice(offsets, func(i, j int) bool { return offsets[i] < offsets[j] })
	median := offsets[len(offsets)/2]
	spread := offsets[len(offsets)-1] - offsets[0]
	return median, len(offsets), spread, nil
}
