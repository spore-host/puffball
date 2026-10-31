package gate_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	braketsdk "github.com/aws/aws-sdk-go-v2/service/braket"
	"github.com/aws/aws-sdk-go-v2/service/braket/types"

	pbraket "github.com/spore-host/puffball/pkg/braket"
	"github.com/spore-host/puffball/pkg/gate"
	"github.com/spore-host/puffball/pkg/testutil"
)

func est(key string, usd float64) gate.Estimate {
	return gate.Estimate{Key: key, Label: key, Arithmetic: "$0.30 + 100 x $0.000425 = " + gate.Money(usd), USD: usd, AsOf: "2026-10"}
}

func budget(v float64) *float64 { return &v }

func newGate(t *testing.T, o gate.Options) (*gate.Gate, *bytes.Buffer) {
	t.Helper()
	var out bytes.Buffer
	o.Out = &out
	if o.Creators == nil {
		o.Creators = testutil.FailingCreators(t)
	}
	if o.Tokens == nil {
		o.Tokens = &gate.MemTokens{}
	}
	g, err := gate.New(o)
	if err != nil {
		t.Fatal(err)
	}
	return g, &out
}

// TestApproveStopsWithoutReal is A-gate's unit half (#8): with no --real, or
// with --dry-run, the arithmetic is printed and no Permit exists, so no
// billable call is possible.
func TestApproveStopsWithoutReal(t *testing.T) {
	cases := []struct {
		name string
		o    gate.Options
		says string
	}{
		{"no --real", gate.Options{}, "Not submitted. Pass --real to spend this."},
		{"--dry-run", gate.Options{DryRun: true}, "Dry run: not submitted."},
		{"--real --dry-run", gate.Options{Real: true, DryRun: true}, "Dry run: not submitted."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, out := newGate(t, tc.o)
			p, err := g.Approve([]gate.Estimate{est("r/a", 0.3425)})
			if err != nil || p != nil {
				t.Fatalf("Approve = %v, %v; want no permit and no error", p, err)
			}
			for _, want := range []string{"estimate (prices as of 2026-10): r/a: $0.30 + 100 x $0.000425 = $0.3425", tc.says} {
				if !strings.Contains(out.String(), want) {
					t.Errorf("printout %q lacks %q", out, want)
				}
			}
		})
	}
}

func TestApproveBudget(t *testing.T) {
	plan := []gate.Estimate{est("r/a", 0.3425), est("r/b", 0.385)}
	g, out := newGate(t, gate.Options{Real: true, Budget: budget(0.70)})
	if _, err := g.Approve(plan); !errors.Is(err, gate.ErrOverBudget) {
		t.Fatalf("err = %v, want ErrOverBudget", err)
	}
	if !strings.Contains(out.String(), "total $0.7275") {
		t.Errorf("total not printed: %q", out)
	}
	// Over budget is refused even without --real: the user sees why it would fail.
	g, _ = newGate(t, gate.Options{Budget: budget(0.5)})
	if _, err := g.Approve(plan); !errors.Is(err, gate.ErrOverBudget) {
		t.Errorf("over budget without --real: %v", err)
	}
	g, _ = newGate(t, gate.Options{Real: true, Budget: budget(0.7275)})
	if p, err := g.Approve(plan); err != nil || p == nil {
		t.Errorf("exactly at budget: %v, %v", p, err)
	}
	// --budget 0 means spend nothing, not "no budget".
	g, _ = newGate(t, gate.Options{Real: true, Budget: budget(0)})
	if _, err := g.Approve(plan); !errors.Is(err, gate.ErrOverBudget) {
		t.Errorf("--budget 0: %v, want ErrOverBudget", err)
	}
	free := est("r/free", 0)
	if p, err := g.Approve([]gate.Estimate{free}); err != nil || p == nil {
		t.Errorf("--budget 0 with a $0 plan: %v, %v", p, err)
	}
}

