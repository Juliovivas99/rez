package platforms

import (
	"context"
	"fmt"
	"net/http"

	"reservation-bot/config"
)

// AttemptLog captures one snipe attempt for structured logging.
type AttemptLog struct {
	SlotTime  string
	Status    string
	LatencyMS int64
	Detail    string
}

// SnipeResult is returned when a reservation is secured or dry-run succeeds.
type SnipeResult struct {
	SlotTime         string
	ConfirmationBody []byte
	DryRun           bool
}

// Platform runs the reservation snipe flow for a provider.
type Platform interface {
	Name() string
	// ClockProbeURL is hit with HEAD to read Date headers for server clock sync.
	ClockProbeURL() string
	ConfigureClockProbe(req *http.Request)
	Snipe(ctx context.Context, client *http.Client, cfg *config.Config, dryRun bool, onAttempt func(AttemptLog)) (*SnipeResult, error)
}

// New returns the platform implementation for cfg.Platform.
func New(cfg *config.Config) (Platform, error) {
	switch cfg.Platform {
	case "resy":
		return &Resy{cfg: cfg}, nil
	case "sevenrooms":
		return &SevenRooms{cfg: cfg}, nil
	default:
		return nil, fmt.Errorf("unknown platform %q", cfg.Platform)
	}
}
