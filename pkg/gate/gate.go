// Package gate is the only place puff makes billable calls (law 1).
//
// A billable call needs a [Permit], and only [Gate.Approve] issues one. Approve
// prints every estimate's arithmetic with its price as-of date, then refuses
// unless --real was given, --dry-run was not, and the total is within --budget.
// A Permit can spend each approved line item once and nothing else.
//
// Billable operations: Braket CreateQuantumTask and CreateJob, and every Cost
// Explorer request. A static test (law1_test.go) fails if any of them is used
// outside this package.
package gate

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	braketsdk "github.com/aws/aws-sdk-go-v2/service/braket"

	"github.com/spore-host/puffball/internal/scrub"
	"github.com/spore-host/puffball/pkg/awscfg"
	"github.com/spore-host/puffball/pkg/braket"
)

// Estimate is one billable line item, priced before the call.
type Estimate struct {
	// Key identifies the call across retries, e.g. "<run-id>/<label>". The
	// client token is stored under it, so the same key never creates twice.
	Key string
	// Label is what the printout calls the item.
	Label string
	// Arithmetic is the pricing-page sum, e.g. "$0.30 + 100 x $0.000425 = $0.3425".
	Arithmetic string
	// USD is the estimate in dollars.
	USD float64
	// AsOf is the price table's as-of date (law 2). Required.
	AsOf string
}

func (e Estimate) validate() error {
	switch {
	case e.Key == "":
		return errors.New("estimate has no key")
	case e.AsOf == "":
		return fmt.Errorf("estimate %q has no price as-of date (law 2)", e.Label)
	case e.Arithmetic == "":
		return fmt.Errorf("estimate %q has no arithmetic", e.Label)
	case math.IsNaN(e.USD) || math.IsInf(e.USD, 0) || e.USD < 0:
		return fmt.Errorf("estimate %q is %v; an unknown price is an error, never a guess (law 2)", e.Label, e.USD)
	}
	return nil
}

// Options configure a Gate.
type Options struct {
	Real   bool // --real
	DryRun bool // --dry-run: print and stop even with --real
	// Budget is --budget in USD; nil means none was given. A budget of 0
	// refuses any plan with a non-zero estimate.
	Budget *float64
	// Out receives the arithmetic printout; stderr in puff, so --json
	// output on stdout stays clean. Required: a gate that prints nothing
	// would hide the arithmetic law 1 requires.
	Out io.Writer
	// Creators returns the billable Braket client for a region. Nil means
	// the real AWS clients; tests pass a fake.
	Creators func(ctx context.Context, region string) (braket.Creator, error)
	// Tokens stores client tokens across runs.
	Tokens TokenStore
}

// Gate decides whether billable calls may happen and makes them.
type Gate struct{ o Options }

// New returns a Gate. Out and Tokens are required.
func New(o Options) (*Gate, error) {
	if o.Out == nil || o.Tokens == nil {
		return nil, errors.New("gate: Out and Tokens are required")
	}
	if b := o.Budget; b != nil && (*b < 0 || math.IsNaN(*b) || math.IsInf(*b, 0)) {
		return nil, fmt.Errorf("--budget %v must be a non-negative number of dollars", *b)
	}
	if o.Creators == nil {
		o.Creators = awsCreators()
	}
	return &Gate{o: o}, nil
}

// ErrOverBudget is returned (wrapped) when the plan's total is above --budget.
var ErrOverBudget = errors.New("over budget")

