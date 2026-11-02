package cmd

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	braketsdk "github.com/aws/aws-sdk-go-v2/service/braket"
	"github.com/spf13/cobra"

	"github.com/spore-host/puffball/internal/scrub"
	pbraket "github.com/spore-host/puffball/pkg/braket"
	"github.com/spore-host/puffball/pkg/price"
	"github.com/spore-host/puffball/pkg/record"
	"github.com/spore-host/puffball/pkg/results"
)

var recordFlags struct {
	out   string
	force bool
}

var recordCmd = &cobra.Command{
	Use:   "record <task-id|task-arn>",
	Short: "Write the provenance record (puffball.record/v0) for a task's result",
	Long: `Write the provenance record for a quantum task's result, as
puffball.record/v0 JSON. It holds:
  - the device, program, shots and counts;
  - the physical qubits and compiled program;
  - the device's calibration for those qubits at submission and at completion;
  - the estimated cost (prices as of the table's date), software versions and tags;
  - the key, ETag and SHA-256 of every S3 object it read.
Anything it couldn't fill is named in missing[] with the reason, and listed on
stderr.

The calibration at submission is the snapshot puff run stores beside the
results before it submits. A task puff didn't submit has none. The
calibration at completion is read from the device when the record is made.
It counts only if every calibration timestamp for the task's qubits predates
the task's end; otherwise it is kept as at_record and at_completion is named
missing.

Reads only: GetQuantumTask, GetDevice, S3 GetObject. Account IDs appear as
<account>.`,
	Example: `  puff record 617b7e2b-f77f-433b-8226-6cc55eb8d077 --out bell.record.json
  puff record verify bell.record.json`,
	Args: usageArgs(cobra.ExactArgs(1)),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := ctxOf(cmd)
		tb, err := price.Load()
		if err != nil {
			return err
		}
		g, err := resolveTask(ctx, args[0])
		if err != nil {
			return err
		}
		f, err := newFactory(ctx)
		if err != nil {
			return err
		}
		region := pbraket.RegionOf(aws.ToString(g.QuantumTaskArn))
		reader, bucket := newReader(), aws.ToString(g.OutputS3Bucket)
		var acct string
		r, err := record.Build(ctx, record.Inputs{
			Task: g, Prices: tb, PuffVersion: Version, Now: clockNow,
			Get: func(ctx context.Context, key string) (results.Object, error) {
				if acct == "" {
					a, err := newResults().Account(ctx)
					if err != nil {
						return results.Object{}, fmt.Errorf("account: %w", err)
					}
					acct = a
				}
				return reader.Read(ctx, region, bucket, key, acct, record.ResultsLimit)
			},
			Device: func(ctx context.Context) (string, error) {
				api, err := f.Braket(ctx, region)
				if err != nil {
					return "", err
				}
				d, err := api.GetDevice(ctx, &braketsdk.GetDeviceInput{DeviceArn: g.DeviceArn})
				if err != nil {
					return "", fmt.Errorf("GetDevice: %w", err)
				}
				return aws.ToString(d.DeviceCapabilities), nil
			},
		})
		if err != nil {
			return err
		}
		b, err := record.Encode(r)
		if err != nil {
			return err
		}
		if p := record.Verify(b); len(p) > 0 {
			// A record puff can't verify is a bug, not a result.
			return fmt.Errorf("the record doesn't verify: %s", strings.Join(p, "; "))
		}
		if err := writeRecord(cmd, b); err != nil {
			return err
		}
		w := cmd.ErrOrStderr()
		_, _ = fmt.Fprintf(w, "record for task %s on %s: %d S3 objects linked", r.Task.ID, r.Device.Name, len(r.Objects))
		if len(r.Missing) == 0 {
			_, _ = fmt.Fprintln(w, ", nothing missing")
			return nil
		}
		_, _ = fmt.Fprintf(w, ", %d field(s) missing:\n", len(r.Missing))
		for _, m := range r.Missing {
			_, _ = fmt.Fprintf(w, "  %s: %s\n", m.Field, display(m.Reason))
		}
		return nil
	},
}

func writeRecord(cmd *cobra.Command, b []byte) error {
	if recordFlags.out == "" || recordFlags.out == "-" {
		_, err := cmd.OutOrStdout().Write(b)
		return err
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if recordFlags.force {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	f, err := os.OpenFile(recordFlags.out, flags, 0o644)
	if errors.Is(err, fs.ErrExist) {
		return usageErrorf("%s exists; a record isn't overwritten without --force", recordFlags.out)
	}
	if err != nil {
		return fmt.Errorf("write record: %w", err)
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return fmt.Errorf("write record: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write record: %w", err)
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "wrote %s\n", recordFlags.out)
	return nil
}

var recordVerifyCmd = &cobra.Command{
	Use:   "verify <file>",
	Short: "Check a record against the puffball.record/v0 schema and its hashes",
	Long: `Check a record offline: that it conforms to the puffball.record/v0 JSON
Schema, and that the SHA-256 of every program source, compiled program and
calibration subset matches. The S3 objects' hashes link to the objects and
are not re-fetched. Exits 0 if the record checks, 1 with each problem if not.`,
	Args: usageArgs(cobra.ExactArgs(1)),
	RunE: func(cmd *cobra.Command, args []string) error {
		b, err := os.ReadFile(args[0])
		if err != nil {
			return fmt.Errorf("read record: %w", err)
		}
		p := record.Verify(b)
		if jsonOutput {
			if p == nil {
				p = []string{}
			}
			if err := writeJSON(cmd.OutOrStdout(), map[string]any{"file": args[0], "ok": len(p) == 0, "problems": p}); err != nil {
				return err
			}
		}
		if len(p) > 0 {
			return fmt.Errorf("%s doesn't verify:\n  %s", args[0], scrub.Error(errors.New(strings.Join(p, "\n  "))).Error())
		}
		if !jsonOutput {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s: verified (schema %s, every recomputable hash matches)\n", args[0], record.Schema)
		}
		return nil
	},
}

func init() {
	recordCmd.Flags().StringVarP(&recordFlags.out, "out", "o", "", "Write the record to this file instead of stdout")
	recordCmd.Flags().BoolVar(&recordFlags.force, "force", false, "Overwrite --out if it exists")
	recordCmd.AddCommand(recordVerifyCmd)
	rootCmd.AddCommand(recordCmd)
}