func TestApproveRejectsBadEstimates(t *testing.T) {
	bad := map[string]gate.Estimate{
		"no key":        {Label: "x", Arithmetic: "a", USD: 1, AsOf: "2026-10"},
		"no as-of":      {Key: "k", Label: "x", Arithmetic: "a", USD: 1},
		"no arithmetic": {Key: "k", Label: "x", USD: 1, AsOf: "2026-10"},
		"negative":      {Key: "k", Label: "x", Arithmetic: "a", USD: -1, AsOf: "2026-10"},
		"NaN":           {Key: "k", Label: "x", Arithmetic: "a", USD: math.NaN(), AsOf: "2026-10"},
		"Inf":           {Key: "k", Label: "x", Arithmetic: "a", USD: math.Inf(1), AsOf: "2026-10"},
	}
	for name, e := range bad {
		g, _ := newGate(t, gate.Options{Real: true})
		if p, err := g.Approve([]gate.Estimate{e}); err == nil || p != nil {
			t.Errorf("%s: accepted", name)
		}
	}
	g, _ := newGate(t, gate.Options{Real: true})
	if _, err := g.Approve(nil); err == nil {
		t.Error("empty plan accepted")
	}
	if _, err := g.Approve([]gate.Estimate{est("k", 1), est("k", 1)}); err == nil {
		t.Error("duplicate key accepted")
	}
	mixed := est("k2", 1)
	mixed.AsOf = "2026-09"
	g2, out := newGate(t, gate.Options{})
	_, _ = g2.Approve([]gate.Estimate{est("k1", 1), mixed})
	if !strings.Contains(out.String(), "estimate (prices as of 2026-09, 2026-10): total") {
		t.Errorf("the total must list every as-of date: %q", out)
	}
}

