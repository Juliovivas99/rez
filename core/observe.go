package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/rs/zerolog/log"

	"reservation-bot/config"
	"reservation-bot/platforms"
)

// ObserveOptions controls drop-window inventory tracing.
type ObserveOptions struct {
	Dir           string
	PollInterval  time.Duration
	LeadTime      time.Duration
	SkipDropWait  bool // true when main thread runs WaitUntil (snipe + observe together)
}

// ObserveHandle runs observation in the background.
type ObserveHandle struct {
	done   chan struct{}
	report *ObserveReport
	err    error
}

// StartObserve launches RunObserve in a goroutine.
func StartObserve(
	ctx context.Context,
	cfg *config.Config,
	scheduler *Scheduler,
	poller platforms.SlotPoller,
	client *http.Client,
	dropAt time.Time,
	endAt time.Time,
	probeURL string,
	probeReq ConfigureClockProbe,
	opts ObserveOptions,
) *ObserveHandle {
	h := &ObserveHandle{done: make(chan struct{})}
	go func() {
		defer close(h.done)
		h.report, h.err = RunObserve(ctx, cfg, scheduler, poller, client, dropAt, endAt, probeURL, probeReq, opts)
	}()
	return h
}

// Wait blocks until observation finishes.
func (h *ObserveHandle) Wait() (*ObserveReport, error) {
	<-h.done
	return h.report, h.err
}

// SlotPollEvent is one /4/find snapshot written to slot-polls.jsonl.
type SlotPollEvent struct {
	At            string              `json:"at"`
	OffsetFromDropMS int64            `json:"offset_from_drop_ms"`
	SlotCount     int                 `json:"slot_count"`
	NewSlots      []platforms.ResySlot `json:"new_slots,omitempty"`
	RemovedKeys   []string            `json:"removed_keys,omitempty"`
	Slots         []platforms.ResySlot `json:"slots"`
	Error         string              `json:"error,omitempty"`
}

// ObserveReport is the end-of-run summary for a drop observation.
type ObserveReport struct {
	Restaurant       string                    `json:"restaurant"`
	VenueID          string                    `json:"venue_id"`
	TargetDate       string                    `json:"target_date"`
	PartySize        int                       `json:"party_size"`
	DropAt           string                    `json:"drop_at"`
	ObserveStartedAt string                    `json:"observe_started_at"`
	ObserveEndedAt   string                    `json:"observe_ended_at"`
	PollCount        int                       `json:"poll_count"`
	FindAPICalls     int                       `json:"find_api_calls"`
	FirstSlotsAt     string                    `json:"first_slots_at,omitempty"`
	FirstSlotCount   int                       `json:"first_slot_count"`
	AtDropSlotCount  int                       `json:"at_drop_slot_count"`
	PeakSlotCount    int                       `json:"peak_slot_count"`
	PeakAt           string                    `json:"peak_at,omitempty"`
	ReleasedAtDrop   []platforms.ResySlot      `json:"released_at_drop"`
	AllSlotsSeen     []ObservedSlotFirstSeen   `json:"all_slots_seen"`
	BySeatingType    map[string]int            `json:"by_seating_type"`
	ByTime           map[string]int            `json:"by_time"`
	Timeline         []SlotReleaseTimelineItem `json:"timeline"`
}

type ObservedSlotFirstSeen struct {
	platforms.ResySlot
	FirstSeenAt        string `json:"first_seen_at"`
	OffsetFromDropMS   int64  `json:"offset_from_drop_ms"`
}

type SlotReleaseTimelineItem struct {
	At               string `json:"at"`
	OffsetFromDropMS int64  `json:"offset_from_drop_ms"`
	NewCount         int    `json:"new_count"`
	TotalCount       int    `json:"total_count"`
}

