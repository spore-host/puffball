// Package record writes puffball.record/v0: the provenance record for a
// Braket task's result. It covers the device, program, shots, counts,
// physical qubits, compiled program, calibration at submission and at
// completion, estimated cost, software versions and tags. Every S3 object it
// read is linked by key, ETag and SHA-256. Everything it couldn't fill is
// named in missing[] (law 6): a field that is absent is never just empty.
//
// The field set follows the tutorial's GHZ record
// (aws-quantum-tutorial-for-research, ghz4_result_record.json at 8a4d2e0),
// extended for hardware. Its population_score is analysis, not provenance,
// and is left to the analysis that computes it. The JSON Schema is
// [SchemaJSON]; [Verify] checks a record against it and its hashes.
//
// Records are written with HTML escaping off, so OpenQASM's < and & read as
// written. Every hash is over bytes as stored: an S3 object's body, a text
// field's UTF-8, or (for the calibration subset) its compact JSON encoding.
package record

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	braketsdk "github.com/aws/aws-sdk-go-v2/service/braket"
	"github.com/aws/aws-sdk-go-v2/service/braket/types"

	"github.com/spore-host/puffball/internal/scrub"
	"github.com/spore-host/puffball/pkg/braket"
	"github.com/spore-host/puffball/pkg/ledger"
	"github.com/spore-host/puffball/pkg/price"
	"github.com/spore-host/puffball/pkg/results"
)

// Schema is the record format.
const Schema = "puffball.record/v0"

// ResultsLimit caps one result object read: results.json holds every
// measurement, so a large task's is large.
const ResultsLimit = 256 << 20

// Record is puffball.record/v0.
type Record struct {
	Schema           string            `json:"schema"`
	RecordedAt       time.Time         `json:"recorded_at"`
	Task             Task              `json:"task"`
	Device           Device            `json:"device"`
	Program          Program           `json:"program"`
	Shots            Shots             `json:"shots"`
	PhysicalQubits   []int             `json:"physical_qubits"`
	Results          []ProgramResult   `json:"results"`
	CompiledPrograms []Text            `json:"compiled_programs"`
	Calibration      Calibrations      `json:"calibration"`
	Cost             *ledger.Figure    `json:"cost"`
	Software         map[string]string `json:"software"`
	Tags             map[string]string `json:"tags"`
	Objects          []Object          `json:"objects"`
	Missing          []Missing         `json:"missing"`
}

// Task identifies the task. Arn has the account as <account> (law 7).
type Task struct {
	ID        string     `json:"id"`
	Arn       string     `json:"arn"`
	Region    string     `json:"region"`
	Status    string     `json:"status"`
	CreatedAt *time.Time `json:"created_at"`
	EndedAt   *time.Time `json:"ended_at"`
	RunID     string     `json:"run_id,omitempty"`
	Project   string     `json:"project,omitempty"`
}

// Device is the device the task ran on.
type Device struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
	Arn      string `json:"arn"`
}

// Program is what was run.
type Program struct {
	// Format is openqasm3, openqasm3-program-set or ahs.
	Format     string `json:"format"`
	ActionType string `json:"action_type"`
	// Sources are the program texts as the results report them, one per
	// program (one for a single task).
	Sources []Text `json:"sources"`
	// SubmittedSHA256 is the SHA-256 of the action JSON puff run submitted,
	// stored with the results (see Objects).
	SubmittedSHA256 string `json:"submitted_action_sha256,omitempty"`
}

// Text is a text and the SHA-256 of its UTF-8 bytes.
type Text struct {
	Program int    `json:"program"`
	Format  string `json:"format"`
	Text    string `json:"text"`
	SHA256  string `json:"sha256"`
}

// Shots are the task's requested, measured and (where the service reports
// it) successful shots.
type Shots struct {
	Requested  int64  `json:"requested"`
	Measured   int64  `json:"measured"`
	Successful *int64 `json:"successful,omitempty"`
}

// ProgramResult is one program's executables.
type ProgramResult struct {
	Program     int                `json:"program"`
	Executables []ExecutableResult `json:"executables"`
}

// ExecutableResult is one executable's counts. A single task has one program
// with one executable.
type ExecutableResult struct {
	Executable     int              `json:"executable"`
	MeasuredQubits []int            `json:"measured_qubits"`
	Shots          int64            `json:"shots"`
	Counts         map[string]int64 `json:"counts"`
}

