package server

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsValidHexColor(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"#2d5016", true},
		{"#FFF", true},
		{"#abcdef", true},
		{"#AABBCC", true},

		// Invalid
		{"2d5016", false},   // missing #
		{"#2d501", false},   // 5 chars
		{"#2d50166", false}, // 7 chars
		{"#2d501g", false},  // non-hex digit
		{"", false},
		{"#", false},
		{"#GGG", false},
		{"#2d5016 ", false}, // trailing whitespace not tolerated
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			assert.Equal(t, tt.want, isValidHexColor(tt.in))
		})
	}
}
