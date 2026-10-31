package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/braket"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// runIdempotency measures whether CreateQuantumTask is idempotent on
// ClientToken (#35): pkg/gate's no-double-spend guarantee rests on it, and G0
// never tested it. Three SV1 calls with one token: the same input twice, then
// different shots. Billable; refuses without --real and refuses to run twice.
func runIdempotency(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("idempotency", flag.ContinueOnError)
	out := flags.String("out", ".probe/idempotency", "output directory (raw, unscrubbed)")
	real := flags.Bool("real", false, "actually submit (billable)")
	region := flags.String("region", "us-west-1", "region to submit SV1 tasks in")
	if err := flags.Parse(args); err != nil {
		return usageErr{err.Error()}
	}
	resultPath := filepath.Join(*out, "result.json")
	if _, err := os.Stat(resultPath); err == nil {
		return fmt.Errorf("%s exists: the idempotency probe already ran; delete it to spend again", resultPath)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	est := simPerMinuteUSD * simMinSeconds / 60
	fmt.Fprintf(os.Stderr, "idempotency probe, SV1 in %s (prices as of %s):\n", *region, pricesAsOf)
	fmt.Fprintf(os.Stderr, "  each SV1 task: estimate %d s minimum x $%g/min = %s\n", simMinSeconds, simPerMinuteUSD, money(est))
	fmt.Fprintf(os.Stderr, "  3 calls, 1 client token: estimate %s if idempotent, at most %s if not\n", money(est), money(3*est))
	if !*real {
		fmt.Fprintln(os.Stderr, "Not submitted. Pass --real to spend this.")
		return nil
	}
	cfg, rec, err := loadConfig(ctx, *region)
	if err != nil {
		return err
	}
	ident, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil || aws.ToString(ident.Account) == "" {
		return fmt.Errorf("GetCallerIdentity: %v", err)
	}
	bucket := fmt.Sprintf("amazon-braket-%s-%s", *region, aws.ToString(ident.Account))
	if _, err := s3.NewFromConfig(cfg).HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)}); err != nil {
		return fmt.Errorf("results bucket amazon-braket-%s-<account> is not there; this probe does not create it", *region)
	}
	c := braket.NewFromConfig(cfg)
	tok := randHex(16)
	action := mustAction(bellQASM)
	call := func(label string, shots int64) map[string]string {
		rec.Reset()
		fmt.Fprintf(os.Stderr, "submitting %s: estimate %s (prices as of %s)\n", label, money(est), pricesAsOf)
		resp, err := c.CreateQuantumTask(ctx, &braket.CreateQuantumTaskInput{
			Action:            aws.String(action),
			ClientToken:       aws.String(tok),
			DeviceArn:         aws.String("arn:aws:braket:::device/quantum-simulator/amazon/sv1"),
			OutputS3Bucket:    aws.String(bucket),
			OutputS3KeyPrefix: aws.String("puffball/idempotency-" + tok[:8]),
			Shots:             aws.Int64(shots),
			Tags:              map[string]string{"puffball:project": "puffball-g1", "puffball:rung": "sv1", "puffball:run-id": "idem-" + tok[:8], "puffball:est-usd": "0.00375"},
		})
		r := map[string]string{"label": label, "shots": fmt.Sprint(shots)}
		if err != nil {
			r["error"] = scrubbed(err)
		} else {
			r["task_arn"] = aws.ToString(resp.QuantumTaskArn)
		}
		_ = writeRaw(filepath.Join(*out, label+".json"), rec.Last())
		fmt.Fprintf(os.Stderr, "  %s: %s\n", label, string(scrubbedJSON(r)))
		return r
	}
	results := []map[string]string{call("first", 100), call("same-token-same-input", 100), call("same-token-other-shots", 200)}
	return writeJSON(resultPath, results)
}

func mustAction(src string) string {
	b, _ := json.Marshal(program(src))
	return string(b)
}

func scrubbedJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return []byte(scrubbed(errors.New(string(b))))
}
