package cmd

import (
	"context"
	"errors"
	"os"
	"testing"

	pbraket "github.com/spore-host/puffball/pkg/braket"
)

// TestMain makes the default AWS factory fail in tests, so a test that runs a
// read-only command without pointing newFactory at fixtures fails instead of
// calling AWS.
func TestMain(m *testing.M) {
	newFactory = func(context.Context) (pbraket.Factory, error) {
		return nil, errors.New("test called AWS: point newFactory at fixtures (g0Factory)")
	}
	os.Exit(m.Run())
}
