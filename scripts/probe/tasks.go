package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/braket"
	"github.com/aws/aws-sdk-go-v2/service/braket/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// G0 prices, from the tutorial's devices.py (PRICING_AS_OF = "2026-10") and
// confirmed against the Price List API in G0(b) (#3).
const (
	pricesAsOf      = "2026-10"
	perTaskUSD      = 0.30
	cepheusShotUSD  = 0.000425
	simPerMinuteUSD = 0.075
	simMinSeconds   = 3
	g0StopUSD       = 2.00 // G0 budget $3, stop at $2 (#1)
)

// bellQASM is a two-qubit Bell circuit.
const bellQASM = `OPENQASM 3.0;
bit[2] b;
qubit[2] q;
h q[0];
cnot q[0], q[1];
b[0] = measure q[0];
b[1] = measure q[1];`

// bellFlippedQASM prepares (|01> + |10>)/sqrt(2), so the two programs in the
// program set are distinguishable in the results.
const bellFlippedQASM = `OPENQASM 3.0;
bit[2] b;
qubit[2] q;
h q[0];
cnot q[0], q[1];
x q[1];
b[0] = measure q[0];
b[1] = measure q[1];`

// Action JSON, from amazon-braket-schemas-python
// (src/braket/ir/openqasm/program_v1.py and program_set_v1.py, commit 8aa7ca9).
type schemaHeader struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type qasmProgram struct {
	Header schemaHeader `json:"braketSchemaHeader"`
	Source string       `json:"source"`
}

type qasmProgramSet struct {
	Header   schemaHeader  `json:"braketSchemaHeader"`
	Programs []qasmProgram `json:"programs"`
}

func program(src string) qasmProgram {
	return qasmProgram{Header: schemaHeader{"braket.ir.openqasm.program", "1"}, Source: src}
}

// plannedTask is one G0(c) submission.
type plannedTask struct {
	Label     string  `json:"label"`
	Rung      string  `json:"rung"`
	DeviceArn string  `json:"device_arn"`
	Shots     int64   `json:"shots"`
	Action    string  `json:"action"`
	EstUSD    float64 `json:"est_usd"`
	Arith     string  `json:"arithmetic"`
	Arn       string  `json:"task_arn,omitempty"`
	ClientTok string  `json:"client_token,omitempty"`
}