func TestNewValidates(t *testing.T) {
	ok := gate.Options{Out: &bytes.Buffer{}, Creators: testutil.FailingCreators(t), Tokens: &gate.MemTokens{}}
	for name, mut := range map[string]func(*gate.Options){
		"no Out":          func(o *gate.Options) { o.Out = nil },
		"no Tokens":       func(o *gate.Options) { o.Tokens = nil },
		"negative budget": func(o *gate.Options) { o.Budget = budget(-1) },
		"NaN budget":      func(o *gate.Options) { o.Budget = budget(math.NaN()) },
	} {
		o := ok
		mut(&o)
		if _, err := gate.New(o); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := gate.New(ok); err != nil {
		t.Errorf("valid options: %v", err)
	}
}

func input() braketsdk.CreateQuantumTaskInput {
	return braketsdk.CreateQuantumTaskInput{
		Action:            aws.String(`{"braketSchemaHeader":{"name":"braket.ir.openqasm.program","version":"1"},"source":"OPENQASM 3.0;"}`),
		DeviceArn:         aws.String("arn:aws:braket:::device/quantum-simulator/amazon/sv1"),
		OutputS3Bucket:    aws.String("amazon-braket-us-west-1-000000000000"),
		OutputS3KeyPrefix: aws.String("puffball/run"),
		Shots:             aws.Int64(100),
		Tags:              map[string]string{"puffball:run-id": "run", "puffball:rung": "sv1"},
	}
}

func TestPermitCreates(t *testing.T) {
	rc := &testutil.RecordingCreator{}
	g, out := newGate(t, gate.Options{Real: true, Creators: rc.Factory()})
	p, err := g.Approve([]gate.Estimate{est("r/a", 0.00375)})
	if err != nil || p == nil {
		t.Fatal(p, err)
	}
	s, err := p.CreateQuantumTask(context.Background(), "us-west-1", "r/a", input())
	if err != nil || s.TaskArn == "" || s.ClientToken == "" {
		t.Fatalf("CreateQuantumTask = %+v, %v", s, err)
	}
	if aws.ToString(rc.Calls[0].ClientToken) != s.ClientToken {
		t.Error("the stored client token was not sent")
	}
	if !strings.Contains(out.String(), "submitting r/a: estimate $0.00375 (prices as of 2026-10)") {
		t.Errorf("submission not printed with its estimate: %q", out)
	}
	if _, err := p.CreateQuantumTask(context.Background(), "us-west-1", "r/a", input()); err == nil {
		t.Error("the same approved item was spent twice under one permit")
	}
	if _, err := p.CreateQuantumTask(context.Background(), "us-west-1", "r/never-approved", input()); err == nil {
		t.Error("an unapproved key was submitted")
	}
}

// TestRetryAfterLostResponseCreatesOnce: the create succeeds at the service
// but the response is lost. The retry, in a new run, sends the same client
// token, so exactly one task exists.
func TestRetryAfterLostResponseCreatesOnce(t *testing.T) {
	rc := &testutil.RecordingCreator{LoseResponse: 1}
	tokens := &gate.MemTokens{}
	run := func() (gate.Submission, error) {
		g, _ := newGate(t, gate.Options{Real: true, Creators: rc.Factory(), Tokens: tokens})
		p, err := g.Approve([]gate.Estimate{est("r/a", 0.3425)})
		if err != nil {
			t.Fatal(err)
		}
		return p.CreateQuantumTask(context.Background(), "us-west-1", "r/a", input())
	}
	if _, err := run(); err == nil {
		t.Fatal("first attempt should report the lost response")
	}
	s, err := run()
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if rc.Created() != 1 || len(rc.Calls) != 2 {
		t.Fatalf("created %d tasks over %d calls; want 1 over 2", rc.Created(), len(rc.Calls))
	}
	if aws.ToString(rc.Calls[0].ClientToken) != aws.ToString(rc.Calls[1].ClientToken) {
		t.Error("retry used a different client token")
	}
	// A third run finds the ARN recorded and does not call the service.
	again, err := run()
	if !errors.Is(err, gate.ErrAlreadySubmitted) || again.TaskArn != s.TaskArn || len(rc.Calls) != 2 {
		t.Errorf("third run: %+v, %v, %d calls; want ErrAlreadySubmitted, the same ARN, no call", again, err, len(rc.Calls))
	}
}

func TestCreateFailures(t *testing.T) {
	ctx := context.Background()
	t.Run("token not saved means no call", func(t *testing.T) {
		g, _ := newGate(t, gate.Options{Real: true, Tokens: &gate.MemTokens{Err: errors.New("disk full")}})
		p, _ := g.Approve([]gate.Estimate{est("r/a", 1)})
		if _, err := p.CreateQuantumTask(ctx, "us-west-1", "r/a", input()); err == nil {
			t.Error("submitted without a saved client token")
		}
	})
	t.Run("service error", func(t *testing.T) {
		rc := &testutil.RecordingCreator{Err: errors.New("AccessDeniedException")}
		g, _ := newGate(t, gate.Options{Real: true, Creators: rc.Factory()})
		p, _ := g.Approve([]gate.Estimate{est("r/a", 1)})
		if _, err := p.CreateQuantumTask(ctx, "us-west-1", "r/a", input()); err == nil || !strings.Contains(err.Error(), "AccessDenied") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("input changed on retry", func(t *testing.T) {
		rc := &testutil.RecordingCreator{LoseResponse: 1}
		tokens := &gate.MemTokens{}
		for i, shots := range []int64{100, 200} {
			g, _ := newGate(t, gate.Options{Real: true, Creators: rc.Factory(), Tokens: tokens})
			p, _ := g.Approve([]gate.Estimate{est("r/a", 1)})
			in := input()
			in.Shots = aws.Int64(shots)
			_, err := p.CreateQuantumTask(ctx, "us-west-1", "r/a", in)
			if i == 1 && (err == nil || !strings.Contains(err.Error(), "different input")) {
				t.Errorf("retry with different input: %v", err)
			}
		}
		if len(rc.Calls) != 1 {
			t.Errorf("%d calls; the changed retry must be refused before calling the service", len(rc.Calls))
		}
	})
	t.Run("region changed on retry", func(t *testing.T) {
		tokens := &gate.MemTokens{}
		g0, _ := newGate(t, gate.Options{Real: true, Creators: (&testutil.RecordingCreator{LoseResponse: 1}).Factory(), Tokens: tokens})
		p0, _ := g0.Approve([]gate.Estimate{est("r/a", 1)})
		_, _ = p0.CreateQuantumTask(ctx, "us-east-1", "r/a", input())
		g, _ := newGate(t, gate.Options{Real: true, Tokens: tokens})
		p, _ := g.Approve([]gate.Estimate{est("r/a", 1)})
		if _, err := p.CreateQuantumTask(ctx, "us-west-1", "r/a", input()); err == nil {
			t.Error("a key first used in another region was submitted again")
		}
	})
	t.Run("no creator for region", func(t *testing.T) {
		g, _ := newGate(t, gate.Options{Real: true, Creators: func(context.Context, string) (pbraket.Creator, error) { return nil, errors.New("no endpoint") }})
		p, _ := g.Approve([]gate.Estimate{est("r/a", 1)})
		if _, err := p.CreateQuantumTask(ctx, "us-west-1", "r/a", input()); err == nil {
			t.Error("missing creator not reported")
		}
	})
}

func TestMoney(t *testing.T) {
	for v, want := range map[float64]string{0.00375: "$0.00375", 0.3425: "$0.3425", 0.385: "$0.385", 2: "$2.00", 0.3: "$0.30", 2.425: "$2.425"} {
		if got := gate.Money(v); got != want {
			t.Errorf("Money(%v) = %q, want %q", v, got, want)
		}
	}
}

func TestFileTokens(t *testing.T) {
	ft := gate.FileTokens{Dir: filepath.Join(t.TempDir(), "tokens")}
	if _, ok, err := ft.Get("k"); ok || err != nil {
		t.Fatalf("empty store: %v %v", ok, err)
	}
	r := gate.TokenRecord{Key: "run/1", Region: "us-west-1", ClientToken: "abc"}
	if err := ft.Put(r); err != nil {
		t.Fatal(err)
	}
	r.TaskArn = "arn:x"
	if err := ft.Put(r); err != nil {
		t.Fatal(err)
	}
	got, ok, err := ft.Get("run/1")
	if err != nil || !ok || got != r {
		t.Fatalf("Get = %+v %v %v", got, ok, err)
	}
	if err := (gate.FileTokens{Dir: "/dev/null/x"}).Put(r); err == nil {
		t.Error("Put into an impossible dir succeeded")
	}
}

func TestDefaultTokenDir(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/s")
	if d, _ := gate.DefaultTokenDir(); d != filepath.Join("/s", "puffball", "tokens") {
		t.Errorf("XDG: %s", d)
	}
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "/h")
	if d, _ := gate.DefaultTokenDir(); d != filepath.Join("/h", ".local", "state", "puffball", "tokens") {
		t.Errorf("fallback: %s", d)
	}
}

// TestConcurrentRunsOfOneKeyCreateOnce: many processes (here goroutines with
// their own gates, sharing the on-disk store) submitting one key claim one
// client token, so the service, idempotent on it (#35), makes one task.
func TestConcurrentRunsOfOneKeyCreateOnce(t *testing.T) {
	dir := t.TempDir()
	rc := &testutil.RecordingCreator{}
	const n = 20
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			g, err := gate.New(gate.Options{Real: true, Out: io.Discard, Creators: rc.Factory(), Tokens: gate.FileTokens{Dir: dir}})
			if err != nil {
				errs <- err
				return
			}
			p, err := g.Approve([]gate.Estimate{est("run/a", 0.00375)})
			if err != nil {
				errs <- err
				return
			}
			_, err = p.CreateQuantumTask(context.Background(), "us-west-1", "run/a", input())
			if err != nil && !errors.Is(err, gate.ErrAlreadySubmitted) {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if rc.Created() != 1 {
		t.Fatalf("%d concurrent runs of one key created %d tasks, want 1", n, rc.Created())
	}
	tokens := map[string]bool{}
	for _, c := range rc.Calls {
		tokens[aws.ToString(c.ClientToken)] = true
	}
	if len(tokens) != 1 {
		t.Errorf("%d different client tokens sent for one key", len(tokens))
	}
}

func TestFileTokensClaim(t *testing.T) {
	ft := gate.FileTokens{Dir: t.TempDir()}
	first, err := ft.Claim(gate.TokenRecord{Key: "k", Region: "us-west-1", ClientToken: "one"})
	if err != nil || first.ClientToken != "one" {
		t.Fatal(first, err)
	}
	second, err := ft.Claim(gate.TokenRecord{Key: "k", Region: "us-west-1", ClientToken: "two"})
	if err != nil || second.ClientToken != "one" {
		t.Fatalf("second claim = %+v, %v; want the first token back", second, err)
	}
	entries, _ := os.ReadDir(ft.Dir)
	if len(entries) != 1 {
		t.Errorf("%d files after two claims, want 1 (no temp files left)", len(entries))
	}
	if _, err := (gate.FileTokens{Dir: "/dev/null/x"}).Claim(gate.TokenRecord{Key: "k"}); err == nil {
		t.Error("claim into an impossible dir succeeded")
	}
	m := &gate.MemTokens{}
	a, _ := m.Claim(gate.TokenRecord{Key: "k", ClientToken: "one"})
	b, _ := m.Claim(gate.TokenRecord{Key: "k", ClientToken: "two"})
	if a.ClientToken != "one" || b.ClientToken != "one" {
		t.Errorf("MemTokens.Claim: %v %v", a, b)
	}
	if r, ok, _ := m.Get("k"); !ok || r.ClientToken != "one" {
		t.Errorf("MemTokens.Get: %v %v", r, ok)
	}
	m.Err = errors.New("full")
	if _, err := m.Claim(gate.TokenRecord{Key: "j"}); err == nil {
		t.Error("MemTokens.Err not returned by Claim")
	}
}

// TestServiceErrorsAreScrubbed: SDK errors can name the results bucket or a
// principal (law 7); the cause stays reachable.
func TestServiceErrorsAreScrubbed(t *testing.T) {
	cause := errors.New("AccessDenied: User: arn:aws:iam::123456789012:user/alice cannot write amazon-braket-us-west-1-123456789012")
	rc := &testutil.RecordingCreator{Err: cause}
	g, _ := newGate(t, gate.Options{Real: true, Creators: rc.Factory()})
	p, _ := g.Approve([]gate.Estimate{est("r/a", 1)})
	_, err := p.CreateQuantumTask(context.Background(), "us-west-1", "r/a", input())
	if err == nil || strings.Contains(err.Error(), "123456789012") || strings.Contains(err.Error(), "alice") {
		t.Fatalf("err = %v; want it scrubbed", err)
	}
	if !errors.Is(err, cause) {
		t.Error("scrubbing lost the cause")
	}
}

func TestExperimentalCapabilitiesRefused(t *testing.T) {
	g, _ := newGate(t, gate.Options{Real: true})
	p, _ := g.Approve([]gate.Estimate{est("r/a", 1)})
	in := input()
	in.ExperimentalCapabilities = &types.ExperimentalCapabilitiesMemberEnabled{Value: types.ExperimentalCapabilitiesEnablementTypeAll}
	if _, err := p.CreateQuantumTask(context.Background(), "us-west-1", "r/a", in); err == nil {
		t.Error("experimental capabilities accepted")
	}
}

func TestInputHashNilAndEmptyAgree(t *testing.T) {
	tokens := &gate.MemTokens{}
	rc := &testutil.RecordingCreator{LoseResponse: 1}
	for i, tags := range []map[string]string{nil, {}} {
		g, _ := newGate(t, gate.Options{Real: true, Creators: rc.Factory(), Tokens: tokens})
		p, _ := g.Approve([]gate.Estimate{est("r/a", 1)})
		in := input()
		in.Tags = tags
		_, err := p.CreateQuantumTask(context.Background(), "us-west-1", "r/a", in)
		if i == 1 && err != nil {
			t.Errorf("retry with empty (not nil) tags refused: %v", err)
		}
	}
	if len(rc.Calls) != 2 || rc.Calls[1].Tags != nil {
		t.Errorf("empty tags were sent as %v; send nil so the wire request matches", rc.Calls[len(rc.Calls)-1].Tags)
	}
}

func TestClaimSweepsStaleTemps(t *testing.T) {
	ft := gate.FileTokens{Dir: t.TempDir()}
	stale := filepath.Join(ft.Dir, ".tok-crashed")
	fresh := filepath.Join(ft.Dir, ".tok-inflight")
	for _, p := range []string{stale, fresh} {
		if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * time.Hour)
	_ = os.Chtimes(stale, old, old)
	if _, err := ft.Claim(gate.TokenRecord{Key: "k", ClientToken: "t"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Error("stale temp file left")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("a recent temp file (another run in flight) was removed")
	}
}
