package platforms

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

// ResySlot is one bookable slot from /4/find.
type ResySlot struct {
	Time        string  `json:"time"`         // HH:MM local
	Start       string  `json:"start"`        // raw date.start
	End         string  `json:"end"`          // raw date.end
	SeatingType string  `json:"seating_type"` // config.type e.g. Dining Room
	ConfigID    string  `json:"config_id"`
	Token       string  `json:"token"`
	SizeMin     int     `json:"size_min"`
	SizeMax     int     `json:"size_max"`
	IsPaid      bool    `json:"is_paid"`
	Amount      float64 `json:"amount,omitempty"`
	Currency    string  `json:"currency,omitempty"`
	Quantity    int     `json:"quantity,omitempty"`
}

// Key uniquely identifies a slot for diffing across polls.
func (s ResySlot) Key() string {
	return s.Time + "|" + s.SeatingType + "|" + s.ConfigID
}

type resyFindResponseFull struct {
	Results struct {
		Venues []struct {
			Slots []struct {
				Config struct {
					Token string `json:"token"`
					Type  string `json:"type"`
					ID    any    `json:"id"`
				} `json:"config"`
				Date struct {
					Start string `json:"start"`
					End   string `json:"end"`
				} `json:"date"`
				Size struct {
					Min int `json:"min"`
					Max int `json:"max"`
				} `json:"size"`
				Payment struct {
					IsPaid   bool    `json:"is_paid"`
					Amount   float64 `json:"amount"`
					Currency string  `json:"currency"`
				} `json:"payment"`
				Quantity int `json:"quantity"`
			} `json:"slots"`
		} `json:"venues"`
	} `json:"results"`
}

// FindSlots calls /4/find and returns every slot with seating metadata.
func (r *Resy) FindSlots(ctx context.Context, client *http.Client) ([]ResySlot, []byte, error) {
	u, _ := url.Parse(resyBase + "/4/find")
	q := u.Query()
	q.Set("venue_id", r.cfg.Resy.VenueID)
	q.Set("party_size", fmt.Sprintf("%d", r.cfg.PartySize))
	q.Set("day", r.cfg.TargetDate)
	q.Set("lat", "0")
	q.Set("long", "0")
	if tf := r.cfg.Resy.TimeFilter; tf != "" {
		q.Set("time_filter", tf)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, nil, err
	}
	r.setHeaders(req)

	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode >= 300 {
		if isRateLimitStatus(resp.StatusCode) {
			return nil, body, fmt.Errorf("%w: find status %d", ErrRateLimited, resp.StatusCode)
		}
		return nil, body, fmt.Errorf("find status %d: %s", resp.StatusCode, truncate(body, 256))
	}

	slots, err := parseFindSlots(body)
	return slots, body, err
}

func parseFindSlots(body []byte) ([]ResySlot, error) {
	var parsed resyFindResponseFull
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}

	var out []ResySlot
	for _, venue := range parsed.Results.Venues {
		for _, slot := range venue.Slots {
			slotTime := extractTime(slot.Date.Start)
			if slotTime == "" || slot.Config.Token == "" {
				continue
			}
			out = append(out, ResySlot{
				Time:        slotTime,
				Start:       slot.Date.Start,
				End:         slot.Date.End,
				SeatingType: slot.Config.Type,
				ConfigID:    configIDString(slot.Config.ID),
				Token:       slot.Config.Token,
				SizeMin:     slot.Size.Min,
				SizeMax:     slot.Size.Max,
				IsPaid:      slot.Payment.IsPaid,
				Amount:      slot.Payment.Amount,
				Currency:    slot.Payment.Currency,
				Quantity:    slot.Quantity,
			})
		}
	}
	return out, nil
}

func configIDString(id any) string {
	switch v := id.(type) {
	case string:
		return v
	case float64:
		return strconv.FormatInt(int64(v), 10)
	case json.Number:
		return v.String()
	default:
		if id == nil {
			return ""
		}
		return fmt.Sprint(id)
	}
}

// SlotPoller is implemented by platforms that support inventory observation.
type SlotPoller interface {
	Platform
	FindSlots(ctx context.Context, client *http.Client) ([]ResySlot, []byte, error)
}
