package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/braket"
)

// deviceFile is the fixture file name for a device: provider-name, lowercased.
func deviceFile(provider, name string) string {
	clean := func(s string) string {
		s = strings.ToLower(s)
		return strings.Map(func(r rune) rune {
			if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
				return r
			}
			return '-'
		}, s)
	}
	return clean(provider) + "_" + clean(name)
}

// runDevices is G0(a) (#2): SearchDevices in every region and GetDevice for
// every device, saved raw. Read-only.
func runDevices(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("devices", flag.ContinueOnError)
	out := fs.String("out", ".probe/devices", "output directory (raw, unscrubbed)")
	if err := fs.Parse(args); err != nil {
		return usageErr{err.Error()}
	}
	seed, err := seedRegion(ctx)
	if err != nil {
		return err
	}
	d, err := discover(ctx, seed)
	if err != nil {
		return err
	}
	if err := writeRaw(filepath.Join(*out, "ssm-braket-regions.json"), d.SSMRaw); err != nil {
		return err
	}

	type devRow struct {
		Region, Name, Provider, Type, Status, Arn, File string
	}
	summary := struct {
		Seed          string              `json:"seed_region"`
		SSMRegions    []string            `json:"ssm_braket_regions"`
		Searched      []string            `json:"regions_searched"`
		DeviceRegions []string            `json:"device_regions"`
		Devices       []devRow            `json:"devices"`
		Errors        map[string]string   `json:"errors,omitempty"`
		PerRegion     map[string][]string `json:"devices_returned_per_region"`
	}{Seed: seed, SSMRegions: d.SSMRegions, Errors: map[string]string{}, PerRegion: map[string][]string{}}

	var failed []string
	for _, r := range d.sortedRegions() {
		res := d.Regions[r]
		summary.Searched = append(summary.Searched, r)
		if res.Err != nil {
			summary.Errors[r] = scrubbed(res.Err)
			failed = append(failed, r)
			continue
		}
		for i, page := range res.Pages {
			if err := writeRaw(filepath.Join(*out, r, fmt.Sprintf("search-devices.%d.json", i)), page); err != nil {
				return err
			}
		}
		for _, dev := range res.Devices {
			summary.PerRegion[r] = append(summary.PerRegion[r], aws.ToString(dev.DeviceArn))
		}
	}
	summary.DeviceRegions = d.deviceRegions()

	// GetDevice: a regional device in its own region; a region-less simulator in
	// every region that returned it.
	type job struct{ region, arn, provider, name, typ, status string }
	var jobs []job
	for _, r := range d.sortedRegions() {
		res := d.Regions[r]
		for _, dev := range res.Devices {
			arn := aws.ToString(dev.DeviceArn)
			if dr := regionOf(arn); dr != "" && dr != r {
				continue
			}
			jobs = append(jobs, job{r, arn, aws.ToString(dev.ProviderName), aws.ToString(dev.DeviceName), string(dev.DeviceType), string(dev.DeviceStatus)})
		}
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	byRegion := map[string][]job{}
	for _, j := range jobs {
		byRegion[j.region] = append(byRegion[j.region], j)
	}
	for r, js := range byRegion {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cfg, rec, err := loadConfig(ctx, r)
			if err != nil {
				mu.Lock()
				summary.Errors[r] = scrubbed(err)
				mu.Unlock()
				return
			}
			client := braket.NewFromConfig(cfg)
			for _, j := range js {
				file := deviceFile(j.provider, j.name)
				out1, err := client.GetDevice(ctx, &braket.GetDeviceInput{DeviceArn: aws.String(j.arn)})
				mu.Lock()
				if err != nil {
					summary.Errors[r+" "+j.name] = "GetDevice: " + scrubbed(err)
					mu.Unlock()
					continue
				}
				mu.Unlock()
				if err := writeRaw(filepath.Join(*out, r, file+".json"), rec.Last()); err != nil {
					fmt.Fprintln(os.Stderr, scrubbed(err))
				}
				// deviceCapabilities arrives as a JSON string; save it parsed as
				// well, since it is what the capability parser reads.
				caps := aws.ToString(out1.DeviceCapabilities)
				if json.Valid([]byte(caps)) {
					if err := writeRaw(filepath.Join(*out, r, file+".capabilities.json"), []byte(caps)); err != nil {
						fmt.Fprintln(os.Stderr, scrubbed(err))
					}
				}
				mu.Lock()
				summary.Devices = append(summary.Devices, devRow{r, j.name, j.provider, j.typ, j.status, j.arn, filepath.Join(r, file+".json")})
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if err := writeJSON(filepath.Join(*out, "summary.json"), summary); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "seed %s; searched %v; device regions %v; %d GetDevice responses saved to %s\n",
		seed, summary.Searched, summary.DeviceRegions, len(summary.Devices), *out)
	if len(failed) > 0 || len(summary.Errors) > 0 {
		return fmt.Errorf("errors in %d region(s)/device(s): see %s", len(summary.Errors), filepath.Join(*out, "summary.json"))
	}
	return nil
}
