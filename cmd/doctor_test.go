package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/pricing"

	pbraket "github.com/spore-host/puffball/pkg/braket"
	"github.com/spore-host/puffball/pkg/local"
	"github.com/spore-host/puffball/pkg/price"
	"github.com/spore-host/puffball/pkg/results"
	"github.com/spore-host/puffball/pkg/testutil"
)

// g0PriceList serves the Price List pages captured in G0(b) (#3); mut can
// change the entries.
type g0PriceList struct {
	pages [][]string
	err   error
}

func loadPriceList(t *testing.T, mut func(string) string) *g0PriceList {
	t.Helper()
	files, _ := filepath.Glob("../testdata/pricing/get-products.*.json")
	sort.Strings(files)
	pl := &g0PriceList{}
	for _, f := range files {
		b, err := os.ReadFile(f) //nolint:gosec // fixtures
		if err != nil {
			t.Fatal(err)
		}
		var page struct{ PriceList []string }
		if err := json.Unmarshal(b, &page); err != nil {
			t.Fatal(err)
		}
		if mut != nil {
			for i := range page.PriceList {
				page.PriceList[i] = mut(page.PriceList[i])
			}
		}
		pl.pages = append(pl.pages, page.PriceList)
	}
	return pl
}

func (p *g0PriceList) GetProducts(_ context.Context, in *pricing.GetProductsInput, _ ...func(*pricing.Options)) (*pricing.GetProductsOutput, error) {
	if p.err != nil {
		return nil, p.err
	}
	i := 0
	if in.NextToken != nil {
		i, _ = strconv.Atoi(*in.NextToken)
	}
	out := &pricing.GetProductsOutput{PriceList: p.pages[i]}
	if i+1 < len(p.pages) {
		out.NextToken = aws.String(strconv.Itoa(i + 1))
	}
	return out, nil
}

type doctorFakes struct {
	f       *testutil.FakeFactory
	res     *fakeResults
	prices  *g0PriceList
	cached  error
	credErr error
}

type credResults struct {
	*fakeResults
	err error
}

func (c credResults) Account(ctx context.Context) (string, error) {
	if c.err != nil {
		return "", c.err
	}
	return c.fakeResults.Account(ctx)
}

func doctorHarness(t *testing.T, mut func(*doctorFakes)) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	d := &doctorFakes{f: testutil.TasksFactory(t, "../testdata"), res: &fakeResults{exists: true}, prices: loadPriceList(t, nil)}
	if mut != nil {
		mut(d)
	}
	origF, origR, origP, origU := newFactory, newResults, newPricing, uvCached
	t.Cleanup(func() { newFactory, newResults, newPricing, uvCached = origF, origR, origP, origU })
	newFactory = func(context.Context) (pbraket.Factory, error) { return d.f, nil }
	newResults = func() results.Store { return credResults{d.res, d.credErr} }
	newPricing = func(context.Context) (price.ProductsAPI, error) { return d.prices, nil }
	uvCached = func(context.Context, local.Options) (string, error) {
		if d.cached != nil {
			return "", d.cached
		}
		return local.SimulatorVersion, nil
	}
	fixedNow(t, mustTime("2026-11-02T00:00:00Z"))
}

func doctorChecks(t *testing.T, out string) map[string]check {
	t.Helper()
	var got struct {
		Checks []check `json:"checks"`
		OK     bool    `json:"ok"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	m := map[string]check{}
	for _, c := range got.Checks {
		m[c.Check] = c
	}
	return m
}

func TestDoctorAllGood(t *testing.T) {
	needUV(t)
	doctorHarness(t, nil)
	out, errs, code := runPuff(t, "doctor", "--json")
	if code != exitOK {
		t.Fatalf("exit %d:\n%s\n%s", code, out, errs)
	}
	m := doctorChecks(t, out)
	for _, name := range []string{"client tokens", "local simulator", "price table", "price drift", "credentials", "region us-west-1", "results bucket us-west-1", "device agreement us-west-1"} {
		if m[name].Status != statusOK {
			t.Errorf("%s: %+v", name, m[name])
		}
	}
	if c := m["device agreement us-west-1"]; !strings.Contains(c.Detail, "on Cepheus-1-108Q") {
		t.Errorf("agreement evidence: %+v", c)
	}
	if c := m["device agreement eu-north-1"]; c.Status != statusUnknown || !strings.Contains(c.Fix, "Permissions and settings > General, in eu-north-1") {
		t.Errorf("unknown agreement: %+v", c)
	}
	noAccount(t, out, errs)
}

func TestDoctorFindings(t *testing.T) {
	needUV(t)
	cases := []struct {
		name, check, status, want string
		code                      int
		mut                       func(*doctorFakes)
	}{
		{"bucket missing", "results bucket us-west-1", statusWarn, "doesn't exist", exitOK, func(d *doctorFakes) { d.res.exists = false }},
		{"simulator not cached", "local simulator", statusWarn, "isn't in uv's cache", exitOK, func(d *doctorFakes) { d.cached = errors.New("isn't in uv's cache") }},
		{"price drift", "price drift", statusFail, "Price List", exitError, func(d *doctorFakes) {
			d.prices = loadPriceList(t, func(s string) string { return strings.Replace(s, `"USD":"0.0004250000"`, `"USD":"0.0005000000"`, 1) })
		}},
		{"price list unreadable", "price drift", statusUnknown, "AccessDenied", exitOK, func(d *doctorFakes) { d.prices.err = errors.New("AccessDenied for " + fakeAccount) }},
		{"no credentials", "credentials", statusFail, "no valid credential", exitError, func(d *doctorFakes) { d.credErr = errors.New("no valid credential sources for " + fakeAccount) }},
		{"region down", "region eu-north-1", statusFail, "injected", exitError, func(d *doctorFakes) { d.f.Regions["eu-north-1"].Err = errors.New("injected failure") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doctorHarness(t, tc.mut)
			out, errs, code := runPuff(t, "doctor", "--json")
			c := doctorChecks(t, out)[tc.check]
			if code != tc.code || c.Status != tc.status || !strings.Contains(c.Detail, tc.want) {
				t.Errorf("exit %d (want %d), %s = %+v\n%s", code, tc.code, tc.check, c, errs)
			}
			noAccount(t, out, errs)
		})
	}
}

func TestDoctorTokenDirUnwritable(t *testing.T) {
	needUV(t)
	doctorHarness(t, nil)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	t.Setenv("XDG_STATE_HOME", dir)
	out, errs, code := runPuff(t, "doctor")
	if code != exitError || !strings.Contains(out, "client tokens") || !strings.Contains(out, "XDG_STATE_HOME") {
		t.Errorf("exit %d:\n%s\n%s", code, out, errs)
	}
}

func TestDoctorHuman(t *testing.T) {
	needUV(t)
	doctorHarness(t, func(d *doctorFakes) { d.res.exists = false })
	out, errs, code := runPuff(t, "doctor")
	if code != exitOK || !strings.Contains(out, "CHECK") || !strings.Contains(out, "To fix:") || !strings.Contains(out, "puff creates it on the first --real run") {
		t.Errorf("exit %d:\n%s\n%s", code, out, errs)
	}
}

// needUV skips when uv isn't installed: doctor's uv check then fails, which
// is right, but the other checks are what these tests are about.
func needUV(t *testing.T) {
	t.Helper()
	if _, err := local.FindUV(local.Options{}); err != nil {
		t.Skip("uv not installed")
	}
}