// tasksState is what a submission run leaves behind, so a re-run resumes
// instead of spending again.
type tasksState struct {
	Region    string        `json:"region"`
	RunID     string        `json:"run_id"`
	Submitted time.Time     `json:"submitted_at"`
	Tasks     []plannedTask `json:"tasks"`
	Bucket    string        `json:"bucket_created,omitempty"`
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func money(v float64) string {
	s := fmt.Sprintf("%.5f", v)
	s = strings.TrimRight(s, "0")
	if i := strings.IndexByte(s, '.'); len(s)-i-1 < 2 {
		s += strings.Repeat("0", 2-(len(s)-i-1))
	}
	return "$" + s
}

// runTasks is G0(c) (#4). Billable: refuses without --real.
func runTasks(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("tasks", flag.ContinueOnError)
	out := flags.String("out", ".probe/tasks", "output directory (raw, unscrubbed)")
	real := flags.Bool("real", false, "actually submit (billable)")
	wait := flags.Duration("wait", 30*time.Minute, "how long to poll for completion")
	resume := flags.Bool("resume", false, "don't submit; re-poll and fetch the tasks in the state file")
	adopt := flags.String("adopt", "", "label=arn: write a state file for a task submitted by a run that failed before recording it; submits nothing")
	finish := flags.Bool("finish", false, "with --real: submit the planned tasks the state file has no ARN for (after a failed run)")
	if err := flags.Parse(args); err != nil {
		return usageErr{err.Error()}
	}
	statePath := filepath.Join(*out, "submitted.json")

	var st tasksState
	b, err := os.ReadFile(statePath)
	switch {
	case err == nil:
		if err := json.Unmarshal(b, &st); err != nil {
			return fmt.Errorf("read %s: %w", statePath, err)
		}
		if *adopt != "" {
			return fmt.Errorf("%s already exists; --adopt only writes a new one", statePath)
		}
		if !*resume && !*finish {
			return fmt.Errorf("%s exists (run %s): G0(c) tasks were already submitted; use --resume to re-poll them, or --finish --real to submit the ones that failed", statePath, st.RunID)
		}
		if st.Region == "" || st.RunID == "" {
			return fmt.Errorf("%s has no region or run id; refusing to submit from it", statePath)
		}
	case !errors.Is(err, fs.ErrNotExist):
		// Anything but "no state yet" (a permission error, say) must not
		// fall through to a fresh plan and a second spend.
		return fmt.Errorf("read %s: %w", statePath, err)
	case *resume || *finish:
		return usageErr{"--resume/--finish: no " + statePath}
	}

	if *adopt != "" {
		return adoptTask(ctx, *adopt, statePath)
	}
	if *finish {
		if !*real {
			return usageErr{"--finish submits tasks; it needs --real"}
		}
		if err := submit(ctx, &st, *out, statePath); err != nil {
			return err
		}
	}
	if !*resume && !*finish {
		var err error
		st, err = planTasks(ctx)
		if err != nil {
			return err
		}
		total := 0.0
		fmt.Fprintf(os.Stderr, "G0(c) plan, region %s (prices as of %s):\n", st.Region, pricesAsOf)
		for _, t := range st.Tasks {
			fmt.Fprintf(os.Stderr, "  %-22s estimate %s\n", t.Label, t.Arith)
			total += t.EstUSD
		}
		fmt.Fprintf(os.Stderr, "  total estimate %s (G0 stop limit %s)\n", money(total), money(g0StopUSD))
		if total > g0StopUSD {
			return fmt.Errorf("plan estimate %s exceeds the G0 stop limit %s", money(total), money(g0StopUSD))
		}
		if !*real {
			fmt.Fprintln(os.Stderr, "Not submitted. Pass --real to spend this.")
			return nil
		}
		// submit writes the state file after every task it submits, so a
		// failure part-way still records what was spent.
		if err := submit(ctx, &st, *out, statePath); err != nil {
			return err
		}
	}
	return pollAndFetch(ctx, st, *out, *wait)
}

// planTasks resolves the devices from live discovery and builds the plan.
func planTasks(ctx context.Context) (tasksState, error) {
	seed, err := seedRegion(ctx)
	if err != nil {
		return tasksState{}, err
	}
	d, err := discover(ctx, seed)
	if err != nil {
		return tasksState{}, err
	}
	cepheus, ok := d.find("Cepheus")
	if !ok {
		return tasksState{}, errors.New("no Cepheus device found in any region")
	}
	if cepheus.DeviceStatus != types.DeviceStatusOnline {
		return tasksState{}, fmt.Errorf("device Cepheus is %s, not ONLINE", cepheus.DeviceStatus)
	}
	region := regionOf(aws.ToString(cepheus.DeviceArn))
	res, ok := d.Regions[region]
	if region == "" || !ok || res.Err != nil {
		return tasksState{}, fmt.Errorf("region %q of Cepheus was not searched successfully", region)
	}
	var sv1 string
	for _, dev := range res.Devices {
		if aws.ToString(dev.DeviceName) == "SV1" {
			sv1 = aws.ToString(dev.DeviceArn)
		}
	}
	if sv1 == "" {
		return tasksState{}, fmt.Errorf("no SV1 in %s", region)
	}

	mustJSON := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	bell := mustJSON(program(bellQASM))
	set := mustJSON(qasmProgramSet{
		Header:   schemaHeader{"braket.ir.openqasm.program_set", "1"},
		Programs: []qasmProgram{program(bellQASM), program(bellFlippedQASM)},
	})
	simEst := simPerMinuteUSD * simMinSeconds / 60
	return tasksState{
		Region: region,
		RunID:  "g0c-" + randHex(4),
		Tasks: []plannedTask{
			{Label: "sv1-bell-100", Rung: "sv1", DeviceArn: sv1, Shots: 100, Action: bell, EstUSD: simEst,
				Arith: fmt.Sprintf("%d s minimum x $%g/min = %s", simMinSeconds, simPerMinuteUSD, money(simEst))},
			{Label: "cepheus-bell-100", Rung: "qpu", DeviceArn: aws.ToString(cepheus.DeviceArn), Shots: 100, Action: bell,
				EstUSD: perTaskUSD + 100*cepheusShotUSD, Arith: fmt.Sprintf("$%.2f + 100 x $%g = %s", perTaskUSD, cepheusShotUSD, money(perTaskUSD+100*cepheusShotUSD))},
			// Program set: shots is the TOTAL across executables (the SDK sends
			// shots_per_executable x executables; amazon-braket-sdk-python
			// aws_quantum_task.py), so 2 programs x 100 shots = 200.
			{Label: "cepheus-progset-2x100", Rung: "qpu", DeviceArn: aws.ToString(cepheus.DeviceArn), Shots: 200, Action: set,
				EstUSD: perTaskUSD + 200*cepheusShotUSD, Arith: fmt.Sprintf("$%.2f + 200 x $%g = %s", perTaskUSD, cepheusShotUSD, money(perTaskUSD+200*cepheusShotUSD))},
		},
	}, nil
}

// submit creates the results bucket if absent and submits every planned task.
func submit(ctx context.Context, st *tasksState, out, statePath string) error {
	pending := 0
	for _, t := range st.Tasks {
		if t.Arn == "" {
			pending++
		}
	}
	if pending == 0 {
		return nil
	}
	if st.Region == "" || st.RunID == "" {
		return errors.New("submit: state has no region or run id")
	}
	cfg, rec, err := loadConfig(ctx, st.Region)
	if err != nil {
		return err
	}
	ident, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return fmt.Errorf("GetCallerIdentity: %w", err)
	}
	if aws.ToString(ident.Account) == "" {
		return errors.New("GetCallerIdentity returned no account")
	}
	bucket := fmt.Sprintf("amazon-braket-%s-%s", st.Region, aws.ToString(ident.Account))
	shown := fmt.Sprintf("amazon-braket-%s-<account>", st.Region)
	s3c := s3.NewFromConfig(cfg)
	if _, err := s3c.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)}); err != nil {
		var nf *s3types.NotFound
		if !errors.As(err, &nf) {
			return fmt.Errorf("HeadBucket %s: %w", shown, err)
		}
		fmt.Fprintf(os.Stderr, "results bucket %s does not exist; creating it (--real run)\n", shown)
		in := &s3.CreateBucketInput{Bucket: aws.String(bucket)}
		if st.Region != "us-east-1" {
			in.CreateBucketConfiguration = &s3types.CreateBucketConfiguration{LocationConstraint: s3types.BucketLocationConstraint(st.Region)}
		}
		if _, err := s3c.CreateBucket(ctx, in); err != nil {
			return fmt.Errorf("CreateBucket %s: %w", shown, err)
		}
		st.Bucket = shown
	} else {
		fmt.Fprintf(os.Stderr, "results bucket %s exists\n", shown)
	}

	bc := braket.NewFromConfig(cfg)
	if st.Submitted.IsZero() {
		st.Submitted = time.Now().UTC()
	}
	for i := range st.Tasks {
		t := &st.Tasks[i]
		if t.Arn != "" {
			continue // submitted by an earlier run
		}
		// The client token makes CreateQuantumTask idempotent: keep the one a
		// previous attempt used, and save it before the call, so a retry after
		// a lost response cannot create (and pay for) a second task.
		if t.ClientTok == "" {
			t.ClientTok = randHex(16)
		}
		if err := writeJSON(statePath, st); err != nil {
			return err
		}
		rec.Reset()
		fmt.Fprintf(os.Stderr, "submitting %s: estimate %s (prices as of %s)\n", t.Label, t.Arith, pricesAsOf)
		resp, err := bc.CreateQuantumTask(ctx, &braket.CreateQuantumTaskInput{
			Action:            aws.String(t.Action),
			ClientToken:       aws.String(t.ClientTok),
			DeviceArn:         aws.String(t.DeviceArn),
			OutputS3Bucket:    aws.String(bucket),
			OutputS3KeyPrefix: aws.String("puffball/" + st.RunID),
			Shots:             aws.Int64(t.Shots),
			Tags: map[string]string{
				"puffball:project": "puffball-g0",
				"puffball:rung":    t.Rung,
				"puffball:run-id":  st.RunID,
				"puffball:est-usd": strings.TrimPrefix(money(t.EstUSD), "$"),
			},
		})
		if err != nil {
			_ = writeRaw(filepath.Join(out, t.Label, "create-error.txt"), []byte(err.Error()+"\n"+string(rec.Last())))
			if werr := writeJSON(statePath, st); werr != nil {
				return werr
			}
			return fmt.Errorf("CreateQuantumTask %s: %w", t.Label, err)
		}
		t.Arn = aws.ToString(resp.QuantumTaskArn)
		if err := writeJSON(statePath, st); err != nil {
			return err
		}
		if t.Arn == "" {
			return fmt.Errorf("CreateQuantumTask %s returned no task ARN; the task may exist — check with SearchQuantumTasks before retrying (client token kept in %s)", t.Label, statePath)
		}
		if err := writeRaw(filepath.Join(out, t.Label, "create.json"), rec.Last()); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "  submitted %s\n", t.Label)
	}
	return nil
}

