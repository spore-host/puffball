package cmd

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/braket/types"
	"github.com/aws/aws-sdk-go-v2/service/pricing"
	"github.com/spf13/cobra"

	"github.com/spore-host/puffball/internal/scrub"
	"github.com/spore-host/puffball/pkg/awscfg"
	pbraket "github.com/spore-host/puffball/pkg/braket"
	"github.com/spore-host/puffball/pkg/gate"
	"github.com/spore-host/puffball/pkg/local"
	"github.com/spore-host/puffball/pkg/price"
	"github.com/spore-host/puffball/pkg/results"
)

// newPricing is the Price List client; tests replace it.
var newPricing = func(ctx context.Context) (price.ProductsAPI, error) {
	cfg, err := awscfg.Load(ctx, price.ListRegion)
	if err != nil {
		return nil, err
	}
	return pricing.NewFromConfig(cfg), nil
}

// uvCached checks uv's cache for the pinned simulator, and findUV finds uv;
// tests replace them.
var (
	uvCached = local.Cached
	findUV   = local.FindUV
)

// Doctor check statuses. fail makes the command exit 1.
const (
	statusOK      = "ok"
	statusWarn    = "warn"
	statusFail    = "fail"
	statusUnknown = "unknown"
	statusSkipped = "skipped"
)

