package cmd

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"github.com/spore-host/libs/update"
)

// Build metadata, set via ldflags by GoReleaser.
var (
	GitCommit = "unknown"
	BuildDate = "unknown"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version information",
	Long:  `Display version, build date, and git commit information for puff.`,
	Args:  usageArgs(cobra.NoArgs),
	RunE: func(cmd *cobra.Command, args []string) error {
		out := cmd.OutOrStdout()
		if jsonOutput {
			return writeVersionJSON(out)
		}
		_, _ = fmt.Fprintf(out, "puff - Amazon Braket job, ledger and provenance CLI\n\n")
		_, _ = fmt.Fprintf(out, "Version:    %s\n", Version)
		_, _ = fmt.Fprintf(out, "Git Commit: %s\n", GitCommit)
		_, _ = fmt.Fprintf(out, "Build Date: %s\n", BuildDate)
		_, _ = fmt.Fprintf(out, "\nProject:    https://spore.host\n")

		// Explicit, user-initiated check: report whether a newer release exists.
		_, _ = fmt.Fprint(cmd.ErrOrStderr(), renderUpdateNotice(update.CheckNow("puffball", Version)))
		return nil
	},
}

// versionInfo is the --json shape of `puff version`.
type versionInfo struct {
	Version   string `json:"version"`
	GitCommit string `json:"git_commit"`
	BuildDate string `json:"build_date"`
}

func writeVersionJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(versionInfo{Version: Version, GitCommit: GitCommit, BuildDate: BuildDate})
}

// renderUpdateNotice formats the on-demand update check for the version command.
// Pure, so it's unit-tested without a network call.
func renderUpdateNotice(res *update.Result) string {
	switch {
	case res == nil:
		return "\n(couldn't check for updates)\n"
	case res.HasUpdate():
		return fmt.Sprintf("\nA newer version is available: %s -> %s\n    %s\n",
			res.CurrentVersion, res.LatestVersion, res.UpdateURL)
	default:
		return "\nYou're on the latest version.\n"
	}
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
