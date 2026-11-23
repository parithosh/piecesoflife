package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateSessionSecretLength(t *testing.T) {
	base := func(dev bool, secret string) *Config {
		return &Config{
			Port:          8080,
			BaseURL:       "https://example.com",
			DevMode:       dev,
			DatabasePath:  "/data/db/app.db",
			UploadPath:    "/data/uploads",
			EmailProvider: "smtp",
			FromEmail:     "loop@example.com",
			SMTPHost:      "smtp.example.com",
			SMTPPort:      465,
			SMTPUser:      "user",
			SMTPPass:      "pass",
			AdminEmail:    "admin@example.com",
			SessionSecret: secret,
		}
	}

	err := base(false, "short-secret").validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "SESSION_SECRET")

	err = base(false, "0123456789abcdef0123456789abcdef").validate()
	assert.NoError(t, err)

	err = base(true, "dev-secret").validate()
	assert.NoError(t, err)
}
