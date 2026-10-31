package braket

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/spore-host/puffball/internal/scrub"
)

// RegionResult is one region's outcome of a fanned-out call. Value is only
// meaningful when Err is nil.
type RegionResult[T any] struct {
	Region string
	Value  T
	Err    error
}

// FanOut runs fn once per region, in parallel, and returns one result per
// region sorted by region. A region whose context is already done when its
// turn comes gets ctx.Err(), so a cancelled run is reported, never empty.
func FanOut[T any](ctx context.Context, regions []string, fn func(ctx context.Context, region string) (T, error)) []RegionResult[T] {
	out := make([]RegionResult[T], len(regions))
	var wg sync.WaitGroup
	for i, r := range regions {
		out[i].Region = r
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := ctx.Err(); err != nil {
				out[i].Err = err
				return
			}
			v, err := fn(ctx, r)
			if err == nil {
				err = ctx.Err() // finished, but the run was cancelled meanwhile
			}
			out[i].Value, out[i].Err = v, err
		}()
	}
	wg.Wait()
	sort.Slice(out, func(a, b int) bool { return out[a].Region < out[b].Region })
	return out
}

// IncompleteError reports that an operation did not cover every region (law
// 3). Callers print it and exit 1; they never present the rest as complete.
type IncompleteError struct {
	Op     string
	Failed map[string]error // region → why
}

func (e *IncompleteError) Error() string {
	regions := e.Regions()
	parts := make([]string, len(regions))
	for i, r := range regions {
		parts[i] = fmt.Sprintf("%s (%v)", r, e.Failed[r])
	}
	msg := fmt.Sprintf("incomplete: %s failed in %d region(s): %s", e.Op, len(regions), strings.Join(parts, "; "))
	// SDK errors embed principal ARNs and account IDs (AccessDenied, for
	// one); this text is printed and may be posted (law 7).
	return string(scrub.Scrub([]byte(msg)))
}

// Regions returns the failed regions, sorted.
func (e *IncompleteError) Regions() []string {
	rs := make([]string, 0, len(e.Failed))
	for r := range e.Failed {
		rs = append(rs, r)
	}
	sort.Strings(rs)
	return rs
}

// Unwrap returns the per-region errors, so errors.Is(err, context.Canceled)
// works through it.
func (e *IncompleteError) Unwrap() []error {
	errs := make([]error, 0, len(e.Failed))
	for _, r := range e.Regions() {
		errs = append(errs, e.Failed[r])
	}
	return errs
}

// Incomplete returns an *IncompleteError naming every failed region in
// results, or nil if all succeeded.
func Incomplete[T any](op string, results []RegionResult[T]) error {
	failed := map[string]error{}
	for _, r := range results {
		if r.Err != nil {
			failed[r.Region] = r.Err
		}
	}
	if len(failed) == 0 {
		return nil
	}
	return &IncompleteError{Op: op, Failed: failed}
}

// AsIncomplete reports whether err is or wraps an *IncompleteError.
func AsIncomplete(err error) (*IncompleteError, bool) {
	var ie *IncompleteError
	ok := errors.As(err, &ie)
	return ie, ok
}
