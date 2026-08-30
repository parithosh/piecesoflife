// Package config provides application configuration loading from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
)

// Config holds all application configuration values.
type Config struct {
	// Server
	Port    int
	BaseURL string
	DevMode bool

	// Database
	DatabasePath string

	// Uploads
	UploadPath string

	// Email delivery. EmailProvider selects the transport: "smtp" (default)
	// or "jmap" (e.g. Fastmail with an API token). FromName is an optional
	// display name shown next to FromEmail in recipients' inboxes.
	EmailProvider string
	FromEmail     string
	FromName      string

	// SMTP (required when EmailProvider == "smtp")
	SMTPHost string
	SMTPPort int
	SMTPUser string
	SMTPPass string

	// JMAP (required when EmailProvider == "jmap")
	// JMAPSessionURL defaults to Fastmail's session endpoint; JMAPAPIToken
	// is a bearer token minted in the provider's settings.
	JMAPSessionURL string
	JMAPAPIToken   string

	// Auth
	AdminEmail string
	// SessionSecret is used solely to HMAC-sign the random CSRF cookie.
	// Session tokens themselves are cryptographically random and stored as
	// SHA-256 hashes — they don't use this secret. The name comes from the
	// docker-compose env var (SESSION_SECRET).
	SessionSecret string

	// Logging
	LogLevel  string
	LogFormat string
}

// Load reads configuration from environment variables and returns a validated Config.
func Load() (*Config, error) {
	cfg := &Config{
		Port:      getEnvInt("PORT", 8080),
		BaseURL:   os.Getenv("BASE_URL"),
		DevMode:   os.Getenv("DEV_MODE") == "true",
		LogLevel:  getEnvDefault("LOG_LEVEL", "info"),
		LogFormat: getEnvDefault("LOG_FORMAT", "json"),

		DatabasePath: os.Getenv("DATABASE_PATH"),
		UploadPath:   os.Getenv("UPLOAD_PATH"),

		EmailProvider: getEnvDefault("EMAIL_PROVIDER", "smtp"),
		FromEmail:     os.Getenv("FROM_EMAIL"),
		FromName:      os.Getenv("FROM_NAME"),

		SMTPHost: os.Getenv("SMTP_HOST"),
		SMTPPort: getEnvInt("SMTP_PORT", 465),
		SMTPUser: os.Getenv("SMTP_USER"),
		SMTPPass: os.Getenv("SMTP_PASS"),

		JMAPSessionURL: getEnvDefault("JMAP_SESSION_URL", "https://api.fastmail.com/jmap/session"),
		JMAPAPIToken:   os.Getenv("JMAP_API_TOKEN"),

		AdminEmail:    os.Getenv("ADMIN_EMAIL"),
		SessionSecret: os.Getenv("SESSION_SECRET"),
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

func (c *Config) validate() error {
	required := map[string]string{
		"DATABASE_PATH":  c.DatabasePath,
		"UPLOAD_PATH":    c.UploadPath,
		"FROM_EMAIL":     c.FromEmail,
		"BASE_URL":       c.BaseURL,
		"ADMIN_EMAIL":    c.AdminEmail,
		"SESSION_SECRET": c.SessionSecret,
	}

	switch c.EmailProvider {
	case "smtp":
		required["SMTP_HOST"] = c.SMTPHost
		required["SMTP_USER"] = c.SMTPUser
		required["SMTP_PASS"] = c.SMTPPass
	case "jmap":
		required["JMAP_API_TOKEN"] = c.JMAPAPIToken
		required["JMAP_SESSION_URL"] = c.JMAPSessionURL
	default:
		return fmt.Errorf("EMAIL_PROVIDER must be \"smtp\" or \"jmap\", got %q", c.EmailProvider)
	}

	for name, value := range required {
		if value == "" {
			return fmt.Errorf("required environment variable %s is not set", name)
		}
	}

	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("PORT must be between 1 and 65535")
	}

	if c.SMTPPort < 1 || c.SMTPPort > 65535 {
		return fmt.Errorf("SMTP_PORT must be between 1 and 65535")
	}

	return nil
}

func getEnvDefault(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return defaultVal
}

func getEnvInt(key string, defaultVal int) int {
	v := os.Getenv(key)
	if v == "" {
		return defaultVal
	}

	n, err := strconv.Atoi(v)
	if err != nil {
		return defaultVal
	}

	return n
}
