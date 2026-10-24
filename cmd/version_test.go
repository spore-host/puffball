package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/spore-host/libs/update"
)

func TestRenderUpdateNotice(t *testing.T) {
	cases := []struct {
		name     string
		res      *update.Result
		contains string
	}{
		{"nil → couldn't check", nil, "couldn't check"},
		{
			"newer available → upgrade line",
			&update.Result{CurrentVersion: "0.1.0", LatestVersion: "0.2.0", UpdateURL: "https://example.test/v0.2.0"},
			"A newer version is available: 0.1.0 -> 0.2.0",
		},
		{
			"on latest → reassurance",
			&update.Result{CurrentVersion: "0.1.0", LatestVersion: "0.1.0"},
			"latest version",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := renderUpdateNotice(tc.res)
			if !strings.Contains(got, tc.contains) {
				t.Errorf("renderUpdateNotice() = %q, want it to contain %q", got, tc.contains)
			}
		})
	}
}

func TestWriteVersionJSON(t *testing.T) {
	var buf bytes.Buffer
	if err := writeVersionJSON(&buf); err != nil {
		t.Fatalf("writeVersionJSON: %v", err)
	}
	var got versionInfo
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, buf.String())
	}
	if got.Version != Version || got.GitCommit != GitCommit || got.BuildDate != BuildDate {
		t.Errorf("got %+v, want the package's build variables", got)
	}
}

// TestVersionJSONCommand runs `puff version --json` end to end. The --json path
// makes no network call (the update check is on the human path only), so stdout
// must be exactly one JSON document.
func TestVersionJSONCommand(t *testing.T) {
	t.Cleanup(func() { jsonOutput = false })
	var stdout, stderr bytes.Buffer
	rootCmd.SetOut(&stdout)
	rootCmd.SetErr(&stderr)
	t.Cleanup(func() { rootCmd.SetOut(nil); rootCmd.SetErr(nil) })

	if code := run(rootCmd, []string{"version", "--json"}, &stderr); code != exitOK {
		t.Fatalf("exit code %d, stderr: %s", code, stderr.String())
	}
	var got versionInfo
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
}
