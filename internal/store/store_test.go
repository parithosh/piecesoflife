package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestExtractVersion(t *testing.T) {
	tests := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{"001_initial.sql", 1, false},
		{"002_theming_auto_create.sql", 2, false},
		{"42_whatever.sql", 42, false},

		// Invalid: first segment isn't numeric, Atoi fails.
		{"no_underscore", 0, true},
		{"abc_one.sql", 0, true},
		// No underscore at all — hits the "invalid migration filename" path.
		{"single.sql", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := extractVersion(tt.in)
			if tt.wantErr {
				assert.Error(t, err, "expected error for %q", tt.in)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
