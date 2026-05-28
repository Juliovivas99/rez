package core

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"reservation-bot/config"
)

// Notifier sends Twilio SMS notifications.
type Notifier struct {
	cfg    config.TwilioConfig
	client *http.Client
}

func NewNotifier(cfg config.TwilioConfig, client *http.Client) *Notifier {
	return &Notifier{cfg: cfg, client: client}
}

func (n *Notifier) SendSMS(body string) error {
	if n.cfg.AccountSID == "" || n.cfg.AuthToken == "" || n.cfg.From == "" || n.cfg.To == "" {
		return fmt.Errorf("twilio credentials incomplete")
	}

	endpoint := fmt.Sprintf(
		"https://api.twilio.com/2010-04-01/Accounts/%s/Messages.json",
		n.cfg.AccountSID,
	)

	form := url.Values{}
	form.Set("From", n.cfg.From)
	form.Set("To", n.cfg.To)
	form.Set("Body", body)

	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.SetBasicAuth(n.cfg.AccountSID, n.cfg.AuthToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("twilio status %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

// ConfirmationSMS returns the Twilio message body for a successful booking.
func ConfirmationSMS(restaurantName, targetDate, slotTime string) string {
	return fmt.Sprintf(
		"✅ %s reservation confirmed for %s at %s. Check confirmation.json for details.",
		restaurantName, targetDate, slotTime,
	)
}

// WriteConfirmation persists the booking confirmation to disk.
func WriteConfirmation(path string, restaurantName, platform, targetDate, slotTime string, body []byte) error {
	payload := map[string]interface{}{
		"restaurant_name": restaurantName,
		"platform":        platform,
		"target_date":     targetDate,
		"slot_time":       slotTime,
		"booked_at":       time.Now().Format(time.RFC3339Nano),
		"response":        json.RawMessage(body),
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}
