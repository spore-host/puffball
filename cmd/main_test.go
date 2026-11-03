package cmd

import (
	"context"
	"errors"
	"os"
	"testing"

	pbraket "github.com/spore-host/puffball/pkg/braket"
	"github.com/spore-host/puffball/pkg/gate"
)

// TestMain makes the default AWS factory fail in tests, so a test that runs a
// read-only command without pointing newFactory at fixtures fails instead of
// calling AWS.
func TestMain(m *testing.M) {
	newFactory = func(context.Context) (pbraket.Factory, error) {
		return nil, errors.New("test called AWS: point newFactory at fixtures (g0Factory)")
	}
	newLimiters = func() gate.Limiters {
		return func(context.Context, string) (pbraket.Limiter, error) {
			return nil, errors.New("test built a spending-limit client: point newLimiters at a FakeLimiter")
		}
	}
	os.Exit(m.Run())
}
