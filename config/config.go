package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"
)

type Config struct {
	Platform           string   `mapstructure:"platform"`
	RestaurantName     string   `mapstructure:"restaurant_name"`
	TargetDate         string   `mapstructure:"target_date"`
	PartySize          int      `mapstructure:"party_size"`
	DropTime           string   `mapstructure:"drop_time"`
	DropDaysAdvance    int      `mapstructure:"drop_days_advance"`
	RetryWindowSeconds        int      `mapstructure:"retry_window_seconds"`
	RetryIntervalMS           int      `mapstructure:"retry_interval_ms"`
	PreferredTimes            []string `mapstructure:"preferred_times"`
	PreferredStart            string   `mapstructure:"preferred_start"`
	PreferredEnd              string   `mapstructure:"preferred_end"`
	PreferredIntervalMinutes  int      `mapstructure:"preferred_interval_minutes"`
	Timezone                  string   `mapstructure:"timezone"` // IANA, e.g. America/New_York (EST/EDT)
	Resy                 ResyConfig         `mapstructure:"resy"`
	SevenRooms           SevenRoomsConfig   `mapstructure:"sevenrooms"`
	Guest                GuestConfig        `mapstructure:"guest"`
	Twilio               TwilioConfig       `mapstructure:"twilio"`
}

type ResyConfig struct {
	APIKey    string `mapstructure:"api_key"`
	AuthToken string `mapstructure:"auth_token"`
	VenueID   string `mapstructure:"venue_id"`
}

type SevenRoomsConfig struct {
	VenueID     string            `mapstructure:"venue_id"`
	ClientToken string            `mapstructure:"client_token"`
	Headers     map[string]string `mapstructure:"headers"`
}

type GuestConfig struct {
	FirstName string `mapstructure:"first_name"`
	LastName  string `mapstructure:"last_name"`
	Email     string `mapstructure:"email"`
	Phone     string `mapstructure:"phone"`
}

type TwilioConfig struct {
	AccountSID string `mapstructure:"account_sid"`
	AuthToken  string `mapstructure:"auth_token"`
	From       string `mapstructure:"from"`
	To         string `mapstructure:"to"`
}

func Load(path string) (*Config, error) {
	v := viper.New()
	v.SetConfigFile(path)
	v.SetConfigType("yaml")

	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}

	cfg.resolveTargetDate()
	if err := cfg.expandPreferredTimes(); err != nil {
		return nil, err
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// resolveTargetDate sets TargetDate from drop_days_advance when not explicitly set.
// The dining date is anchored to the calendar day of the next drop (not time.Now), so
// starting the bot the night before still snipes the correct release date.
func (c *Config) resolveTargetDate() {
	if c.TargetDate != "" {
		return
	}
	if c.DropDaysAdvance <= 0 {
		return
	}
	loc := c.Location()
	drop, err := c.DropAt()
	if err != nil {
		c.TargetDate = time.Now().In(loc).AddDate(0, 0, c.DropDaysAdvance).Format("2006-01-02")
		return
	}
	dropDay := time.Date(drop.Year(), drop.Month(), drop.Day(), 0, 0, 0, 0, loc)
	c.TargetDate = dropDay.AddDate(0, 0, c.DropDaysAdvance).Format("2006-01-02")
}

// Location returns the configured timezone, defaulting to America/New_York (EST/EDT).
func (c *Config) Location() *time.Location {
	tz := c.Timezone
	if tz == "" {
		tz = "America/New_York"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return time.Local
	}
	return loc
}

func (c *Config) validate() error {
	switch c.Platform {
	case "resy", "sevenrooms":
	default:
		return fmt.Errorf("platform must be resy or sevenrooms, got %q", c.Platform)
	}
	if c.TargetDate == "" {
		return fmt.Errorf("target_date is required")
	}
	if _, err := time.Parse("2006-01-02", c.TargetDate); err != nil {
		return fmt.Errorf("invalid target_date: %w", err)
	}
	if c.PartySize < 1 {
		return fmt.Errorf("party_size must be >= 1")
	}
	if c.DropTime == "" {
		return fmt.Errorf("drop_time is required")
	}
	if _, err := time.Parse("15:04", c.DropTime); err != nil {
		return fmt.Errorf("invalid drop_time (use HH:MM): %w", err)
	}
	if len(c.PreferredTimes) == 0 {
		return fmt.Errorf("set preferred_times and/or preferred_start + preferred_end")
	}
	for _, t := range c.PreferredTimes {
		if _, err := ParseTimeHM(t); err != nil {
			return fmt.Errorf("invalid preferred time %q: %w", t, err)
		}
	}
	if c.RetryWindowSeconds <= 0 {
		c.RetryWindowSeconds = 10
	}
	if c.RetryIntervalMS <= 0 {
		c.RetryIntervalMS = 300
	}

	switch c.Platform {
	case "resy":
		if c.Resy.APIKey == "" || c.Resy.AuthToken == "" || c.Resy.VenueID == "" {
			return fmt.Errorf("resy.api_key, resy.auth_token, and resy.venue_id are required")
		}
	case "sevenrooms":
		if c.SevenRooms.VenueID == "" {
			return fmt.Errorf("sevenrooms.venue_id is required")
		}
		if c.Guest.FirstName == "" || c.Guest.LastName == "" || c.Guest.Email == "" || c.Guest.Phone == "" {
			return fmt.Errorf("guest fields are required for sevenrooms")
		}
	}
	return nil
}

// DropAt returns the next drop instant in the configured timezone (today or tomorrow at drop_time).
func (c *Config) DropAt() (time.Time, error) {
	loc := c.Location()
	t, err := time.ParseInLocation("15:04", c.DropTime, loc)
	if err != nil {
		return time.Time{}, err
	}
	now := time.Now().In(loc)
	drop := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, loc)
	if !drop.After(now) {
		drop = drop.Add(24 * time.Hour)
	}
	return drop, nil
}

