package cmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	braketsdk "github.com/aws/aws-sdk-go-v2/service/braket"
	"github.com/spf13/cobra"

	"github.com/spore-host/puffball/pkg/awscfg"
	pbraket "github.com/spore-host/puffball/pkg/braket"
	"github.com/spore-host/puffball/pkg/devices"
	"github.com/spore-host/puffball/pkg/gate"
	"github.com/spore-host/puffball/pkg/price"
	"github.com/spore-host/puffball/pkg/results"
)

// newResults is the results-bucket store; tests replace it.
var newResults = func() results.Store { return results.AWS{} }

// Tag keys puff sets on everything it submits (#1). User --tag keys may not
// use the puffball: namespace.
const (
	tagProject    = "puffball:project"
	tagRung       = "puffball:rung"
	tagRunID      = "puffball:run-id"
	tagEstUSD     = "puffball:est-usd"
	tagPricesAsOf = "puffball:prices-as-of"
)

// pinnedTags are derived from the run, not chosen by the user: a retry
// resends them as first sent (see gate.Options.Pinned).
var pinnedTags = []string{tagEstUSD, tagPricesAsOf}

// tagChars is the AWS tag character set (AWS documentation, "Tag naming
// limits and requirements"): letters, numbers, spaces, and _ . : / = + - @.
var tagChars = regexp.MustCompile(`^[\p{L}\p{Z}\p{N}_.:/=+\-@]*$`)

// runIDPattern keeps a run id safe in an S3 prefix and a tag value.
var runIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// checkTag applies the AWS tag rules, so a request the service would reject
// never gets as far as claiming a client token.
func checkTag(k, v string) error {
	switch {
	case k == "":
		return errors.New("empty tag key")
	case utf8.RuneCountInString(k) > 128:
		return fmt.Errorf("tag key %q is over 128 characters", k)
	case utf8.RuneCountInString(v) > 256:
		return fmt.Errorf("tag %q value is over 256 characters", k)
	case strings.HasPrefix(strings.ToLower(k), "aws:"):
		return fmt.Errorf("tag key %q: the aws: prefix is reserved", k)
	case !tagChars.MatchString(k) || !tagChars.MatchString(v):
		return fmt.Errorf("tag %q=%q: AWS tags allow letters, numbers, spaces and _ . : / = + - @", k, v)
	}
	return nil
}

// parseTags turns --tag k=v flags into a map, refusing puffball: keys and
// anything the AWS tag rules reject.
func parseTags(kvs []string) (map[string]string, error) {
	out := map[string]string{}
	for _, kv := range kvs {
		k, v, ok := strings.Cut(kv, "=")
		k = strings.TrimSpace(k)
		switch {
		case !ok || k == "":
			return nil, fmt.Errorf("--tag %q: want key=value", kv)
		case strings.HasPrefix(k, "puffball:"):
			return nil, fmt.Errorf("--tag %q: the puffball: namespace is puff's own", kv)
		}
		if err := checkTag(k, v); err != nil {
			return nil, fmt.Errorf("--tag: %w", err)
		}
		out[k] = v
	}
	return out, nil
}

func newRunID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "run-" + hex.EncodeToString(b)
}

// pickRegion chooses where to submit, and says why: a QPU's own region; for
// a simulator found in several regions, --device-region (want), else the
// configured region if the simulator is ONLINE there, else the first ONLINE
// region in order. failed are regions discovery couldn't search: a choice
// that would depend on one is refused.
func pickRegion(d devices.Device, want, configured string, failed map[string]bool) (string, string, error) {
	online := func(r string) bool {
		if s, ok := d.StatusByRegion[r]; ok {
			return s == "ONLINE"
		}
		return d.Status == "ONLINE"
	}
	if want != "" && failed[want] {
		return "", "", fmt.Errorf("%s couldn't be searched, so whether %s runs there is unknown", want, d.Name)
	}
	if want != "" {
		for _, r := range d.Regions {
			if r == want {
				why := "--device-region"
				if !online(r) {
					why += "; warning: not ONLINE there"
				}
				return r, why, nil
			}
		}
		return "", "", fmt.Errorf("%s is not offered in %s (it is in %s)", d.Name, want, strings.Join(d.Regions, ", "))
	}
	if len(d.Regions) == 1 {
		return d.Regions[0], "the device's region", nil
	}
	if configured != "" && failed[configured] {
		// Falling back to another region here would hide the failure.
		return "", "", fmt.Errorf("the configured region %s couldn't be searched, so whether %s runs there is unknown; pass --device-region", configured, d.Name)
	}
	if configured != "" {
		for _, r := range d.Regions {
			if r == configured && online(r) {
				return r, "the configured region", nil
			}
		}
	}
	for _, r := range d.Regions {
		if online(r) {
			return r, "the first region it is ONLINE in", nil
		}
	}
	return "", "", fmt.Errorf("%s is not ONLINE in any region (%s)", d.Name, strings.Join(d.Regions, ", "))
}

