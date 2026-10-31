package braket

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/braket"
	"github.com/aws/aws-sdk-go-v2/service/braket/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

// RegionsParameterPath is the public SSM path listing the regions the braket
// service is offered in. SearchDevices returns only the calling region's
// devices (G0(a), #2), so the region set has to come from here.
const RegionsParameterPath = "/aws/service/global-infrastructure/services/braket/regions"

// Inventory is every Braket device found, by region.
type Inventory struct {
	Seed       string
	SSMRegions []string                         // as listed in SSM
	Devices    map[string][]types.DeviceSummary // region → devices, successful regions only
	err        error
}

// Regions returns every region that was searched, sorted, including failed ones.
func (inv *Inventory) Regions() []string {
	set := map[string]bool{}
	for r := range inv.Devices {
		set[r] = true
	}
	if ie, ok := AsIncomplete(inv.err); ok {
		for r := range ie.Failed {
			set[r] = true
		}
	}
	return sortedKeys(set)
}

// Err is non-nil, an *IncompleteError, when any region's search failed.
func (inv *Inventory) Err() error { return inv.err }

// RegionOf returns the region field of a Braket ARN, or "" for the region-less
// simulator ARNs (arn:aws:braket:::device/quantum-simulator/...).
func RegionOf(arn string) string {
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) < 6 || parts[0] != "arn" {
		return ""
	}
	return parts[3]
}

// ListRegions reads the Braket region list from SSM via the seed region. A
// failure is an error: there is no silent fallback to the seed alone.
func ListRegions(ctx context.Context, f Factory) ([]string, error) {
	p, err := f.Parameters(ctx, f.Seed())
	if err != nil {
		return nil, fmt.Errorf("read Braket region list from SSM (%s): %w", f.Seed(), err)
	}
	var regions []string
	seen := map[string]bool{}
	pg := ssm.NewGetParametersByPathPaginator(p, &ssm.GetParametersByPathInput{Path: aws.String(RegionsParameterPath)})
	for pg.HasMorePages() {
		out, err := pg.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("read Braket region list from SSM (%s): %w", f.Seed(), err)
		}
		for _, prm := range out.Parameters {
			if v := aws.ToString(prm.Value); v != "" && !seen[v] {
				seen[v] = true
				regions = append(regions, v)
			}
		}
	}
	if len(regions) == 0 {
		return nil, fmt.Errorf("SSM %s lists no regions", RegionsParameterPath)
	}
	sort.Strings(regions)
	return regions, nil
}

// Discover searches devices in every Braket region SSM lists, then in any
// further region a returned device ARN names, until no new region appears.
//
// If the region list can't be read, it returns a nil Inventory and the error.
// If any region's search fails, it returns the Inventory of the regions that
// succeeded together with an *IncompleteError naming the rest (also available
// as [Inventory.Err]), so an `if err != nil` check never mistakes a partial
// inventory for a complete one.
func Discover(ctx context.Context, f Factory) (*Inventory, error) {
	regions, err := ListRegions(ctx, f)
	if err != nil {
		return nil, err
	}
	inv := &Inventory{Seed: f.Seed(), SSMRegions: regions, Devices: map[string][]types.DeviceSummary{}}
	failed := map[string]error{}
	seen := map[string]bool{}
	wave := regions
	for len(wave) > 0 {
		for _, r := range wave {
			seen[r] = true
		}
		results := FanOut(ctx, wave, func(ctx context.Context, region string) ([]types.DeviceSummary, error) {
			return searchDevices(ctx, f, region)
		})
		next := map[string]bool{}
		for _, res := range results {
			if res.Err != nil {
				failed[res.Region] = res.Err
				continue
			}
			inv.Devices[res.Region] = res.Value
			for _, d := range res.Value {
				if r := RegionOf(aws.ToString(d.DeviceArn)); r != "" && !seen[r] {
					next[r] = true
				}
			}
		}
		wave = sortedKeys(next)
	}
	if len(failed) > 0 {
		inv.err = &IncompleteError{Op: "SearchDevices", Failed: failed}
		return inv, inv.err
	}
	return inv, nil
}

// searchDevices pages SearchDevices in one region. An error on any page fails
// the whole region: a partial device list is never returned as a full one.
func searchDevices(ctx context.Context, f Factory, region string) ([]types.DeviceSummary, error) {
	c, err := f.Braket(ctx, region)
	if err != nil {
		return nil, err
	}
	var devs []types.DeviceSummary
	pg := braket.NewSearchDevicesPaginator(c, &braket.SearchDevicesInput{Filters: []types.SearchDevicesFilter{}})
	for pg.HasMorePages() {
		out, err := pg.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("SearchDevices: %w", err)
		}
		devs = append(devs, out.Devices...)
	}
	return devs, nil
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