// Approve prints the plan and returns a Permit, or nil and no error when the
// run stops by design (no --real, or --dry-run). A plan over --budget is an
// error wrapping [ErrOverBudget].
func (g *Gate) Approve(plan []Estimate) (*Permit, error) {
	if len(plan) == 0 {
		return nil, errors.New("gate: empty plan")
	}
	items := map[string]Estimate{}
	total := 0.0
	dates := map[string]bool{}
	for _, e := range plan {
		if err := e.validate(); err != nil {
			return nil, err
		}
		if _, dup := items[e.Key]; dup {
			return nil, fmt.Errorf("gate: plan has key %q twice", e.Key)
		}
		items[e.Key] = e
		total += e.USD
		dates[e.AsOf] = true
	}
	asOf := make([]string, 0, len(dates))
	for d := range dates {
		asOf = append(asOf, d)
	}
	sort.Strings(asOf)
	w := g.o.Out
	for _, e := range plan {
		_, _ = fmt.Fprintf(w, "estimate (prices as of %s): %s: %s\n", e.AsOf, e.Label, e.Arithmetic)
	}
	if len(plan) > 1 {
		_, _ = fmt.Fprintf(w, "estimate (prices as of %s): total %s\n", strings.Join(asOf, ", "), Money(total))
	}
	if b := g.o.Budget; b != nil && total > *b+1e-9 {
		return nil, fmt.Errorf("estimate %s exceeds --budget %s; not submitted: %w", Money(total), Money(*b), ErrOverBudget)
	}
	switch {
	case g.o.DryRun:
		_, _ = fmt.Fprintln(w, "Dry run: not submitted.")
		return nil, nil
	case !g.o.Real:
		_, _ = fmt.Fprintln(w, "Not submitted. Pass --real to spend this.")
		return nil, nil
	}
	return &Permit{g: g, items: items}, nil
}

// Permit spends approved line items, each at most once.
type Permit struct {
	g     *Gate
	mu    sync.Mutex
	items map[string]Estimate // approved and not yet spent
}

// ErrAlreadySubmitted means the key already created a task in an earlier run;
// the Submission carries its ARN and nothing new was created.
var ErrAlreadySubmitted = errors.New("already submitted")

// Submission is the outcome of a create.
type Submission struct {
	Key, Region, TaskArn, ClientToken string
}

func (p *Permit) take(key string) (Estimate, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.items[key]
	if !ok {
		return Estimate{}, fmt.Errorf("gate: %q was not approved, or was already spent under this permit", key)
	}
	delete(p.items, key)
	return e, nil
}

// CreateQuantumTask creates the task for an approved key in region.
//
// The key's client token is claimed atomically before the call (the first
// claim wins; every later run of the key, in this process or another, gets the
// same token), and the service is idempotent on it (measured in #35), so any
// number of runs or retries of a key create at most one task. A key whose
// task is already recorded returns that Submission and ErrAlreadySubmitted
// without calling the service; a key first used with other input or in
// another region is refused before calling.
func (p *Permit) CreateQuantumTask(ctx context.Context, region, key string, in braketsdk.CreateQuantumTaskInput) (Submission, error) {
	e, err := p.take(key)
	if err != nil {
		return Submission{}, err
	}
	// Send nil, not empty: a retry that built its tags as {} must be the same
	// request on the wire, or the service answers 409 to its own token.
	if len(in.Tags) == 0 {
		in.Tags = nil
	}
	if len(in.Associations) == 0 {
		in.Associations = nil
	}
	hash, err := inputHash(in)
	if err != nil {
		return Submission{}, err
	}
	tok, err := newToken()
	if err != nil {
		return Submission{}, err
	}
	rec, err := p.g.o.Tokens.Claim(TokenRecord{Key: key, Region: region, ClientToken: tok, InputHash: hash})
	if err != nil {
		return Submission{}, fmt.Errorf("save client token before submitting: %w", err)
	}
	if rec.InputHash != hash {
		return Submission{}, fmt.Errorf("gate: key %q was first submitted with different input; a client token can't be reused for a different request (the service answers 409 ConflictException)", key)
	}
	if rec.Region != region {
		return Submission{}, fmt.Errorf("gate: key %q was first submitted in %s, not %s", key, rec.Region, region)
	}
	if rec.TaskArn != "" {
		return Submission{Key: key, Region: rec.Region, TaskArn: rec.TaskArn, ClientToken: rec.ClientToken}, ErrAlreadySubmitted
	}
	c, err := p.g.o.Creators(ctx, region)
	if err != nil {
		return Submission{}, scrub.Error(err)
	}
	in.ClientToken = aws.String(rec.ClientToken)
	_, _ = fmt.Fprintf(p.g.o.Out, "submitting %s: estimate %s (prices as of %s)\n", e.Label, Money(e.USD), e.AsOf)
	out, err := c.CreateQuantumTask(ctx, &in)
	if err != nil {
		// SDK errors can name the results bucket or a principal (law 7).
		return Submission{}, scrub.Error(fmt.Errorf("CreateQuantumTask %s: %w (retry the same key to reuse its client token)", e.Label, err))
	}
	arn := aws.ToString(out.QuantumTaskArn)
	if arn == "" {
		return Submission{}, fmt.Errorf("CreateQuantumTask %s returned no task ARN; retry the same key, which reuses its client token", e.Label)
	}
	rec.TaskArn = arn
	if err := p.g.o.Tokens.Put(rec); err != nil {
		return Submission{}, scrub.Error(fmt.Errorf("task %s created but not recorded (retry the same key: it reuses the token and gets this task back): %w", arn, err))
	}
	return Submission{Key: key, Region: region, TaskArn: arn, ClientToken: rec.ClientToken}, nil
}

