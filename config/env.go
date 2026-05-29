package config

import (
	"os"
	"strconv"

	"github.com/joho/godotenv"
)

// loadDotEnv reads .env from the current working directory (ignored if missing).
func loadDotEnv() {
	_ = godotenv.Load(".env")
}

func (c *Config) applyEnvSecrets() {
	if v := os.Getenv("RESY_API_KEY"); v != "" {
		c.Resy.APIKey = v
	}
	if v := os.Getenv("RESY_AUTH_TOKEN"); v != "" {
		c.Resy.AuthToken = v
	}
	if v := os.Getenv("RESY_PAYMENT_METHOD_ID"); v != "" {
		id, err := strconv.Atoi(v)
		if err != nil || id <= 0 {
			return
		}
		c.Resy.PaymentMethodID = id
	}
}