// buildAction reads program and builds the action JSON for the device's
// paradigm: OpenQASM 3 (.qasm) for gate devices, AHS (.json) for analog.
func buildAction(program string, src []byte, d devices.Device) (string, error) {
	ext := strings.ToLower(filepath.Ext(program))
	switch {
	case ext == ".qasm" && d.Paradigm == devices.ParadigmGate:
		b, err := pbraket.OpenQASMAction(pbraket.Program{Source: string(src)})
		return string(b), err
	case ext == ".json" && d.Paradigm == devices.ParadigmAnalog:
		b, err := pbraket.AHSAction(src)
		return string(b), err
	case ext == ".qasm":
		return "", fmt.Errorf("%s runs analog (AHS) programs, not OpenQASM", d.Name)
	case ext == ".json":
		return "", fmt.Errorf("%s runs OpenQASM 3 programs, not AHS", d.Name)
	default:
		return "", fmt.Errorf("%s: want an OpenQASM 3 (.qasm) or AHS (.json) program", program)
	}
}

// braketRun is the --json shape of `puff run --device`.
type braketRun struct {
	Rung          string `json:"rung"`
	Device        string `json:"device"`
	DeviceArn     string `json:"device_arn"`
	Region        string `json:"region"`
	Program       string `json:"program"`
	ProgramSHA256 string `json:"program_sha256"`
	RunID         string `json:"run_id"`
	Submitted     bool   `json:"submitted"`
	AlreadyRan    bool   `json:"already_submitted,omitempty"`
	// TaskArn has the account replaced by <account> (law 7: --json output
	// gets pasted); TaskID is the part later commands need.
	TaskArn       string            `json:"task_arn,omitempty"`
	TaskID        string            `json:"task_id,omitempty"`
	Retried       bool              `json:"retried,omitempty"`
	RegionReason  string            `json:"region_reason"`
	ResultsPrefix string            `json:"results_prefix,omitempty"` // s3://<bucket-with-<account>>/puffball/<run-id>/
	BucketCreated bool              `json:"bucket_created,omitempty"`
	Tags          map[string]string `json:"tags"`
	Estimate      price.Estimate    `json:"estimate"`
	PuffVersion   string            `json:"puff_version"`
}