func terminal(s types.QuantumTaskStatus) bool {
	return s == types.QuantumTaskStatusCompleted || s == types.QuantumTaskStatusFailed || s == types.QuantumTaskStatusCancelled
}

// pollAndFetch polls every task with queue info until it ends or wait runs
// out, then saves the final metadata, the S3 results and the tag checks.
func pollAndFetch(ctx context.Context, st tasksState, out string, wait time.Duration) error {
	cfg, rec, err := loadConfig(ctx, st.Region)
	if err != nil {
		return err
	}
	bc := braket.NewFromConfig(cfg)
	s3c := s3.NewFromConfig(cfg)
	deadline := time.Now().Add(wait)
	done := map[string]bool{}
	want := 0
	for _, t := range st.Tasks {
		if t.Arn != "" {
			want++
		}
	}
	for poll := 0; ; poll++ {
		for _, t := range st.Tasks {
			if done[t.Label] || t.Arn == "" {
				continue
			}
			g, err := bc.GetQuantumTask(ctx, &braket.GetQuantumTaskInput{
				QuantumTaskArn:           aws.String(t.Arn),
				AdditionalAttributeNames: []types.QuantumTaskAdditionalAttributeName{types.QuantumTaskAdditionalAttributeNameQueueInfo},
			})
			if err != nil {
				return fmt.Errorf("GetQuantumTask %s: %w", t.Label, err)
			}
			body := rec.Last()
			if err := writeRaw(filepath.Join(out, t.Label, fmt.Sprintf("get.%03d.json", poll)), body); err != nil {
				return err
			}
			q := ""
			if g.QueueInfo != nil {
				q = fmt.Sprintf(" queue=%s position=%s", g.QueueInfo.Queue, aws.ToString(g.QueueInfo.Position))
			}
			fmt.Fprintf(os.Stderr, "%s %s %s%s\n", time.Now().UTC().Format("15:04:05"), t.Label, g.Status, q)
			if terminal(g.Status) {
				done[t.Label] = true
				if err := writeRaw(filepath.Join(out, t.Label, "get.final.json"), body); err != nil {
					return err
				}
				if err := fetchResults(ctx, s3c, aws.ToString(g.OutputS3Bucket), aws.ToString(g.OutputS3Directory), filepath.Join(out, t.Label, "s3")); err != nil {
					fmt.Fprintln(os.Stderr, "  ", scrubbed(err))
				}
			}
		}
		if len(done) == want || time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(15 * time.Second):
		}
	}
	if err := tagChecks(ctx, bc, rec, st, out); err != nil {
		return err
	}
	if len(done) < want {
		return fmt.Errorf("%d of %d submitted tasks still not finished after %s; re-run with --resume", want-len(done), want, wait)
	}
	if want < len(st.Tasks) {
		return fmt.Errorf("%d of %d planned tasks were never submitted; see create-error.txt, then --finish --real", len(st.Tasks)-want, len(st.Tasks))
	}
	return nil
}