// Calibrations are the device's calibration at submission and at completion.
// AtRecord is set only when the device was recalibrated after the task ended,
// so the read made for the record isn't the calibration at completion; it is
// kept, labelled, rather than dropped.
type Calibrations struct {
	AtSubmission *Calibration `json:"at_submission,omitempty"`
	AtCompletion *Calibration `json:"at_completion,omitempty"`
	AtRecord     *Calibration `json:"at_record,omitempty"`
}

// Calibration is the part of a capabilities document that covers the task's
// physical qubits: per-qubit and per-pair properties
// (standardized.oneQubitProperties, twoQubitProperties) and provider
// benchmark sites on those qubits (provider.specs.benchmarks[].sites[]),
// verbatim. CalibratedFrom and CalibratedTo span their timestamps.
type Calibration struct {
	Source         string                       `json:"source"`
	ReadAt         time.Time                    `json:"read_at"`
	DocumentSHA256 string                       `json:"device_capabilities_sha256"`
	CalibratedFrom *time.Time                   `json:"calibrated_from,omitempty"`
	CalibratedTo   *time.Time                   `json:"calibrated_to,omitempty"`
	Qubits         map[string]json.RawMessage   `json:"qubits"`
	Pairs          map[string]json.RawMessage   `json:"pairs"`
	Benchmarks     map[string][]json.RawMessage `json:"benchmarks"`
	// SubsetSHA256 is the SHA-256 of {"benchmarks","pairs","qubits"} as
	// compact JSON with sorted keys (see [SubsetSum]).
	SubsetSHA256 string `json:"subset_sha256"`
}

// Object is an S3 object the record read: its key, ETag, size and SHA-256.
// Bucket has the account as <account>.
type Object struct {
	Bucket string `json:"bucket"`
	Key    string `json:"key"`
	ETag   string `json:"etag"`
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256"`
	// Role says what the object is: results, program, executable, metadata,
	// submitted-action, device-at-submission.
	Role string `json:"role"`
}

// Missing names a field the record couldn't fill, and why.
type Missing struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

// Inputs are what Build reads.
type Inputs struct {
	// Task is the task's GetQuantumTask response.
	Task *braketsdk.GetQuantumTaskOutput
	// Get reads key from the task's results bucket.
	Get func(ctx context.Context, key string) (results.Object, error)
	// Device reads the device's capabilities document now.
	Device func(ctx context.Context) (string, error)
	Prices *price.Table
	// PuffVersion is puff's version.
	PuffVersion string
	// Now is the time the record is made.
	Now func() time.Time
}

type builder struct {
	in     Inputs
	r      *Record
	bucket string
	dir    string
}

func (b *builder) miss(field, reason string) {
	b.r.Missing = append(b.r.Missing, Missing{Field: field, Reason: scrub.Error(errors.New(reason)).Error()})
}

// get reads a result object and links it. A missing object's error is
// short ("not stored"); others keep the cause.
func (b *builder) get(ctx context.Context, key, role string) ([]byte, error) {
	o, err := b.in.Get(ctx, key)
	if errors.Is(err, results.ErrNotFound) {
		return nil, errors.New("not stored")
	}
	if err != nil {
		return nil, err
	}
	b.r.Objects = append(b.r.Objects, Object{Bucket: displayBucket(b.bucket), Key: key, ETag: o.ETag, Bytes: len(o.Body), SHA256: Sum(o.Body), Role: role})
	return o.Body, nil
}

