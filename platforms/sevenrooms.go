package platforms

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"reservation-bot/config"
)

const sevenRoomsBase = "https://www.sevenrooms.com/api-yoa"

type SevenRooms struct {
	cfg *config.Config
}

func (s *SevenRooms) Name() string { return "sevenrooms" }

func (s *SevenRooms) ClockProbeURL() string {
	return sevenRoomsBase + "/availability/slot/list"
}

func (s *SevenRooms) ConfigureClockProbe(req *http.Request) {
	s.setHeaders(req)
	req.Header.Set("Content-Type", "application/json")
}

type slotListRequest struct {
	Venue      string `json:"venue"`
	PartySize  int    `json:"party_size"`
	StartDate  string `json:"start_date"`
	EndDate    string `json:"end_date"`
	StartTime  string `json:"start_time"`
	EndTime    string `json:"end_time"`
	Channel    string `json:"channel"`
}

type slotListResponse struct {
	Data struct {
		Availability []struct {
			Time               string `json:"time"`
			SlotAvailabilityID string `json:"slot_availability_id"`
		} `json:"availability"`
	} `json:"data"`
}

type createReservationRequest struct {
	VenueID            string `json:"venue_id"`
	SlotAvailabilityID string `json:"slot_availability_id"`
	PartySize          int    `json:"party_size"`
	FirstName          string `json:"first_name"`
	LastName           string `json:"last_name"`
	Email              string `json:"email"`
	PhoneNumber        string `json:"phone_number"`
}

func (s *SevenRooms) Snipe(ctx context.Context, client *http.Client, cfg *config.Config, dryRun bool, onAttempt func(AttemptLog)) (*SnipeResult, error) {
	start := time.Now()
	slotMap, err := s.fetchSlots(ctx, client)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		onAttempt(AttemptLog{SlotTime: "*", Status: "availability_error", LatencyMS: latency, Detail: err.Error()})
		return nil, fmt.Errorf("no sevenrooms slots available")
	}
	onAttempt(AttemptLog{SlotTime: "*", Status: "availability_ok", LatencyMS: latency, Detail: fmt.Sprintf("%d slots", len(slotMap))})

	var (
		bookOnce  sync.Once
		result    *SnipeResult
		resultErr error
		wg        sync.WaitGroup
	)

	for _, preferred := range cfg.PreferredTimes {
		preferred := preferred
		slotID := slotMap[normalizeTime(preferred)]
		if slotID == "" {
			onAttempt(AttemptLog{SlotTime: preferred, Status: "no_slot", LatencyMS: 0})
			continue
		}

		wg.Add(1)
		go func() {
			defer wg.Done()

			if dryRun {
				bookOnce.Do(func() {
					body, _ := json.Marshal(map[string]string{
						"dry_run":              "true",
						"slot_availability_id": slotID,
						"slot_time":            preferred,
					})
					result = &SnipeResult{SlotTime: preferred, ConfirmationBody: body, DryRun: true}
					onAttempt(AttemptLog{SlotTime: preferred, Status: "dry_run_ok", LatencyMS: 0})
				})
				return
			}

			bookOnce.Do(func() {
				start := time.Now()
				body, err := s.createReservation(ctx, client, slotID)
				latency := time.Since(start).Milliseconds()
				if err != nil {
					resultErr = err
					onAttempt(AttemptLog{SlotTime: preferred, Status: "book_error", LatencyMS: latency, Detail: err.Error()})
					return
				}
				onAttempt(AttemptLog{SlotTime: preferred, Status: "book_ok", LatencyMS: latency})
				result = &SnipeResult{SlotTime: preferred, ConfirmationBody: body}
			})
		}()
	}

	wg.Wait()

	if result != nil {
		return result, nil
	}
	if resultErr != nil {
		return nil, resultErr
	}
	return nil, fmt.Errorf("no sevenrooms slots available")
}

func (s *SevenRooms) fetchSlots(ctx context.Context, client *http.Client) (map[string]string, error) {
	reqBody := slotListRequest{
		Venue:     s.cfg.SevenRooms.VenueID,
		PartySize: s.cfg.PartySize,
		StartDate: s.cfg.TargetDate,
		EndDate:   s.cfg.TargetDate,
		StartTime: "00:00",
		EndTime:   "23:00",
		Channel:   "SEVENROOMS_WIDGET",
	}
	raw, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sevenRoomsBase+"/availability/slot/list", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	s.setHeaders(req)
	req.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := client.Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("slot list status %d (latency %dms): %s", resp.StatusCode, latency, truncate(body, 256))
	}

	var parsed slotListResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}

	out := make(map[string]string)
	for _, slot := range parsed.Data.Availability {
		t := normalizeTime(slot.Time)
		if slot.SlotAvailabilityID != "" {
			out[t] = slot.SlotAvailabilityID
		}
	}
	return out, nil
}

func (s *SevenRooms) createReservation(ctx context.Context, client *http.Client, slotID string) ([]byte, error) {
	reqBody := createReservationRequest{
		VenueID:            s.cfg.SevenRooms.VenueID,
		SlotAvailabilityID: slotID,
		PartySize:          s.cfg.PartySize,
		FirstName:          s.cfg.Guest.FirstName,
		LastName:           s.cfg.Guest.LastName,
		Email:              s.cfg.Guest.Email,
		PhoneNumber:        s.cfg.Guest.Phone,
	}
	raw, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sevenRoomsBase+"/reservation/create", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	s.setHeaders(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("create status %d: %s", resp.StatusCode, string(body))
	}
	return body, nil
}

func (s *SevenRooms) setHeaders(req *http.Request) {
	req.Header.Set("User-Agent", "Mozilla/5.0")
	if s.cfg.SevenRooms.ClientToken != "" {
		req.Header.Set("X-Client-Token", s.cfg.SevenRooms.ClientToken)
	}
	for k, v := range s.cfg.SevenRooms.Headers {
		req.Header.Set(k, v)
	}
}

func normalizeTime(t string) string {
	if norm, err := config.ParseTimeHM(t); err == nil {
		return norm
	}
	return t
}