func fetchResults(ctx context.Context, s3c *s3.Client, bucket, dir, out string) error {
	p := s3.NewListObjectsV2Paginator(s3c, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(dir)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("ListObjectsV2: %w", err)
		}
		for _, o := range page.Contents {
			key := aws.ToString(o.Key)
			obj, err := s3c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
			if err != nil {
				return fmt.Errorf("GetObject %s: %w", filepath.Base(key), err)
			}
			b, err := io.ReadAll(obj.Body)
			_ = obj.Body.Close()
			if err != nil {
				return fmt.Errorf("read %s: %w", filepath.Base(key), err)
			}
			rel, err := resultPath(dir, key)
			if err != nil {
				return err
			}
			if err := writeRaw(filepath.Join(out, rel), b); err != nil {
				return err
			}
		}
	}
	return nil
}

// resultPath is key relative to the task's output directory. Program-set
// results nest (programs/<i>/results.json), so the base name alone would let
// one program's files overwrite another's.
func resultPath(dir, key string) (string, error) {
	rel := strings.TrimPrefix(strings.TrimPrefix(key, dir), "/")
	clean := filepath.Clean(filepath.FromSlash(rel))
	if rel == "" || !filepath.IsLocal(clean) {
		return "", fmt.Errorf("S3 key %q is not inside %q", key, dir)
	}
	return clean, nil
}

