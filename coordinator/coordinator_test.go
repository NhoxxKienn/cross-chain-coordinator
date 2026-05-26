package coordinator

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"perun.network/go-perun/channel"
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

// TestIsAlreadyConcluded_TypedSentinel exercises the typed-error path:
// errors.Is must match channel.ErrChannelAlreadyConcluded even when wrapped.
func TestIsAlreadyConcluded_TypedSentinel(t *testing.T) {
	assert.True(t, isAlreadyConcluded(channel.ErrChannelAlreadyConcluded))
	assert.True(t, isAlreadyConcluded(fmt.Errorf("dispatch: %w", channel.ErrChannelAlreadyConcluded)))
	assert.False(t, isAlreadyConcluded(nil))
}
