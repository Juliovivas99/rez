package platforms

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"reservation-bot/config"
)

const resyBase = "https://api.resy.com"

type Resy struct {
	cfg               *config.Config
	paymentMethodID   int
	paymentMethodOnce sync.Once
	paymentMethodErr  error
}

type resyUserResponse struct {
	PaymentMethodID int `json:"payment_method_id"`
	PaymentMethods  []struct {
		ID        int  `json:"id"`
		IsDefault bool `json:"is_default"`
	} `json:"payment_methods"`
}

func (r *Resy) Name() string { return "resy" }

func (r *Resy) ClockProbeURL() string {
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
	return u.String()
}

func (r *Resy) ConfigureClockProbe(req *http.Request) {
	r.setHeaders(req)
}

type resyDetailsResponse struct {
	BookToken struct {
		Value string `json:"value"`
	} `json:"book_token"`
}

func (r *Resy) Snipe(ctx context.Context, client *http.Client, cfg *config.Config, dryRun bool, onAttempt func(AttemptLog)) (*SnipeResult, error) {
	startFind := time.Now()
	byTime, findErr := r.fetchFind(ctx, client)
	findLatency := time.Since(startFind).Milliseconds()
	if findErr != nil {
		onAttempt(AttemptLog{SlotTime: "*", Status: "find_error", LatencyMS: findLatency, Detail: findErr.Error()})
		if errors.Is(findErr, ErrRateLimited) {
			return nil, findErr
		}
		return nil, fmt.Errorf("no resy slots available")
	}
	onAttempt(AttemptLog{SlotTime: "*", Status: "find_ok", LatencyMS: findLatency, Detail: fmt.Sprintf("%d slots", len(byTime))})

	var (
		bookOnce  sync.Once
		result    *SnipeResult
		resultErr error
		wg        sync.WaitGroup
	)

	for _, slotTime := range cfg.PreferredTimes {
		slotTime := slotTime
		configID := byTime[slotTime]
		if configID == "" {
			onAttempt(AttemptLog{SlotTime: slotTime, Status: "no_slot", LatencyMS: 0})
			continue
		}

		wg.Add(1)
		go func() {
			defer wg.Done()

			start := time.Now()
			token, err := r.getBookToken(ctx, client, configID)
			latency := time.Since(start).Milliseconds()
			if err != nil {
				onAttempt(AttemptLog{SlotTime: slotTime, Status: "details_error", LatencyMS: latency, Detail: err.Error()})
				return
			}
			onAttempt(AttemptLog{SlotTime: slotTime, Status: "details_ok", LatencyMS: latency})

			if dryRun {
				bookOnce.Do(func() {
					result = &SnipeResult{SlotTime: slotTime, ConfirmationBody: []byte(`{"dry_run":true,"book_token":"` + token + `"}`), DryRun: true}
				})
				return
			}

			bookOnce.Do(func() {
				start := time.Now()
				body, err := r.book(ctx, client, token)
				latency := time.Since(start).Milliseconds()
				if err != nil {
					resultErr = err
					onAttempt(AttemptLog{SlotTime: slotTime, Status: "book_error", LatencyMS: latency, Detail: err.Error()})
					return
				}
				onAttempt(AttemptLog{SlotTime: slotTime, Status: "book_ok", LatencyMS: latency})
				result = &SnipeResult{SlotTime: slotTime, ConfirmationBody: body}
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
	return nil, fmt.Errorf("no resy slots available")
}

// fetchFind calls /4/find once and maps HH:MM -> config token for all returned slots.
func (r *Resy) fetchFind(ctx context.Context, client *http.Client) (map[string]string, error) {
	slots, _, err := r.FindSlots(ctx, client)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string)
	for _, slot := range slots {
		if existing, ok := out[slot.Time]; ok && existing != "" {
			// Keep first token per time; snipe uses preferred_times order.
			continue
		}
		out[slot.Time] = slot.Token
	}
	return out, nil
}

func (r *Resy) getBookToken(ctx context.Context, client *http.Client, configID string) (string, error) {
	payload, err := json.Marshal(map[string]any{
		"config_id":  configID,
		"party_size": r.cfg.PartySize,
		"day":        r.cfg.TargetDate,
	})
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, resyBase+"/3/details", strings.NewReader(string(payload)))
	if err != nil {
		return "", err
	}
	r.setHeaders(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 300 {
		if isRateLimitStatus(resp.StatusCode) {
			return "", fmt.Errorf("%w: details status %d", ErrRateLimited, resp.StatusCode)
		}
		return "", fmt.Errorf("details status %d: %s", resp.StatusCode, truncate(body, 256))
	}

	var parsed resyDetailsResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", err
	}
	if parsed.BookToken.Value == "" {
		return "", fmt.Errorf("empty book_token")
	}
	return parsed.BookToken.Value, nil
}

func (r *Resy) book(ctx context.Context, client *http.Client, bookToken string) ([]byte, error) {
	paymentID, err := r.resolvePaymentMethodID(ctx, client)
	if err != nil {
		return nil, err
	}

	form := url.Values{}
	form.Set("book_token", bookToken)
	form.Set("struct_payment_method", fmt.Sprintf(`{"id":%d}`, paymentID))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, resyBase+"/3/book", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	r.setHeaders(req)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

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
		if isRateLimitStatus(resp.StatusCode) {
			return nil, fmt.Errorf("%w: book status %d", ErrRateLimited, resp.StatusCode)
		}
		return nil, fmt.Errorf("book status %d: %s", resp.StatusCode, string(body))
	}
	return body, nil
}

func (r *Resy) resolvePaymentMethodID(ctx context.Context, client *http.Client) (int, error) {
	if r.cfg.Resy.PaymentMethodID > 0 {
		return r.cfg.Resy.PaymentMethodID, nil
	}

	r.paymentMethodOnce.Do(func() {
		r.paymentMethodID, r.paymentMethodErr = r.fetchDefaultPaymentMethodID(ctx, client)
	})
	return r.paymentMethodID, r.paymentMethodErr
}

func (r *Resy) fetchDefaultPaymentMethodID(ctx context.Context, client *http.Client) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, resyBase+"/2/user", nil)
	if err != nil {
		return 0, err
	}
	r.setHeaders(req)

	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}
	if resp.StatusCode >= 300 {
		return 0, fmt.Errorf("user status %d: %s", resp.StatusCode, truncate(body, 256))
	}

	var parsed resyUserResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0, err
	}
	if parsed.PaymentMethodID > 0 {
		return parsed.PaymentMethodID, nil
	}
	for _, pm := range parsed.PaymentMethods {
		if pm.IsDefault {
			return pm.ID, nil
		}
	}
	if len(parsed.PaymentMethods) > 0 {
		return parsed.PaymentMethods[0].ID, nil
	}
	return 0, fmt.Errorf("no payment method on resy account")
}

func (r *Resy) setHeaders(req *http.Request) {
	req.Header.Set("Authorization", fmt.Sprintf(`ResyAPI api_key="%s"`, r.cfg.Resy.APIKey))
	req.Header.Set("X-Resy-Auth-Token", r.cfg.Resy.AuthToken)
	req.Header.Set("Origin", "https://resy.com")
	req.Header.Set("Referer", "https://resy.com/")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
}

func extractTime(iso string) string {
	// Resy returns e.g. "2026-06-11 19:00:00" or ISO variants
	if len(iso) >= 16 {
		for _, layout := range []string{"2006-01-02 15:04:05", time.RFC3339} {
			if t, err := time.Parse(layout, iso); err == nil {
				return t.Format("15:04")
			}
		}
	}
	if idx := strings.Index(iso, " "); idx >= 0 && len(iso) >= idx+6 {
		part := iso[idx+1:]
		if len(part) >= 5 {
			return part[:5]
		}
	}
	return ""
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
