package main

import (
	"context"
	"errors"
	"flag"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"reservation-bot/config"
	"reservation-bot/core"
	"reservation-bot/platforms"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to config file")
	dryRun := flag.Bool("dry-run", false, "run timing and availability checks without booking")
	runNow := flag.Bool("now", false, "skip scheduler and snipe immediately (for testing)")
	flag.Parse()

	zerolog.TimeFieldFormat = zerolog.TimeFormatUnixMs
	log.Logger = zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339}).
		With().
		Timestamp().
		Logger()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to load config")
	}

	platform, err := platforms.New(cfg)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to init platform")
	}

	client := newHTTPClient()
	notifier := core.NewNotifier(cfg.Twilio, client)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	scheduler, err := core.SyncNTP()
	if err != nil {
		log.Fatal().Err(err).Msg("ntp sync failed")
	}

	probeURL := platform.ClockProbeURL()
	probeReq := func(req *http.Request) {
		platform.ConfigureClockProbe(req)
	}
	if err := scheduler.SyncServerClock(ctx, client, probeURL, probeReq); err != nil {
		if *dryRun {
			log.Warn().Err(err).Msg("server clock sync failed (dry-run); continuing without server offset")
		} else {
			log.Fatal().Err(err).Msg("reservation server clock sync failed")
		}
	}

	dropAt, err := cfg.DropAt()
	if err != nil {
		log.Fatal().Err(err).Msg("invalid drop time")
	}

	log.Info().
		Str("config", *configPath).
		Str("restaurant", cfg.RestaurantName).
		Str("platform", platform.Name()).
		Str("target_date", cfg.TargetDate).
		Int("drop_days_advance", cfg.DropDaysAdvance).
		Time("drop_at", dropAt).
		Bool("dry_run", *dryRun).
		Bool("now", *runNow).
		Dur("ntp_offset_ms", scheduler.NTPOffset).
		Dur("server_offset_ms", scheduler.ServerOffset).
		Dur("total_offset_ms", scheduler.TotalOffset()).
		Msg("reservation bot starting")

	if !*runNow {
		if err := scheduler.WaitUntil(ctx, dropAt, client, probeURL, probeReq, *dryRun); err != nil {
			log.Fatal().Err(err).Msg("scheduler interrupted")
		}
	} else {
		log.Warn().Msg("scheduler skipped (--now); sniping immediately")
	}

	retryUntil := dropAt.Add(time.Duration(cfg.RetryWindowSeconds) * time.Second)
	if *runNow {
		retryUntil = scheduler.Now().Add(time.Duration(cfg.RetryWindowSeconds) * time.Second)
	}

	rateLimitBackoff := 2 * time.Second
	retryInterval := time.Duration(cfg.RetryIntervalMS) * time.Millisecond

	for {
		if scheduler.Now().After(retryUntil) {
			log.Error().Msg("retry window exhausted")
			os.Exit(1)
		}

		result, err := runSnipe(ctx, platform, client, cfg, *dryRun)
		if err != nil {
			if *dryRun {
				log.Fatal().Err(err).Msg("dry-run snipe failed (no retries in dry-run)")
			}
			if errors.Is(err, platforms.ErrRateLimited) {
				log.Warn().Dur("backoff", rateLimitBackoff).Msg("rate limited; backing off before retry")
				time.Sleep(rateLimitBackoff)
				if rateLimitBackoff < 5*time.Second {
					rateLimitBackoff *= 2
				}
			} else {
				time.Sleep(retryInterval)
			}
			log.Warn().Err(err).Msg("snipe attempt failed, retrying")
			continue
		}

		log.Info().
			Str("restaurant", cfg.RestaurantName).
			Str("platform", platform.Name()).
			Str("slot_time", result.SlotTime).
			Bool("dry_run", result.DryRun).
			RawJSON("confirmation", result.ConfirmationBody).
			Msg("reservation secured")

		if result.DryRun {
			log.Info().Msg("dry-run complete, no booking submitted")
			return
		}

		if err := core.WriteConfirmation(
			"confirmation.json",
			cfg.RestaurantName,
			platform.Name(),
			cfg.TargetDate,
			result.SlotTime,
			result.ConfirmationBody,
		); err != nil {
			log.Error().Err(err).Msg("failed to write confirmation.json")
		}

		smsBody := core.ConfirmationSMS(cfg.RestaurantName, cfg.TargetDate, result.SlotTime)
		if err := notifier.SendSMS(smsBody); err != nil {
			log.Warn().Err(err).Msg("twilio notification failed")
		} else {
			log.Info().Msg("sms notification sent")
		}
		return
	}
}

func runSnipe(ctx context.Context, platform platforms.Platform, client *http.Client, cfg *config.Config, dryRun bool) (*platforms.SnipeResult, error) {
	onAttempt := func(a platforms.AttemptLog) {
		log.Info().
			Str("platform", platform.Name()).
			Str("slot_time", a.SlotTime).
			Str("status", a.Status).
			Int64("latency_ms", a.LatencyMS).
			Str("detail", a.Detail).
			Msg("attempt")
	}
	return platform.Snipe(ctx, client, cfg, dryRun, onAttempt)
}

func newHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConnsPerHost: 20,
			IdleConnTimeout:     90 * time.Second,
			DisableCompression:  false,
			ForceAttemptHTTP2:   true,
		},
	}
}