// Build makes the record for a task. Only a missing task is an error;
// anything else it can't read is named in Missing.
func Build(ctx context.Context, in Inputs) (*Record, error) {
	g := in.Task
	if g == nil || g.QuantumTaskArn == nil {
		return nil, errors.New("record: no task")
	}
	if in.Get == nil || in.Device == nil || in.Prices == nil || in.Now == nil {
		return nil, errors.New("record: incomplete inputs")
	}
	arn, dev := aws.ToString(g.QuantumTaskArn), aws.ToString(g.DeviceArn)
	region := braket.RegionOf(arn)
	r := &Record{Schema: Schema, RecordedAt: in.Now().UTC(), Software: map[string]string{}, Tags: g.Tags,
		Objects: []Object{}, Missing: []Missing{}, Results: []ProgramResult{}, CompiledPrograms: []Text{}, PhysicalQubits: []int{}}
	if r.Tags == nil {
		r.Tags = map[string]string{}
	}
	id := last(arn)
	r.Task = Task{ID: id, Arn: braket.TaskARN(region, "<account>", id), Region: region, Status: string(g.Status),
		CreatedAt: utc(g.CreatedAt), EndedAt: utc(g.EndedAt), RunID: g.Tags["puffball:run-id"], Project: g.Tags["puffball:project"]}
	r.Device = Device{Name: last(dev), Provider: provider(dev), Arn: dev}
	r.Shots.Requested = aws.ToInt64(g.Shots)
	r.Shots.Successful = g.NumSuccessfulShots
	b := &builder{in: in, r: r, bucket: aws.ToString(g.OutputS3Bucket), dir: aws.ToString(g.OutputS3Directory)}
	b.software()

	var resultsJSON []byte
	switch {
	case b.bucket == "" || b.dir == "":
		b.miss("results", "the task names no results location")
	case g.Status != types.QuantumTaskStatusCompleted:
		b.miss("results", "the task is "+string(g.Status)+", so it has no results")
	default:
		var err error
		if resultsJSON, err = b.get(ctx, b.dir+"/results.json", "results"); err != nil {
			b.miss("results", "results.json not read: "+err.Error())
		}
	}
	if resultsJSON != nil {
		b.parseResults(ctx, resultsJSON)
	} else {
		b.programFromMetadata()
		for _, f := range []string{"program.sources", "compiled_programs", "physical_qubits", "shots.measured"} {
			b.miss(f, "no results to read it from")
		}
	}
	if r.Shots.Successful == nil {
		b.miss("shots.successful", "the service reports successful shots only for some tasks (program sets); not reported for this one")
	}
	b.submission(ctx)
	b.calibration(ctx)
	b.cost(ctx, resultsJSON)
	sort.Ints(r.PhysicalQubits)
	return r, nil
}

func (b *builder) software() {
	b.r.Software["puff"] = b.in.PuffVersion
	b.r.Software["go"] = runtime.Version()
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, m := range bi.Deps {
			if m.Path == "github.com/aws/aws-sdk-go-v2/service/braket" {
				b.r.Software[m.Path] = m.Version
			}
		}
	}
	if _, ok := b.r.Software["github.com/aws/aws-sdk-go-v2/service/braket"]; !ok {
		b.miss("software.github.com/aws/aws-sdk-go-v2/service/braket", "build info carries no module versions (a test or go run build)")
	}
}

func (b *builder) programFromMetadata() {
	if m := b.in.Task.ActionMetadata; m != nil {
		b.r.Program.ActionType = aws.ToString(m.ActionType)
	}
	b.r.Program.Format = format(b.r.Program.ActionType)
	b.r.Program.Sources = []Text{}
}

type header struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

func (b *builder) header(raw json.RawMessage) {
	var h header
	if json.Unmarshal(raw, &h) == nil && h.Name != "" {
		b.r.Software[h.Name] = h.Version
	}
}

