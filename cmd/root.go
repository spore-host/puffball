// Package cmd holds puff's cobra commands.
package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/spore-host/libs/i18n"
	"github.com/spore-host/puffball/pkg/awscfg"
)

var (
	// Version is set via ldflags at build time.
	Version = "dev"

	// Global flags.
	jsonOutput bool
	verbose    bool

	// Shared spore.host config flags (see libs/sporeconfig). The profile and
	// region go to pkg/awscfg.
	sharedProfile string
	sharedRegion  string

	// i18n flags. Command text is English for now: the translation keys live in
	// libs/i18n and puffball has none there yet.
	flagLang          string
	flagNoEmoji       bool
	flagAccessibility bool
)

// Exit codes, per the spore.host CLI standard.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// usageError marks an error caused by how the command was invoked, so
// [Execute] exits 2 rather than 1.
type usageError struct{ err error }

func (e usageError) Error() string { return e.err.Error() }
func (e usageError) Unwrap() error { return e.err }

// usageErrorf returns a usage error (exit code 2).
func usageErrorf(format string, a ...any) error {
	return usageError{fmt.Errorf(format, a...)}
}

var rootCmd = &cobra.Command{
	Use:   "puff",
	Short: "Amazon Braket from the command line: devices, prices, runs, ledger, records",
	Long: `puff is the spore.host tool for Amazon Braket: a job, ledger and provenance
layer. It discovers devices in every Braket region, prices work before it
runs, submits OpenQASM 3 and AHS programs, tracks tasks across regions, keeps
an account-side spend estimate, wraps QPU spending limits, and writes a
provenance record for every hardware result.

Nothing billable happens without --real. The local simulator is the default.`,
}

var i18nInitialized = false

// Execute runs the root command and exits with the spore.host exit codes.
// Ctrl-C and SIGTERM cancel the command's context, which stops anything it
// started (the local simulator runs in its own process group, so the
// terminal's signal doesn't reach it directly).
func Execute() {
	_ = rootCmd.ParseFlags(os.Args[1:])
	ensureI18nInitialized()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	rootCmd.SetContext(ctx)
	code := run(rootCmd, os.Args[1:], os.Stderr)
	stop()
	os.Exit(code)
}

// run executes root with args and returns the process exit code, writing any
// error to stderr.
func run(root *cobra.Command, args []string, stderr io.Writer) int {
	root.SetArgs(args)
	err := root.Execute()
	if err == nil {
		return exitOK
	}
	_, _ = fmt.Fprintln(stderr, "Error:", err)
	var ue usageError
	if errors.As(err, &ue) || isCobraUsageError(err) {
		_, _ = fmt.Fprintf(stderr, "Run '%s --help' for usage.\n", root.CommandPath())
		return exitUsage
	}
	return exitError
}

// isCobraUsageError reports whether err came from cobra's own flag parsing or
// command lookup, which are usage errors. Cobra returns command-lookup failures
// as plain errors, so they are recognised by their fixed prefix.
func isCobraUsageError(err error) bool {
	var fe flagError
	return errors.As(err, &fe) || strings.HasPrefix(err.Error(), "unknown command ")
}

// usageArgs wraps a cobra positional-argument validator so its failures are
// usage errors (exit code 2).
func usageArgs(v cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := v(cmd, args); err != nil {
			return usageError{err}
		}
		return nil
	}
}

// flagError wraps errors from cobra's flag parsing (see SetFlagErrorFunc).
type flagError struct{ err error }

func (e flagError) Error() string { return e.err.Error() }
func (e flagError) Unwrap() error { return e.err }

func init() {
	// Execute prints the error itself and sets the exit code deliberately, so
	// cobra must not print it a second time.
	rootCmd.SilenceErrors = true
	// A runtime error is not a reason to print the usage text; run prints a
	// pointer to --help for usage errors only.
	rootCmd.SilenceUsage = true
	rootCmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return flagError{err}
	})

	rootCmd.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		ensureI18nInitialized()
		// Record shared-config flag values for pkg/awscfg (flag > env > file > default).
		awscfg.SetFlags(sharedProfile, sharedRegion)
		return nil
	}

	rootCmd.PersistentFlags().StringVar(&flagLang, "lang", "", "Language for output (en, es, fr, de, ja, pt)")
	rootCmd.PersistentFlags().BoolVar(&flagNoEmoji, "no-emoji", false, "Disable emoji in output")
	rootCmd.PersistentFlags().BoolVar(&flagAccessibility, "accessibility", false, "Enable accessibility mode (implies --no-emoji)")

	// Shared spore.host config: AWS profile and default region, resolved
	// flag > env (SPORE_*/AWS_*) > ~/.config/spore/config.toml > default. Braket
	// device regions are discovered, not taken from --region.
	rootCmd.PersistentFlags().StringVar(&sharedProfile, "profile", "", "AWS named profile (overrides SPORE_PROFILE/AWS_PROFILE and the shared config)")
	rootCmd.PersistentFlags().StringVar(&sharedRegion, "region", "", "Region for the first device-discovery call (overrides SPORE_REGION/AWS_REGION and the shared config)")

	rootCmd.PersistentFlags().BoolVar(&jsonOutput, "json", false, "Write JSON to stdout")
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Enable verbose output")
}

func ensureI18nInitialized() {
	if i18nInitialized {
		return
	}
	cfg := i18n.Config{
		Language:          flagLang,
		AccessibilityMode: flagAccessibility,
		NoEmoji:           flagNoEmoji,
	}
	if err := i18n.Init(cfg); err != nil {
		log.Printf("Warning: failed to initialize i18n: %v", err)
	}
	i18nInitialized = true
}
