package coordinator

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsAlreadyConcluded(t *testing.T) {
	tests := []struct {
		msg  string
		want bool
	}{
		{"channel already concluded", true},
		{"execution reverted: already registered", true},
		{"concluded on-chain", true},
		{"tx already submitted", true},
		{"something else failed", false},
		{"transaction reverted", false},
		{"", false},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, isAlreadyConcluded(errors.New(tt.msg)), "err=%q", tt.msg)
	}
}