// hashedInput is the part of a request that identifies it, in a form puffball
// owns, so an SDK upgrade that adds fields doesn't change every stored hash.
type hashedInput struct {
	Action, DeviceArn, DeviceParameters string
	OutputS3Bucket, OutputS3KeyPrefix   string
	JobToken                            string
	Shots                               int64
	Tags                                map[string]string // encoding/json sorts map keys
	Associations                        []string
}

// inputHash identifies a request apart from its client token.
func inputHash(in braketsdk.CreateQuantumTaskInput) (string, error) {
	if in.ExperimentalCapabilities != nil {
		return "", errors.New("gate: experimental capabilities are not supported")
	}
	h := hashedInput{
		Action: aws.ToString(in.Action), DeviceArn: aws.ToString(in.DeviceArn), DeviceParameters: aws.ToString(in.DeviceParameters),
		OutputS3Bucket: aws.ToString(in.OutputS3Bucket), OutputS3KeyPrefix: aws.ToString(in.OutputS3KeyPrefix),
		JobToken: aws.ToString(in.JobToken), Shots: aws.ToInt64(in.Shots),
	}
	if len(in.Tags) > 0 {
		h.Tags = in.Tags
	}
	for _, a := range in.Associations {
		h.Associations = append(h.Associations, string(a.Type)+" "+aws.ToString(a.Arn))
	}
	b, err := json.Marshal(h)
	if err != nil {
		return "", fmt.Errorf("hash request: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func newToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("client token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// Money formats dollars with at least two and at most five decimals:
// $0.30, $0.3425, $0.00375.
func Money(v float64) string {
	s := strings.TrimRight(fmt.Sprintf("%.5f", v), "0")
	if i := strings.IndexByte(s, '.'); len(s)-i-1 < 2 {
		s += strings.Repeat("0", 2-(len(s)-i-1))
	}
	return "$" + s
}

// awsCreators returns billable Braket clients from the shared spore.host
// config, one per region. Unexported: the only way to a real billable client
// is through a Gate.
func awsCreators() func(ctx context.Context, region string) (braket.Creator, error) {
	var mu sync.Mutex
	clients := map[string]braket.Creator{}
	return func(ctx context.Context, region string) (braket.Creator, error) {
		mu.Lock()
		defer mu.Unlock()
		if c, ok := clients[region]; ok {
			return c, nil
		}
		cfg, err := awscfg.Load(ctx, region)
		if err != nil {
			return nil, err
		}
		c := braketsdk.NewFromConfig(cfg)
		clients[region] = c
		return c, nil
	}
}
