package gate

import (
	"context"
	"testing"
)

func TestAWSCreatorsBuildsWithoutNetwork(t *testing.T) {
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("SPORE_PROFILE", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("AWS_CONFIG_FILE", "/dev/null")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/dev/null")
	f := awsCreators()
	a, err := f(context.Background(), "us-west-1")
	if err != nil || a == nil {
		t.Fatal(a, err)
	}
	if b, _ := f(context.Background(), "us-west-1"); b != a {
		t.Error("creator not cached per region")
	}
}

// TestNewDefaultsToAWSCreators: with no Creators, the gate uses the real
// clients; it is the only way to obtain them.
func TestNewDefaultsToAWSCreators(t *testing.T) {
	g, err := New(Options{Out: testWriter{}, Tokens: &MemTokens{}})
	if err != nil || g.o.Creators == nil {
		t.Fatal(g, err)
	}
}

type testWriter struct{}

func (testWriter) Write(p []byte) (int, error) { return len(p), nil }