// check is one doctor finding.
type check struct {
	Check  string `json:"check"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

type doctor struct {
	cmd    *cobra.Command
	checks []check
}

func (d *doctor) add(name, status, detail, fix string) {
	d.checks = append(d.checks, check{Check: name, Status: status, Detail: display(detail), Fix: display(fix)})
}

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check that puff can run: credentials, regions, buckets, uv, prices, device agreement",
	Long: `Check what puff needs, and say how to fix what's missing:
  - AWS credentials resolve;
  - every Braket region answers;
  - each region's default results bucket exists (reported, never created);
  - the client-token directory is writable;
  - uv is installed and the pinned simulator is in uv's cache, so a local
    run works offline;
  - the price table is current and matches the AWS Price List;
  - the third-party device user agreement, per QPU region, as far as service
    state shows it: accepted when Braket has accepted a QPU task in the region
    (the newest in 90 days, else any), unknown otherwise. This assumes the
    agreement covers every QPU provider in a region, which hasn't been
    measured; the detail names the device whose task is the evidence.

On AWS it only reads: STS, Braket searches, S3 HeadBucket, and the Price List
API (none billed). It calls no Cost Explorer and changes nothing in the
account. Locally it creates the client-token directory if it is missing.
Exits 1 if a check fails, including a region or the Price List that can't
be read.`,
	Args: usageArgs(cobra.NoArgs),
	RunE: func(cmd *cobra.Command, args []string) error {
		d := &doctor{cmd: cmd}
		ctx := ctxOf(cmd)
		d.local(ctx)
		d.prices(ctx)
		d.aws(ctx)
		if jsonOutput {
			if err := writeJSON(cmd.OutOrStdout(), map[string]any{"checks": d.checks, "ok": !d.failed()}); err != nil {
				return err
			}
		} else {
			writeChecks(cmd.OutOrStdout(), d.checks)
		}
		if d.failed() {
			return fmt.Errorf("puff doctor: %d check(s) failed", d.count(statusFail))
		}
		return nil
	},
}

func (d *doctor) count(status string) int {
	n := 0
	for _, c := range d.checks {
		if c.Status == status {
			n++
		}
	}
	return n
}

func (d *doctor) failed() bool { return d.count(statusFail) > 0 }

func (d *doctor) local(ctx context.Context) {
	if dir, err := gate.DefaultTokenDir(); err != nil {
		d.add("client tokens", statusFail, err.Error(), "set XDG_STATE_HOME to a writable directory")
	} else if err := (gate.FileTokens{Dir: dir}).Check(); err != nil {
		d.add("client tokens", statusFail, err.Error(), "puff run --real can't record its client token there; set XDG_STATE_HOME to a writable directory, or fix the directory's owner")
	} else {
		d.add("client tokens", statusOK, dir+" is writable", "")
	}
	if _, err := findUV(local.Options{}); err != nil {
		d.add("uv", statusFail, err.Error(), "install uv: https://docs.astral.sh/uv/getting-started/installation/")
		return
	}
	v, err := uvCached(ctx, local.Options{})
	if err != nil {
		d.add("local simulator", statusWarn, err.Error(), "run `puff run <file.qasm>` once with the network; uv caches the simulator, and later local runs (and --offline) work without it")
		return
	}
	d.add("local simulator", statusOK, fmt.Sprintf("%s %s for Python %s is in uv's cache", local.SimulatorPackage, v, local.PythonVersion), "")
}

func (d *doctor) prices(ctx context.Context) {
	tb, err := price.Load()
	if err != nil {
		d.add("price table", statusFail, err.Error(), "")
		return
	}
	if tb.Stale(clockNow()) {
		d.add("price table", statusWarn, tb.StaleWarning(clockNow()), "update pkg/price/prices.json from the Braket pricing page (file an issue)")
	} else {
		d.add("price table", statusOK, fmt.Sprintf("as of %s, %d devices", tb.AsOf, len(tb.Devices)), "")
	}
	// An unreadable Price List fails: a scheduled doctor must not pass
	// having checked nothing.
	c, err := newPricing(ctx)
	if err != nil {
		d.add("price drift", statusFail, "Price List client: "+scrub.Error(err).Error(), "check AWS credentials")
		return
	}
	products, err := price.FetchProducts(ctx, c)
	if err != nil {
		d.add("price drift", statusFail, scrub.Error(err).Error(), "the drift check needs pricing:GetProducts in "+price.ListRegion+" (the Price List API isn't billed)")
		return
	}
	diffs, err := tb.Drift(products)
	if err != nil {
		d.add("price drift", statusFail, err.Error(), "")
		return
	}
	if len(diffs) == 0 {
		d.add("price drift", statusOK, fmt.Sprintf("the table matches the Price List (%d products)", len(products)), "")
		return
	}
	var lines []string
	for _, x := range diffs {
		lines = append(lines, x.String())
	}
	d.add("price drift", statusFail, strings.Join(lines, "; "), "estimates may be wrong: update pkg/price/prices.json and its as-of date (file an issue)")
}

func (d *doctor) aws(ctx context.Context) {
	rc := awscfg.Resolved()
	where := fmt.Sprintf("profile %s, region %s", dash(rc.Profile), dash(rc.Region))
	acct, err := newResults().Account(ctx)
	if err != nil {
		d.add("credentials", statusFail, where+": "+scrub.Error(err).Error(), "log in (for SSO: aws sso login --profile <name>), or set --profile / AWS_PROFILE")
		d.add("regions", statusSkipped, "needs credentials", "")
		return
	}
	d.add("credentials", statusOK, where+", account <account>", "")
	f, err := newFactory(ctx)
	if err != nil {
		d.add("regions", statusFail, scrub.Error(err).Error(), "")
		return
	}
	inv, derr := pbraket.Discover(ctx, f)
	if inv == nil {
		d.add("regions", statusFail, scrub.Error(derr).Error(), "the Braket region list comes from SSM /aws/service/global-infrastructure/services/braket/regions; check network access and ssm:GetParametersByPath")
		return
	}
	failed := failedRegions(derr)
	for _, r := range inv.Regions() {
		if e, ok := failed[r]; ok {
			d.add("region "+r, statusFail, e, "check braket:SearchDevices in "+r)
			continue
		}
		d.add("region "+r, statusOK, fmt.Sprintf("%d devices", len(inv.Devices[r])), "")
	}
	store := newResults()
	for _, r := range inv.Regions() {
		if _, ok := failed[r]; ok {
			d.add("results bucket "+r, statusSkipped, "the region couldn't be searched", "")
			continue
		}
		ok, err := store.Exists(ctx, r, results.BucketName(r, acct), acct)
		switch {
		case err != nil:
			d.add("results bucket "+r, statusFail, results.Display(r)+": "+scrub.Error(err).Error(), "check s3:ListBucket on it")
		case ok:
			d.add("results bucket "+r, statusOK, results.Display(r)+" exists", "")
		default:
			d.add("results bucket "+r, statusWarn, results.Display(r)+" doesn't exist", "puff creates it on the first --real run in "+r+" and says so; nothing to do unless you want it made first")
		}
	}
	d.agreement(ctx, f, inv, failed)
}

// agreement reports the third-party device user agreement per region with
// QPUs, from service state: the agreement can't be read through the API,
// and the call that reveals it (CreateQuantumTask) is billable. A task on a
// QPU in a region means Braket accepted a create there under this account,
// so the agreement was in force; without one it is unknown (#27).
func (d *doctor) agreement(ctx context.Context, f pbraket.Factory, inv *pbraket.Inventory, failed map[string]string) {
	var regions []string
	for r, ds := range inv.Devices {
		for _, s := range ds {
			if strings.Contains(aws.ToString(s.DeviceArn), "::device/qpu/") && s.DeviceStatus != types.DeviceStatusRetired {
				regions = append(regions, r)
				break
			}
		}
	}
	for r := range failed {
		d.add("device agreement "+r, statusSkipped, "the region couldn't be searched", "")
	}
	sort.Strings(regions)
	found, err := pbraket.LatestQPUTask(ctx, f, regions, clockNow().AddDate(0, 0, -90))
	searchFailed := failedRegions(err)
	for _, r := range regions {
		name := "device agreement " + r
		if e, ok := searchFailed[r]; ok {
			d.add(name, statusFail, "task search: "+e, "check braket:SearchQuantumTasks in "+r)
			continue
		}
		t := found[r]
		if t == nil {
			d.add(name, statusUnknown, "no QPU task in "+r+" yet, so service state doesn't show whether the third-party device user agreement is accepted",
				"if it isn't, the first `puff run --real` on a QPU here is refused (HTTP 403, nothing created) and puff names it; accept it on the Braket console, Permissions and settings > General, in "+r)
			continue
		}
		dev := aws.ToString(t.Summary.DeviceArn)
		d.add(name, statusOK, fmt.Sprintf("accepted: Braket accepted task %s on %s (%s)", taskID(aws.ToString(t.Summary.QuantumTaskArn)), dev[strings.LastIndex(dev, "/")+1:], when(t.Summary.CreatedAt)), "")
	}
}

func writeChecks(w io.Writer, cs []check) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "CHECK\tSTATUS\tDETAIL")
	for _, c := range cs {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", c.Check, c.Status, c.Detail)
	}
	_ = tw.Flush()
	first := true
	for _, c := range cs {
		if c.Fix == "" {
			continue
		}
		if first {
			_, _ = fmt.Fprintln(w, "\nTo fix:")
			first = false
		}
		_, _ = fmt.Fprintf(w, "- %s: %s\n", c.Check, c.Fix)
	}
}

func init() { rootCmd.AddCommand(doctorCmd) }