// expandPreferredTimes builds PreferredTimes from a start/end range when configured.
func (c *Config) expandPreferredTimes() error {
	if c.PreferredStart == "" && c.PreferredEnd == "" {
		return nil
	}

	interval := c.PreferredIntervalMinutes
	if interval <= 0 {
		interval = 30
	}

	generated, err := TimesInRange(c.PreferredStart, c.PreferredEnd, interval)
	if err != nil {
		return err
	}

	if len(c.PreferredTimes) == 0 {
		c.PreferredTimes = generated
		return nil
	}

	seen := make(map[string]struct{}, len(c.PreferredTimes)+len(generated))
	merged := make([]string, 0, len(c.PreferredTimes)+len(generated))
	for _, t := range c.PreferredTimes {
		norm, err := ParseTimeHM(t)
		if err != nil {
			continue
		}
		if _, ok := seen[norm]; ok {
			continue
		}
		seen[norm] = struct{}{}
		merged = append(merged, norm)
	}
	for _, t := range generated {
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		merged = append(merged, t)
	}
	c.PreferredTimes = merged
	return nil
}

// ParseTimeHM normalizes a time string to HH:MM (24h).
func ParseTimeHM(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	layouts := []string{"15:04", "3:04 PM", "15:04:05", "3:04:05 PM"}
	for _, layout := range layouts {
		t, err := time.Parse(layout, raw)
		if err == nil {
			return t.Format("15:04"), nil
		}
	}
	return "", fmt.Errorf("use HH:MM or h:MM PM")
}

// TimesInRange returns HH:MM times from start through end every interval minutes.
func TimesInRange(start, end string, intervalMinutes int) ([]string, error) {
	startHM, err := ParseTimeHM(start)
	if err != nil {
		return nil, fmt.Errorf("preferred_start: %w", err)
	}
	endHM, err := ParseTimeHM(end)
	if err != nil {
		return nil, fmt.Errorf("preferred_end: %w", err)
	}
	if intervalMinutes <= 0 {
		return nil, fmt.Errorf("preferred_interval_minutes must be > 0")
	}

	startT, _ := time.Parse("15:04", startHM)
	endT, _ := time.Parse("15:04", endHM)
	if !endT.After(startT) {
		return nil, fmt.Errorf("preferred_end must be after preferred_start")
	}

	var out []string
	for cur := startT; !cur.After(endT); cur = cur.Add(time.Duration(intervalMinutes) * time.Minute) {
		out = append(out, cur.Format("15:04"))
	}
	return out, nil
}