// runOnBraket submits program to a Braket device through the gate.
func runOnBraket(cmd *cobra.Command, program string, src []byte, sum string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	userTags, err := parseTags(runFlags.tags)
	if err != nil {
		return usageError{err}
	}
	if runFlags.runID != "" && !runIDPattern.MatchString(runFlags.runID) {
		return usageErrorf("--run-id %q: use 1-64 of A-Z a-z 0-9 . _ -", runFlags.runID)
	}
	if runFlags.project == "" {
		return usageErrorf("--project must not be empty")
	}
	if err := checkTag(tagProject, runFlags.project); err != nil {
		return usageErrorf("--project: %v", err)
	}
	tb, err := price.Load()
	if err != nil {
		return err
	}
	aliases := devices.Aliases{}
	for _, d := range tb.Devices {
		aliases[d.Alias] = d.Arn
	}
	f, err := newFactory(ctx)
	if err != nil {
		return err
	}
	l, collectErr := devices.Collect(ctx, f, aliases)
	if l == nil {
		return collectErr
	}
	failed := map[string]bool{}
	if ie, ok := pbraket.AsIncomplete(collectErr); ok {
		for _, r := range ie.Regions() {
			failed[r] = true
		}
		// Named, not dropped (law 3).
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "warning:", ie.Error())
	}
	d, err := l.Resolve(runFlags.device, aliases)
	if err != nil {
		if collectErr != nil {
			return fmt.Errorf("%w (and %v)", err, collectErr)
		}
		return usageError{err}
	}
	if d.Status != "ONLINE" && len(d.StatusByRegion) == 0 {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s is %s; the service decides whether it accepts the task\n", d.Name, d.Status)
	}
	region, why, err := pickRegion(d, runFlags.deviceRegion, awscfg.Resolved().Region, failed)
	if err != nil {
		return usageError{err}
	}
	action, err := buildAction(program, src, d)
	if err != nil {
		return usageError{err}
	}
	pd, err := tb.Lookup(d.Arn)
	if err != nil {
		return usageError{err}
	}
	if pd.Simulator() && cmd.Flags().Changed("budget") && runFlags.duration == 0 {
		return usageErrorf("%s bills its run time, which isn't known before the run; --budget needs --duration as an upper bound", d.Name)
	}
	est, err := tb.Estimate(pd, price.Request{Programs: 1, Shots: runFlags.shots, Duration: runFlags.duration})
	if err != nil {
		return usageError{err}
	}
	runID := runFlags.runID
	if runID == "" {
		runID = newRunID()
	}
	rung := "qpu"
	if pd.Simulator() {
		rung = pd.Alias
	}
	tags := map[string]string{}
	for k, v := range userTags {
		tags[k] = v
	}
	tags[tagProject] = runFlags.project
	tags[tagRung] = rung
	tags[tagRunID] = runID
	tags[tagEstUSD] = strings.TrimPrefix(est.TotalUSD, "$")
	tags[tagPricesAsOf] = est.PricesAsOf

	g, err := openGate(cmd, runFlags.gate)
	if err != nil {
		return err
	}
	key := runID + "/" + pd.Alias
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "%s on %s in %s (%s), run id %s\n", program, d.Name, region, why, runID)
	permit, err := g.Approve([]gate.Estimate{{Key: key, Label: pd.Alias, Arithmetic: est.Arithmetic, USD: est.Total.USD(), AsOf: est.PricesAsOf}})
	if err != nil {
		return err
	}
	out := braketRun{Rung: rung, Device: pd.Alias, DeviceArn: d.Arn, Region: region, RegionReason: why, Program: program, ProgramSHA256: sum,
		RunID: runID, Tags: tags, Estimate: est, PuffVersion: Version}
	if permit == nil { // stopped by design: no --real, or --dry-run
		return writeBraketRun(cmd, out)
	}
	// Everything that can fail cheaply fails before the bucket is touched.
	if err := permit.Check(); err != nil {
		return err
	}
	bucket, created, err := results.Ensure(ctx, newResults(), region, true)
	if err != nil {
		return err
	}
	if created {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "created the results bucket %s (it did not exist; --real run)\n", results.Display(region))
	}
	out.BucketCreated = created
	prefix := results.Prefix + runID
	s, err := permit.Submit(ctx, region, key, braketsdk.CreateQuantumTaskInput{
		Action:            aws.String(action),
		DeviceArn:         aws.String(d.Arn),
		OutputS3Bucket:    aws.String(bucket),
		OutputS3KeyPrefix: aws.String(prefix),
		Shots:             aws.Int64(runFlags.shots),
		Tags:              tags,
	})
	switch {
	case errors.Is(err, gate.ErrAlreadySubmitted):
		out.AlreadyRan = true
		// Show the tags the task carries, not this run's recomputation.
		if s.Pinned == nil {
			out.Tags = nil // recorded before tags were kept
		}
	case err != nil:
		return err
	}
	for k, v := range s.Pinned {
		out.Tags[k] = v
	}
	out.Submitted = !out.AlreadyRan
	out.Retried = s.Retried
	out.TaskID = taskID(s.TaskArn)
	out.TaskArn = fmt.Sprintf("arn:aws:braket:%s:<account>:quantum-task/%s", region, out.TaskID)
	out.ResultsPrefix = "s3://" + results.Display(region) + "/" + prefix + "/"
	return writeBraketRun(cmd, out)
}

func writeBraketRun(cmd *cobra.Command, r braketRun) error {
	if jsonOutput {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		if err := enc.Encode(r); err != nil {
			return fmt.Errorf("write JSON: %w", err)
		}
		return nil
	}
	if r.TaskArn == "" {
		return nil // the gate has printed the arithmetic and why it stopped
	}
	w := cmd.OutOrStdout()
	verb := "submitted"
	switch {
	case r.AlreadyRan:
		verb = "already submitted under this run id (nothing new created)"
		if est, ok := r.Tags[tagEstUSD]; ok && "$"+est != r.Estimate.TotalUSD {
			_, _ = fmt.Fprintf(w, "note: the estimate above is for this invocation; the task was created with an estimate of $%s (prices as of %s)\n", est, r.Tags[tagPricesAsOf])
		}
	case r.Retried:
		verb = "submitted (a retry of an earlier attempt whose response was lost; the service returns that attempt's task if it made one)"
	}
	_, _ = fmt.Fprintf(w, "%s: task %s on %s in %s\n", verb, r.TaskID, r.Device, r.Region)
	_, _ = fmt.Fprintf(w, "run id:  %s (repeat it with --run-id to retry without creating a second task)\n", r.RunID)
	_, _ = fmt.Fprintf(w, "results: %s\n", r.ResultsPrefix)
	if r.Tags == nil {
		_, _ = fmt.Fprintln(w, "tags:    (not recorded locally for this run; the task carries the tags it was created with)")
		return nil
	}
	keys := make([]string, 0, len(r.Tags))
	for k := range r.Tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + r.Tags[k]
	}
	_, _ = fmt.Fprintf(w, "tags:    %s\n", strings.Join(parts, " "))
	return nil
}

// taskID is the UUID at the end of a task ARN, which carries no account ID.
func taskID(arn string) string { return arn[strings.LastIndex(arn, "/")+1:] }
