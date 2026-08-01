package config

import (
	"os"
	"time"
)

// Config holds all runtime configuration loaded from environment variables.
// No hardcoded app names or domains — everything comes from env.
type Config struct {
	DatabaseURL      string
	RedisURL         string
	Port             string
	AppName          string
	AppDomain        string
	SupabaseURL      string // https://<project>.supabase.co
	SupabaseJWKSURL  string // JWKS endpoint for verifying Supabase-issued JWTs
	SupabaseService  string // service role / secret key — admin actions only
	AnthropicAPIKey  string // required for AI search enrichment and flow interpretation
	VoyageAPIKey     string // required for reach embeddings and /ask endpoint
	USGSAPIKey       string // optional, raises rate limits
	DWRAPIKeys       string // optional comma-separated; raises rate limits (1000 req/day per key without)
	USGSPollInterval string
	DWRPollInterval  string
	MigrationsPath   string
	ResendAPIKey     string // #246 A4: unset -> invite emails degrade to mail.NoopMailer (logs instead of sending)
	MailFrom         string // #246 A4: RFC 5322 From header, e.g. "H2OFlows <trips@h2oflows.app>" — must be set together with ResendAPIKey
	WebBaseURL       string // #246: web origin embedded in invite emails/.ics links — staging must point at its own web deploy, not prod
	// RSVPInboundSecret gates POST /api/v1/hooks/rsvp-inbound (X-RSVP-Secret
	// header, compared constant-time) — the Cloudflare Email Worker relaying
	// inbound METHOD:REPLY mail for invites@h2oflows.app
	// (infra/cloudflare/rsvp-inbound-worker.js) is configured with the same
	// value. Unset -> the handler refuses ALL requests (503) rather than
	// running with no gate at all; see handlers.NewRSVPInboundHandler.
	RSVPInboundSecret string
}

func Load() Config {
	return Config{
		DatabaseURL:       mustEnv("DATABASE_URL"),
		RedisURL:          env("REDIS_URL", "redis://localhost:6379"),
		Port:              env("APP_PORT", "8080"),
		AppName:           env("APP_NAME", "H2OFlows"),
		AppDomain:         env("APP_DOMAIN", "localhost"),
		SupabaseURL:       env("SUPABASE_URL", ""),
		SupabaseJWKSURL:   env("SUPABASE_JWKS_URL", ""),
		SupabaseService:   env("SUPABASE_SERVICE_KEY", ""),
		AnthropicAPIKey:   env("ANTHROPIC_API_KEY", ""),
		VoyageAPIKey:      env("VOYAGE_API_KEY", ""),
		USGSAPIKey:        env("USGS_API_KEY", ""),
		DWRAPIKeys:        env("DWR_API_KEY", ""),
		USGSPollInterval:  env("USGS_POLL_INTERVAL", "15m"),
		DWRPollInterval:   env("DWR_POLL_INTERVAL", "15m"),
		MigrationsPath:    env("MIGRATIONS_PATH", "migrations"),
		ResendAPIKey:      env("RESEND_API_KEY", ""),
		MailFrom:          env("MAIL_FROM", ""),
		WebBaseURL:        env("WEB_BASE_URL", "https://h2oflows.app"),
		RSVPInboundSecret: env("RSVP_INBOUND_SECRET", ""),
	}
}

// PollIntervals holds parsed durations for each gauge source.
type PollIntervals struct {
	USGS time.Duration
	DWR  time.Duration
}

// ParsePollInterval parses the string interval fields into durations.
// Falls back to 15 minutes if a value is missing or unparseable.
func (c Config) ParsePollInterval() PollIntervals {
	parse := func(s string) time.Duration {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			return d
		}
		return 15 * time.Minute
	}
	return PollIntervals{
		USGS: parse(c.USGSPollInterval),
		DWR:  parse(c.DWRPollInterval),
	}
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		panic("required env var not set: " + key)
	}
	return v
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