// RunObserve polls Resy /4/find around drop time and writes trace + summary files.
// It sleeps until drop-lead, spin-waits at drop, then polls through endAt.
func RunObserve(
	ctx context.Context,
	cfg *config.Config,
	scheduler *Scheduler,
	poller platforms.SlotPoller,
	client *http.Client,
	dropAt time.Time,
	endAt time.Time,
	probeURL string,
	probeReq ConfigureClockProbe,
	opts ObserveOptions,
) (*ObserveReport, error) {
	if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
		return nil, err
	}

	slotFile, err := os.Create(filepath.Join(opts.Dir, "slot-polls.jsonl"))
	if err != nil {
		return nil, err
	}
	defer slotFile.Close()
	slotEnc := json.NewEncoder(slotFile)

	report := &ObserveReport{
		Restaurant:   cfg.RestaurantName,
		VenueID:      cfg.Resy.VenueID,
		TargetDate:   cfg.TargetDate,
		PartySize:    cfg.PartySize,
		DropAt:       dropAt.Format(time.RFC3339),
		BySeatingType: make(map[string]int),
		ByTime:        make(map[string]int),
	}

	startAt := dropAt.Add(-opts.LeadTime)
	if scheduler.Now().Before(startAt) {
		wait := startAt.Sub(scheduler.Now())
		log.Info().Dur("wait", wait).Time("observe_start", startAt).Msg("waiting to begin observe polling")
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}

	report.ObserveStartedAt = scheduler.Now().Format(time.RFC3339Nano)

	var (
		prevKeys       = make(map[string]struct{})
		firstSeen      = make(map[string]ObservedSlotFirstSeen)
		atDropSnap     []platforms.ResySlot
		atDropRecorded bool
		peakCount     int
		peakAt        time.Time
		dropWaitDone  bool
	)

	poll := func() error {
		now := scheduler.Now()
		offsetMS := now.Sub(dropAt).Milliseconds()

		slots, _, err := poller.FindSlots(ctx, client)
		report.PollCount++
		report.FindAPICalls++

		ev := SlotPollEvent{
			At:               now.UTC().Format(time.RFC3339Nano),
			OffsetFromDropMS: offsetMS,
			SlotCount:        len(slots),
			Slots:            slots,
		}
		if err != nil {
			ev.Error = err.Error()
			_ = slotEnc.Encode(ev)
			log.Warn().Err(err).Int64("offset_from_drop_ms", offsetMS).Msg("observe poll failed")
			return err
		}

		curKeys := make(map[string]struct{}, len(slots))
		for _, s := range slots {
			curKeys[s.Key()] = struct{}{}
			if _, ok := firstSeen[s.Key()]; !ok {
				firstSeen[s.Key()] = ObservedSlotFirstSeen{
					ResySlot:         s,
					FirstSeenAt:      now.UTC().Format(time.RFC3339Nano),
					OffsetFromDropMS: offsetMS,
				}
			}
		}

		for k := range curKeys {
			if _, ok := prevKeys[k]; !ok {
				for _, s := range slots {
					if s.Key() == k {
						ev.NewSlots = append(ev.NewSlots, s)
						break
					}
				}
			}
		}
		for k := range prevKeys {
			if _, ok := curKeys[k]; !ok {
				ev.RemovedKeys = append(ev.RemovedKeys, k)
			}
		}
		sort.Slice(ev.NewSlots, func(i, j int) bool {
			if ev.NewSlots[i].Time != ev.NewSlots[j].Time {
				return ev.NewSlots[i].Time < ev.NewSlots[j].Time
			}
			return ev.NewSlots[i].SeatingType < ev.NewSlots[j].SeatingType
		})
		sort.Strings(ev.RemovedKeys)

		if len(ev.NewSlots) > 0 {
			report.Timeline = append(report.Timeline, SlotReleaseTimelineItem{
				At:               ev.At,
				OffsetFromDropMS: offsetMS,
				NewCount:         len(ev.NewSlots),
				TotalCount:       len(slots),
			})
			log.Info().
				Int64("offset_from_drop_ms", offsetMS).
				Int("new_slots", len(ev.NewSlots)).
				Int("total_slots", len(slots)).
				Msg("slots appeared")
			for _, s := range ev.NewSlots {
				log.Info().
					Str("time", s.Time).
					Str("seating", s.SeatingType).
					Str("config_id", s.ConfigID).
					Msg("new slot")
			}
		}

		if report.FirstSlotsAt == "" && len(slots) > 0 {
			report.FirstSlotsAt = ev.At
			report.FirstSlotCount = len(slots)
		}

		if !atDropRecorded && !now.Before(dropAt) {
			atDropSnap = append([]platforms.ResySlot(nil), slots...)
			report.AtDropSlotCount = len(slots)
			atDropRecorded = true
		}

		if len(slots) > peakCount {
			peakCount = len(slots)
			peakAt = now
		}

		_ = slotEnc.Encode(ev)
		prevKeys = curKeys
		return nil
	}

	ticker := time.NewTicker(opts.PollInterval)
	defer ticker.Stop()

	for {
		if !opts.SkipDropWait && !dropWaitDone {
			now := scheduler.Now()
			if !now.Before(dropAt.Add(-3 * time.Second)) {
				dropWaitDone = true
				if err := scheduler.WaitUntil(ctx, dropAt, client, probeURL, probeReq, false); err != nil {
					return report, err
				}
			}
		}

		if err := poll(); err != nil && ctx.Err() != nil {
			break
		}
		if !scheduler.Now().Before(endAt) {
			break
		}

		select {
		case <-ctx.Done():
			return report, ctx.Err()
		case <-ticker.C:
		}
	}

	report.ObserveEndedAt = scheduler.Now().Format(time.RFC3339Nano)
	report.PeakSlotCount = peakCount
	if !peakAt.IsZero() {
		report.PeakAt = peakAt.UTC().Format(time.RFC3339Nano)
	}

	report.ReleasedAtDrop = atDropSnap
	if len(atDropSnap) == 0 {
		// Drop instant may fall between polls; use slots first seen within 2s after drop.
		const postDropWindowMS = 2000
		for _, s := range firstSeen {
			if s.OffsetFromDropMS >= 0 && s.OffsetFromDropMS <= postDropWindowMS {
				report.ReleasedAtDrop = append(report.ReleasedAtDrop, s.ResySlot)
			}
		}
		sort.Slice(report.ReleasedAtDrop, func(i, j int) bool {
			if report.ReleasedAtDrop[i].Time != report.ReleasedAtDrop[j].Time {
				return report.ReleasedAtDrop[i].Time < report.ReleasedAtDrop[j].Time
			}
			return report.ReleasedAtDrop[i].SeatingType < report.ReleasedAtDrop[j].SeatingType
		})
	}

	keys := make([]string, 0, len(firstSeen))
	for k := range firstSeen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		s := firstSeen[k]
		report.AllSlotsSeen = append(report.AllSlotsSeen, s)
		st := s.SeatingType
		if st == "" {
			st = "(unknown)"
		}
		report.BySeatingType[st]++
		report.ByTime[s.Time]++
	}

	reportPath := filepath.Join(opts.Dir, "observe-report.json")
	f, err := os.Create(reportPath)
	if err != nil {
		return report, err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(report); err != nil {
		f.Close()
		return report, err
	}
	f.Close()

	log.Info().
		Str("dir", opts.Dir).
		Int("polls", report.PollCount).
		Int("released_at_drop", len(report.ReleasedAtDrop)).
		Int("all_unique_slots", len(report.AllSlotsSeen)).
		Int("peak_slots", report.PeakSlotCount).
		Msg("observe complete")

	fmt.Fprintf(os.Stderr, "\n=== Observe summary ===\n")
	fmt.Fprintf(os.Stderr, "Output: %s\n", opts.Dir)
	fmt.Fprintf(os.Stderr, "Slots at drop (or first 2s): %d\n", len(report.ReleasedAtDrop))
	fmt.Fprintf(os.Stderr, "Peak inventory: %d\n", report.PeakSlotCount)
	if len(report.ReleasedAtDrop) > 0 {
		fmt.Fprintf(os.Stderr, "\nReleased at drop:\n")
		for _, s := range report.ReleasedAtDrop {
			fmt.Fprintf(os.Stderr, "  %s  %s  (config %s)\n", s.Time, s.SeatingType, s.ConfigID)
		}
	}
	fmt.Fprintf(os.Stderr, "\n")

	return report, nil
}