// parseResults reads results.json: a single task's result, or a program
// set's index of programs/<i>/results.json and their executables (#4).
func (b *builder) parseResults(ctx context.Context, raw []byte) {
	var top struct {
		Header             json.RawMessage `json:"braketSchemaHeader"`
		MeasuredQubits     []int           `json:"measuredQubits"`
		Measurements       [][]int         `json:"measurements"`
		ProgramResults     []string        `json:"programResults"`
		TaskMetadata       json.RawMessage `json:"taskMetadata"`
		AdditionalMetadata struct {
			Action  json.RawMessage `json:"action"`
			Rigetti *struct {
				Header   json.RawMessage `json:"braketSchemaHeader"`
				Compiled string          `json:"compiledProgram"`
			} `json:"rigettiMetadata"`
			Simulator json.RawMessage `json:"simulatorMetadata"`
		} `json:"additionalMetadata"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		b.miss("results", "results.json is not JSON: "+err.Error())
		b.programFromMetadata()
		return
	}
	b.header(top.Header)
	if len(top.ProgramResults) > 0 {
		b.programSet(ctx, top.ProgramResults, top.TaskMetadata)
		return
	}
	// A single task.
	var action struct {
		Header json.RawMessage `json:"braketSchemaHeader"`
		Source string          `json:"source"`
	}
	if len(top.AdditionalMetadata.Action) > 0 && json.Unmarshal(top.AdditionalMetadata.Action, &action) == nil {
		var h header
		_ = json.Unmarshal(action.Header, &h)
		b.r.Program.ActionType = h.Name
		b.header(action.Header)
	}
	if b.r.Program.ActionType == "" {
		b.programFromMetadata()
	}
	b.r.Program.Format = format(b.r.Program.ActionType)
	b.r.Program.Sources = []Text{}
	if action.Source != "" {
		b.r.Program.Sources = append(b.r.Program.Sources, text(0, sourceFormat(b.r.Program.Format), action.Source))
	} else {
		b.miss("program.sources", "results.json additionalMetadata.action has no source")
	}
	if top.Measurements == nil {
		b.miss("results", "results.json has no measurements")
		b.miss("shots.measured", "no measurements")
	} else {
		ex := counts(0, top.MeasuredQubits, top.Measurements)
		b.r.Results = append(b.r.Results, ProgramResult{Program: 0, Executables: []ExecutableResult{ex}})
		b.r.Shots.Measured = ex.Shots
	}
	switch rm := top.AdditionalMetadata.Rigetti; {
	case rm != nil && rm.Compiled != "":
		b.header(rm.Header)
		b.compiled(0, rm.Compiled)
	case len(top.AdditionalMetadata.Simulator) > 0:
		b.miss("compiled_programs", "simulators report no compiled program")
		b.miss("physical_qubits", "simulators have no physical qubits")
	default:
		b.miss("compiled_programs", "results.json additionalMetadata carries no compiled program for this provider")
		b.miss("physical_qubits", "no compiled program to read them from")
	}
}

func (b *builder) programSet(ctx context.Context, programs []string, meta json.RawMessage) {
	b.programFromMetadata()
	if b.r.Program.ActionType == "" {
		b.r.Program.ActionType = braket.ActionProgramSet
		b.r.Program.Format = format(b.r.Program.ActionType)
	}
	var metaKey string
	if json.Unmarshal(meta, &metaKey) == nil && metaKey != "" {
		if _, err := b.get(ctx, b.dir+"/"+metaKey, "metadata"); err != nil {
			b.miss("objects.metadata", metaKey+": "+err.Error())
		}
	}
	for i, p := range programs {
		pdir := b.dir + "/" + p[:strings.LastIndex(p, "/")+1]
		raw, err := b.get(ctx, b.dir+"/"+p, "results")
		if err != nil {
			b.miss(fmt.Sprintf("results[%d]", i), p+": "+err.Error())
			continue
		}
		var pr struct {
			Header      json.RawMessage `json:"braketSchemaHeader"`
			Executables []string        `json:"executableResults"`
			Source      string          `json:"source"`
			Additional  struct {
				Rigetti *struct {
					Header   json.RawMessage `json:"braketSchemaHeader"`
					Compiled string          `json:"compiledProgram"`
				} `json:"rigettiMetadata"`
			} `json:"additionalMetadata"`
		}
		if err := json.Unmarshal(raw, &pr); err != nil {
			b.miss(fmt.Sprintf("results[%d]", i), p+" is not JSON: "+err.Error())
			continue
		}
		b.header(pr.Header)
		if pr.Source != "" {
			if src, err := b.get(ctx, pdir+pr.Source, "program"); err != nil {
				b.miss(fmt.Sprintf("program.sources[%d]", i), pr.Source+": "+err.Error())
			} else {
				var prog struct {
					Header json.RawMessage `json:"braketSchemaHeader"`
					Source string          `json:"source"`
				}
				if json.Unmarshal(src, &prog) == nil && prog.Source != "" {
					b.header(prog.Header)
					b.r.Program.Sources = append(b.r.Program.Sources, text(i, "openqasm3", prog.Source))
				} else {
					b.miss(fmt.Sprintf("program.sources[%d]", i), pr.Source+" has no source")
				}
			}
		}
		if rm := pr.Additional.Rigetti; rm != nil && rm.Compiled != "" {
			b.header(rm.Header)
			b.compiled(i, rm.Compiled)
		} else {
			b.miss(fmt.Sprintf("compiled_programs[%d]", i), p+" carries no compiled program")
		}
		res := ProgramResult{Program: i, Executables: []ExecutableResult{}}
		for j, e := range pr.Executables {
			eraw, err := b.get(ctx, pdir+e, "executable")
			if err != nil {
				b.miss(fmt.Sprintf("results[%d].executables[%d]", i, j), e+": "+err.Error())
				continue
			}
			var ex struct {
				Header         json.RawMessage `json:"braketSchemaHeader"`
				MeasuredQubits []int           `json:"measuredQubits"`
				Measurements   [][]int         `json:"measurements"`
			}
			if err := json.Unmarshal(eraw, &ex); err != nil || ex.Measurements == nil {
				b.miss(fmt.Sprintf("results[%d].executables[%d]", i, j), e+" has no measurements")
				continue
			}
			b.header(ex.Header)
			er := counts(j, ex.MeasuredQubits, ex.Measurements)
			res.Executables = append(res.Executables, er)
			b.r.Shots.Measured += er.Shots
		}
		b.r.Results = append(b.r.Results, res)
	}
}

func counts(i int, measured []int, ms [][]int) ExecutableResult {
	c := map[string]int64{}
	var sb strings.Builder
	for _, m := range ms {
		sb.Reset()
		for _, bit := range m {
			sb.WriteString(strconv.Itoa(bit))
		}
		c[sb.String()]++
	}
	if measured == nil {
		measured = []int{}
	}
	return ExecutableResult{Executable: i, MeasuredQubits: measured, Shots: int64(len(ms)), Counts: c}
}

var (
	quilQubit = regexp.MustCompile(`^\d+$`)
	qasmQubit = regexp.MustCompile(`\$(\d+)`)
)

// compiled records a compiled program and the physical qubits it uses:
// Quil names them as bare integers (MEASURE 81 ro[0]), OpenQASM 3 as $81.
func (b *builder) compiled(i int, prog string) {
	f := "quil"
	if strings.HasPrefix(strings.TrimSpace(prog), "OPENQASM") {
		f = "openqasm3"
	}
	b.r.CompiledPrograms = append(b.r.CompiledPrograms, text(i, f, prog))
	seen := map[int]bool{}
	for _, q := range b.r.PhysicalQubits {
		seen[q] = true
	}
	add := func(s string) {
		if n, err := strconv.Atoi(s); err == nil && !seen[n] {
			seen[n] = true
			b.r.PhysicalQubits = append(b.r.PhysicalQubits, n)
		}
	}
	if f == "openqasm3" {
		for _, m := range qasmQubit.FindAllStringSubmatch(prog, -1) {
			add(m[1])
		}
		return
	}
	for _, line := range strings.Split(prog, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] == "PRAGMA" || fields[0] == "DECLARE" || strings.HasPrefix(fields[0], "#") {
			continue
		}
		for _, tok := range fields[1:] {
			if quilQubit.MatchString(tok) {
				add(tok)
			}
		}
	}
}

// submission links what puff run stored before submitting (#42): the action
// JSON and the device snapshot. A task puff didn't submit has neither.
func (b *builder) submission(ctx context.Context) {
	sim := isSimulator(b.r.Device.Arn)
	if b.r.Task.RunID == "" || b.bucket == "" {
		reason := "the task carries no puffball:run-id tag, so puff run didn't submit it"
		b.miss("program.submitted_action_sha256", reason)
		if !sim {
			b.miss("calibration.at_submission", reason)
		}
		return
	}
	dir := SubmissionDir(results.Prefix + b.r.Task.RunID)
	if raw, err := b.get(ctx, dir+ActionObject, "submitted-action"); err != nil {
		b.miss("program.submitted_action_sha256", dir+ActionObject+": "+err.Error())
	} else {
		b.r.Program.SubmittedSHA256 = Sum(raw)
	}
	if sim {
		return // calibration names why simulators have none
	}
	raw, err := b.get(ctx, dir+DeviceObject, "device-at-submission")
	if err != nil {
		b.miss("calibration.at_submission", dir+DeviceObject+": "+err.Error())
		return
	}
	s, err := ParseSnapshot(raw)
	if err != nil {
		b.miss("calibration.at_submission", err.Error())
		return
	}
	if s.DeviceArn != b.r.Device.Arn {
		b.miss("calibration.at_submission", "the snapshot is of "+s.DeviceArn+", not the task's device")
		return
	}
	c, err := subset(s.Capabilities, b.r.PhysicalQubits)
	if err != nil {
		b.miss("calibration.at_submission", err.Error())
		return
	}
	c.Source, c.ReadAt = "puff run, just before submission ("+dir+DeviceObject+")", s.ReadAt
	b.r.Calibration.AtSubmission = c
	b.noTimes(c, "calibration.at_submission")
}

func (b *builder) noTimes(c *Calibration, field string) {
	if c.CalibratedTo == nil {
		b.miss(field+".calibrated_to", "the capabilities carry no calibration timestamp for these qubits (IQM and QuEra carry none, #2)")
	}
}

// calibration reads the device now. Its calibration is the one in force at
// completion when every timestamp in it predates the task's end; if any is
// later, the device was recalibrated after the task ended, and the read is
// kept as at_record instead.
func (b *builder) calibration(ctx context.Context) {
	if isSimulator(b.r.Device.Arn) {
		b.miss("calibration.at_submission", "simulators have no calibration")
		b.miss("calibration.at_completion", "simulators have no calibration")
		return
	}
	caps, err := b.in.Device(ctx)
	if err != nil {
		b.miss("calibration.at_completion", "GetDevice: "+err.Error())
		return
	}
	c, err := subset(caps, b.r.PhysicalQubits)
	if err != nil {
		b.miss("calibration.at_completion", err.Error())
		return
	}
	c.Source, c.ReadAt = "GetDevice when the record was made", b.r.RecordedAt
	end := b.r.Task.EndedAt
	switch {
	case end == nil:
		b.r.Calibration.AtRecord = c
		b.miss("calibration.at_completion", "the task has no end time")
	case c.CalibratedTo == nil:
		b.r.Calibration.AtRecord = c
		b.miss("calibration.at_completion", "the capabilities carry no calibration timestamp, so whether this is the calibration at completion is unknown (IQM and QuEra carry none, #2)")
	case c.CalibratedTo.After(*end):
		b.r.Calibration.AtRecord = c
		b.miss("calibration.at_completion", fmt.Sprintf("the device was recalibrated at %s, after the task ended at %s; at_record is the later calibration", c.CalibratedTo.Format(time.RFC3339), end.Format(time.RFC3339)))
	default:
		b.r.Calibration.AtCompletion = c
	}
}

// cost is the ledger's estimate for the task (#41), using the results.json
// already read.
func (b *builder) cost(ctx context.Context, resultsJSON []byte) {
	g := b.in.Task
	sum := types.QuantumTaskSummary{QuantumTaskArn: g.QuantumTaskArn, DeviceArn: g.DeviceArn, Status: g.Status, Shots: g.Shots,
		CreatedAt: g.CreatedAt, EndedAt: g.EndedAt, OutputS3Bucket: g.OutputS3Bucket, OutputS3Directory: g.OutputS3Directory, Tags: g.Tags}
	o := ledger.Options{Prices: b.in.Prices, Reader: memReader(resultsJSON), Account: func(context.Context) (string, error) { return "", nil }}
	l, err := ledger.Build(ctx, []braket.Task{{Region: b.r.Task.Region, Summary: sum}}, o)
	if err != nil || len(l.Lines) != 1 {
		b.miss("cost", fmt.Sprintf("ledger: %v", err))
		return
	}
	ln := l.Lines[0]
	if ln.Estimate == nil {
		b.miss("cost", ln.Note)
		return
	}
	b.r.Cost = ln.Estimate
	if ln.Standing == ledger.Pending {
		b.miss("cost.final", "the task hasn't finished; the estimate is for it as submitted")
	}
	if ln.Note != "" && ln.Standing != ledger.Pending {
		b.miss("cost.billed_duration", ln.Note)
	}
}

type memReader []byte

func (m memReader) Read(context.Context, string, string, string, string, int64) (results.Object, error) {
	if m == nil {
		return results.Object{}, errors.New("results.json wasn't read")
	}
	return results.Object{Body: m}, nil
}

func text(i int, f, s string) Text {
	return Text{Program: i, Format: f, Text: s, SHA256: Sum([]byte(s))}
}

func format(actionType string) string {
	switch {
	case actionType == braket.ActionProgramSet:
		return "openqasm3-program-set"
	case strings.HasPrefix(actionType, "braket.ir.openqasm."):
		return "openqasm3"
	case strings.HasPrefix(actionType, "braket.ir.ahs."):
		return "ahs"
	default:
		return "unknown"
	}
}

func sourceFormat(f string) string {
	if f == "openqasm3-program-set" {
		return "openqasm3"
	}
	return f
}

func isSimulator(deviceArn string) bool {
	return strings.Contains(deviceArn, ":device/quantum-simulator/")
}

func last(arn string) string { return arn[strings.LastIndex(arn, "/")+1:] }

// provider is the provider part of a device ARN: rigetti in
// arn:aws:braket:us-west-1::device/qpu/rigetti/Cepheus-1-108Q.
func provider(arn string) string {
	parts := strings.Split(arn[strings.Index(arn, ":device/")+1:], "/")
	if len(parts) >= 3 {
		return parts[2]
	}
	return ""
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func displayBucket(b string) string {
	return strings.ReplaceAll(string(scrub.Scrub([]byte(b))), scrub.Placeholder, "<account>")
}
