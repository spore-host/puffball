// Command probe runs the G0 API probes (#2–#6) and the post-session sweep check.
//
// It is repo tooling, not part of puff: it talks to the Braket, S3, Price List
// and Cost Explorer APIs directly and saves the raw HTTP response bodies, so
// every field puffball later relies on can be quoted from a live response
// (law 9). Raw output carries account IDs; write it to the gitignored .probe/
// and let scripts/scrub-fixtures copy what belongs in testdata/.
//
//	probe devices     --out DIR     G0(a): SearchDevices + GetDevice, every region (read-only)
//	probe pricing     --out DIR     G0(b): Price List API for AmazonBraket (read-only)
//	probe tasks --real --out DIR    G0(c): SV1 + Cepheus tasks and a program set (billable)
//	probe costs --real --out DIR    G0(e): Cost Explorer for Braket (billable, $0.01/request)
//	probe sweep-check               no queued/running puffball-tagged tasks, no spending limits
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// usage is printed for a missing or unknown subcommand.
const usage = `usage: probe <devices|pricing|tasks|costs|sweep-check> [flags]
run 'probe <subcommand> -h' for its flags`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	ctx := context.Background()
	var err error
	args := os.Args[2:]
	switch os.Args[1] {
	case "devices":
		err = runDevices(ctx, args)
	case "pricing":
		err = runPricing(ctx, args)
	case "tasks":
		err = runTasks(ctx, args)
	case "costs":
		err = runCosts(ctx, args)
	case "sweep-check":
		err = runSweepCheck(ctx, args)
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "probe:", scrubbed(err))
		var ue usageErr
		if errors.As(err, &ue) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

// usageErr marks an invocation error (exit 2).
type usageErr struct{ msg string }

func (e usageErr) Error() string { return e.msg }