// tagChecks records whether SearchQuantumTasks can filter on tags (the API
// documents only quantumTaskArn, deviceArn, jobArn, status and createdAt),
// whether search summaries carry the tags, and ListTagsForResource per task.
func tagChecks(ctx context.Context, bc *braket.Client, rec *recorder, st tasksState, out string) error {
	dir := filepath.Join(out, "tags")
	res := map[string]string{}
	for _, name := range []string{"tags", "tag:puffball:run-id", "puffball:run-id"} {
		_, err := bc.SearchQuantumTasks(ctx, &braket.SearchQuantumTasksInput{Filters: []types.SearchQuantumTasksFilter{{
			Name: aws.String(name), Operator: types.SearchQuantumTasksFilterOperatorEqual, Values: []string{st.RunID},
		}}})
		if err != nil {
			res["filter "+name] = err.Error()
		} else {
			res["filter "+name] = "accepted"
			_ = writeRaw(filepath.Join(dir, "search-filter-"+strings.ReplaceAll(name, ":", "_")+".json"), rec.Last())
		}
	}
	// By time window, then client-side on the summaries' Tags.
	if _, err := bc.SearchQuantumTasks(ctx, &braket.SearchQuantumTasksInput{Filters: []types.SearchQuantumTasksFilter{{
		Name: aws.String("createdAt"), Operator: types.SearchQuantumTasksFilterOperatorGte,
		Values: []string{st.Submitted.Add(-time.Minute).Format(time.RFC3339)},
	}}}); err != nil {
		res["search createdAt"] = err.Error()
	} else if err := writeRaw(filepath.Join(dir, "search-createdAt.json"), rec.Last()); err != nil {
		return err
	}
	for _, t := range st.Tasks {
		if t.Arn == "" {
			continue
		}
		if _, err := bc.ListTagsForResource(ctx, &braket.ListTagsForResourceInput{ResourceArn: aws.String(t.Arn)}); err != nil {
			res["list-tags "+t.Label] = err.Error()
			continue
		}
		if err := writeRaw(filepath.Join(dir, "list-tags-"+t.Label+".json"), rec.Last()); err != nil {
			return err
		}
	}
	return writeJSON(filepath.Join(dir, "results.json"), res)
}

// adoptTask rebuilds the plan, attaches an already-submitted task's ARN to its
// label, and takes the run ID and submission time from the task itself.
func adoptTask(ctx context.Context, spec, statePath string) error {
	label, arn, ok := strings.Cut(spec, "=")
	if !ok {
		return usageErr{"--adopt wants label=arn"}
	}
	st, err := planTasks(ctx)
	if err != nil {
		return err
	}
	cfg, _, err := loadConfig(ctx, st.Region)
	if err != nil {
		return err
	}
	g, err := braket.NewFromConfig(cfg).GetQuantumTask(ctx, &braket.GetQuantumTaskInput{QuantumTaskArn: aws.String(arn)})
	if err != nil {
		return fmt.Errorf("GetQuantumTask: %w", err)
	}
	if regionOf(arn) != st.Region {
		return fmt.Errorf("task is in %s, plan is for %s", regionOf(arn), st.Region)
	}
	st.RunID = g.Tags["puffball:run-id"]
	st.Submitted = aws.ToTime(g.CreatedAt)
	found := false
	for i := range st.Tasks {
		if st.Tasks[i].Label == label {
			if st.Tasks[i].DeviceArn != aws.ToString(g.DeviceArn) {
				return fmt.Errorf("task ran on %s, label %s plans %s", aws.ToString(g.DeviceArn), label, st.Tasks[i].DeviceArn)
			}
			st.Tasks[i].Arn = arn
			found = true
		}
	}
	if !found || st.RunID == "" {
		return fmt.Errorf("label %q not in the plan, or the task has no puffball:run-id tag", label)
	}
	return writeJSON(statePath, st)
}
