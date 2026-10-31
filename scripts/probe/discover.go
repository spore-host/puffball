package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/braket"
	"github.com/aws/aws-sdk-go-v2/service/braket/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

// regionResult is what SearchDevices returned in one region.
type regionResult struct {
	Region  string
	Devices []types.DeviceSummary
	Pages   [][]byte // raw response bodies, one per page
	Err     error
}

// discovery is the outcome of searching devices in every Braket region.
//
// SearchDevices is regional: it returns the devices of the region it is called
// in (measured in G0(a), #2), so the region set cannot come from one call. It
// comes from the public SSM global-infrastructure parameters for the braket
// service, read at run time, plus any region named in a returned device ARN.
type discovery struct {
	Seed       string
	SSMRegions []string
	SSMRaw     []byte
	Regions    map[string]*regionResult
}

// braketRegionsPath is the public SSM parameter path listing the regions the
// braket service is offered in.
const braketRegionsPath = "/aws/service/global-infrastructure/services/braket/regions"

// ssmRegions reads the Braket region list from the public SSM parameters.
func ssmRegions(ctx context.Context, seed string) ([]string, []byte, error) {
	cfg, rec, err := loadConfig(ctx, seed)
	if err != nil {
		return nil, nil, err
	}
	client := ssm.NewFromConfig(cfg)
	var regions []string
	var raw []byte
	p := ssm.NewGetParametersByPathPaginator(client, &ssm.GetParametersByPathInput{Path: aws.String(braketRegionsPath)})
	for p.HasMorePages() {
		out, err := p.NextPage(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("GetParametersByPath %s: %w", braketRegionsPath, err)
		}
		raw = append(raw, rec.Last()...)
		for _, prm := range out.Parameters {
			regions = append(regions, aws.ToString(prm.Value))
		}
	}
	sort.Strings(regions)
	return regions, raw, nil
}

// regionOf returns the region field of a Braket device ARN, or "" for the
// region-less simulator ARNs (arn:aws:braket:::device/quantum-simulator/...).
func regionOf(arn string) string {
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) < 4 {
		return ""
	}
	return parts[3]
}

// discover searches devices in every Braket region SSM lists, then in any
// further region named in a returned ARN, until no new region appears. Regions
// are searched in parallel.
func discover(ctx context.Context, seed string) (*discovery, error) {
	d := &discovery{Seed: seed, Regions: map[string]*regionResult{}}
	regions, raw, err := ssmRegions(ctx, seed)
	if err != nil {
		return nil, err
	}
	d.SSMRegions, d.SSMRaw = regions, raw
	wave := append([]string(nil), regions...)
	if !contains(wave, seed) {
		wave = append(wave, seed)
	}
	for len(wave) > 0 {
		results := make([]*regionResult, len(wave))
		var wg sync.WaitGroup
		for i, r := range wave {
			wg.Add(1)
			go func() {
				defer wg.Done()
				results[i] = searchRegion(ctx, r)
			}()
		}
		wg.Wait()
		var next []string
		for _, res := range results {
			d.Regions[res.Region] = res
		}
		for _, res := range results {
			for _, dev := range res.Devices {
				r := regionOf(aws.ToString(dev.DeviceArn))
				if r == "" || d.Regions[r] != nil || contains(next, r) {
					continue
				}
				next = append(next, r)
			}
		}
		sort.Strings(next)
		wave = next
	}
	return d, nil
}

func contains(xs []string, x string) bool {
	for _, s := range xs {
		if s == x {
			return true
		}
	}
	return false
}

func searchRegion(ctx context.Context, region string) *regionResult {
	res := &regionResult{Region: region}
	cfg, rec, err := loadConfig(ctx, region)
	if err != nil {
		res.Err = err
		return res
	}
	client := braket.NewFromConfig(cfg)
	p := braket.NewSearchDevicesPaginator(client, &braket.SearchDevicesInput{Filters: []types.SearchDevicesFilter{}})
	for p.HasMorePages() {
		out, err := p.NextPage(ctx)
		if err != nil {
			res.Err = fmt.Errorf("SearchDevices in %s: %w", region, err)
			return res
		}
		res.Pages = append(res.Pages, rec.Last())
		res.Devices = append(res.Devices, out.Devices...)
	}
	return res
}

// sortedRegions returns the discovered regions in order.
func (d *discovery) sortedRegions() []string {
	rs := make([]string, 0, len(d.Regions))
	for r := range d.Regions {
		rs = append(rs, r)
	}
	sort.Strings(rs)
	return rs
}

// deviceRegions returns the regions that host at least one device with a
// regional ARN (the QPUs), which is the set every command must cover.
func (d *discovery) deviceRegions() []string {
	set := map[string]bool{}
	for _, res := range d.Regions {
		for _, dev := range res.Devices {
			if r := regionOf(aws.ToString(dev.DeviceArn)); r != "" {
				set[r] = true
			}
		}
	}
	rs := make([]string, 0, len(set))
	for r := range set {
		rs = append(rs, r)
	}
	sort.Strings(rs)
	return rs
}

// find returns the first device whose name contains name (case-insensitive),
// with the region its ARN names.
func (d *discovery) find(name string) (types.DeviceSummary, bool) {
	for _, r := range d.sortedRegions() {
		for _, dev := range d.Regions[r].Devices {
			if strings.Contains(strings.ToLower(aws.ToString(dev.DeviceName)), strings.ToLower(name)) {
				return dev, true
			}
		}
	}
	return types.DeviceSummary{}, false
}
