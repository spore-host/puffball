// Package devices turns Braket's device responses into puff's device list:
// status and qubits from the service, execution windows, queue depth,
// calibration time, program-set support, aliases, and the retired devices'
// replacements.
//
// Every field path here was read from the GetDevice responses captured in
// G0(a) (#2, testdata/devices/). Qubit counts and status always come from the
// service, never from the alias table (G0: Cepheus reports 107 qubits; Ankaa-3
// is RETIRED).
package devices

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/braket"
	"github.com/aws/aws-sdk-go-v2/service/braket/types"

	pbraket "github.com/spore-host/puffball/pkg/braket"
)

// Paradigms, from the device's action types.
const (
	ParadigmGate   = "gate"   // braket.ir.openqasm.*
	ParadigmAnalog = "analog" // braket.ir.ahs.program
)

// Window is one execution window, in UTC, as the device reports it.
type Window struct {
	Day   string `json:"day"`   // Everyday, Weekdays, or a weekday name
	Start string `json:"start"` // HH:MM:SS
	End   string `json:"end"`   // HH:MM:SS, inclusive
}

// Queue is one queue's depth, from GetDevice.deviceQueueInfo.
type Queue struct {
	Name     string `json:"queue"`              // QUANTUM_TASKS_QUEUE or JOBS_QUEUE
	Priority string `json:"priority,omitempty"` // Normal or Priority (task queues)
	Size     string `json:"size"`               // as reported, a string
}

// ProgramSet is the device's program-set limits.
type ProgramSet struct {
	MaxExecutables int64 `json:"max_executables"`
	MaxTotalShots  int64 `json:"max_total_shots"`
	MinTotalShots  int64 `json:"min_total_shots,omitempty"`
}

// Calibration is when the device's calibration data was taken, and where in
// the capabilities that came from. At is the oldest calibration time, so age
// is never understated; Newest is set when the data spans more than one time
// (Rigetti benchmarks are taken at different times). At is nil when the
// capabilities carry no calibration time (IQM, QuEra, #2); Missing then says
// why, and nothing is substituted (law 6).
type Calibration struct {
	At      *time.Time `json:"at"`
	Newest  *time.Time `json:"newest,omitempty"`
	Source  string     `json:"source,omitempty"`
	Missing string     `json:"missing,omitempty"`
}

// Device is one device as puff lists it.
type Device struct {
	Alias       string   `json:"alias,omitempty"`
	Name        string   `json:"name"`
	Provider    string   `json:"provider"`
	Arn         string   `json:"arn"`
	Type        string   `json:"type"` // QPU or SIMULATOR
	Status      string   `json:"status"`
	Regions     []string `json:"regions"` // one for a QPU; every region a simulator was found in
	Paradigm    string   `json:"paradigm,omitempty"`
	Qubits      int      `json:"qubits,omitempty"`
	NativeGates []string `json:"native_gates,omitempty"`
	ShotsRange  []int64  `json:"shots_range,omitempty"`
	Windows     []Window `json:"execution_windows,omitempty"`
	// Queues is the first region's (in sorted order). A simulator's queue is
	// per region; QueuesByRegion has each one.
	Queues         []Queue            `json:"queues,omitempty"`
	QueuesByRegion map[string][]Queue `json:"queues_by_region,omitempty"`
	// StatusByRegion is set for a device found in several regions, so an
	// outage in one of them shows (Status is the first region's).
	StatusByRegion map[string]string `json:"status_by_region,omitempty"`
	// Missing names every field above the service didn't provide, so an
	// omitted field is never silent (law 6).
	Missing     []string    `json:"missing,omitempty"`
	ProgramSet  *ProgramSet `json:"program_set,omitempty"`
	Calibration Calibration `json:"calibration"`
	ReplacedBy  string      `json:"replaced_by,omitempty"`
	UpdatedAt   string      `json:"capabilities_updated_at,omitempty"`
}

// capabilities is the part of deviceCapabilities puff reads.
type capabilities struct {
	Service struct {
		ExecutionWindows []struct {
			ExecutionDay    string `json:"executionDay"`
			WindowStartHour string `json:"windowStartHour"`
			WindowEndHour   string `json:"windowEndHour"`
		} `json:"executionWindows"`
		ShotsRange []int64 `json:"shotsRange"`
		UpdatedAt  string  `json:"updatedAt"`
	} `json:"service"`
	Action   map[string]json.RawMessage `json:"action"`
	Paradigm struct {
		QubitCount    int      `json:"qubitCount"`
		NativeGateSet []string `json:"nativeGateSet"`
	} `json:"paradigm"`
	Provider struct {
		Specs struct {
			Benchmarks []struct {
				Sites []struct {
					Characteristics []struct {
						Timestamp string `json:"timestamp"`
					} `json:"characteristics"`
				} `json:"sites"`
			} `json:"benchmarks"`
		} `json:"specs"`
	} `json:"provider"`
	Standardized struct {
		UpdatedAt string `json:"updatedAt"`
	} `json:"standardized"`
}

// Parse builds a Device from a GetDevice response found in region.
func Parse(g *braket.GetDeviceOutput, region string) (Device, error) {
	d := Device{
		Name: aws.ToString(g.DeviceName), Provider: aws.ToString(g.ProviderName), Arn: aws.ToString(g.DeviceArn),
		Type: string(g.DeviceType), Status: string(g.DeviceStatus), Regions: []string{region},
	}
	var c capabilities
	if err := json.Unmarshal([]byte(aws.ToString(g.DeviceCapabilities)), &c); err != nil {
		return Device{}, fmt.Errorf("%s capabilities: %w", d.Name, err)
	}
	d.Qubits = c.Paradigm.QubitCount
	d.NativeGates = c.Paradigm.NativeGateSet
	d.ShotsRange = c.Service.ShotsRange
	d.UpdatedAt = c.Service.UpdatedAt
	for _, w := range c.Service.ExecutionWindows {
		d.Windows = append(d.Windows, Window{Day: w.ExecutionDay, Start: w.WindowStartHour, End: w.WindowEndHour})
	}
	for a := range c.Action {
		switch {
		case strings.HasPrefix(a, "braket.ir.ahs."):
			d.Paradigm = ParadigmAnalog
		case strings.HasPrefix(a, "braket.ir.openqasm."):
			if d.Paradigm == "" {
				d.Paradigm = ParadigmGate
			}
		}
	}
	if raw, ok := c.Action[pbraket.ActionProgramSet]; ok {
		var ps struct {
			MaxExecutables int64 `json:"maximumExecutables"`
			MaxTotalShots  int64 `json:"maximumTotalShots"`
			MinTotalShots  int64 `json:"minimumTotalShots"`
		}
		if err := json.Unmarshal(raw, &ps); err != nil {
			return Device{}, fmt.Errorf("%s program_set action: %w", d.Name, err)
		}
		d.ProgramSet = &ProgramSet{MaxExecutables: ps.MaxExecutables, MaxTotalShots: ps.MaxTotalShots, MinTotalShots: ps.MinTotalShots}
	}
	for _, q := range g.DeviceQueueInfo {
		d.Queues = append(d.Queues, Queue{Name: string(q.Queue), Priority: string(q.QueuePriority), Size: aws.ToString(q.QueueSize)})
	}
	d.Calibration = calibration(d, c)
	d.Missing = missingFields(d)
	return d, nil
}

// missingFields names the device fields the service left empty.
func missingFields(d Device) []string {
	var m []string
	for name, empty := range map[string]bool{
		"paradigm": d.Paradigm == "", "qubits": d.Qubits == 0,
		"execution_windows": len(d.Windows) == 0, "queues": len(d.Queues) == 0,
	} {
		if empty {
			m = append(m, name)
		}
	}
	sort.Strings(m)
	return m
}

// calibration finds the calibration time, by the paths measured in #2:
// standardized.updatedAt (AQT, IonQ), or the newest
// provider.specs.benchmarks[].sites[].characteristics[].timestamp (Rigetti).
// IQM and QuEra capabilities carry neither.
func calibration(d Device, c capabilities) Calibration {
	if d.Type == string(types.DeviceTypeSimulator) {
		return Calibration{Missing: "simulators have no calibration"}
	}
	if t, err := time.Parse(time.RFC3339Nano, c.Standardized.UpdatedAt); err == nil {
		return Calibration{At: &t, Source: "standardized.updatedAt"}
	}
	var oldest, newest *time.Time
	for _, b := range c.Provider.Specs.Benchmarks {
		for _, s := range b.Sites {
			for _, ch := range s.Characteristics {
				t, err := time.Parse(time.RFC3339Nano, ch.Timestamp)
				if err != nil {
					continue
				}
				if oldest == nil || t.Before(*oldest) {
					oldest = &t
				}
				if newest == nil || t.After(*newest) {
					newest = &t
				}
			}
		}
	}
	if oldest != nil {
		c := Calibration{At: oldest, Source: "provider.specs.benchmarks[].sites[].characteristics[].timestamp (oldest; newest in newest)"}
		if !newest.Equal(*oldest) {
			c.Newest = newest
		}
		return c
	}
	return Calibration{Missing: "the device's capabilities carry no calibration timestamp; service.updatedAt is the document's time, not the calibration's, so it is not used"}
}

// QueueSize returns the depth of the task queue with the given priority
// ("Normal" or "Priority"), or of the jobs queue for priority "", and whether
// it was reported as a number.
func (d Device) QueueSize(priority string) (int, bool) {
	for _, q := range d.Queues {
		jobs := q.Name == string(types.QueueNameJobsQueue)
		if (priority == "" && jobs) || (!jobs && q.Priority == priority) {
			n, err := strconv.Atoi(q.Size)
			return n, err == nil
		}
	}
	return 0, false
}

// Retired names devices that were retired, with their replacements, from
// aws-quantum-tutorial-for-research devices.py (RETIRED, commit 8a4d2e0).
// A retired device not here has no known replacement; none is guessed.
var Retired = map[string]string{
	"arn:aws:braket:us-east-1::device/qpu/ionq/Aria-1":     "arn:aws:braket:us-east-1::device/qpu/ionq/Forte-1",
	"arn:aws:braket:us-east-1::device/qpu/ionq/Aria-2":     "arn:aws:braket:us-east-1::device/qpu/ionq/Forte-1",
	"arn:aws:braket:us-east-1::device/qpu/ionq/Harmony":    "arn:aws:braket:us-east-1::device/qpu/ionq/Forte-1",
	"arn:aws:braket:::device/quantum-simulator/amazon/tn1": "arn:aws:braket:::device/quantum-simulator/amazon/sv1",
}

// Aliases maps short names to device ARNs.
type Aliases map[string]string

// List is every device found, and which regions (if any) failed.
type List struct {
	Devices []Device
	Err     error // an *braket.IncompleteError when a region or device failed
}

// Collect discovers devices in every region and reads each one's GetDevice.
// A regional QPU is read in its own region; a simulator in every region it
// was found in, merged in sorted region order (QueuesByRegion keeps each
// region's queue). Retired devices are listed from SearchDevices alone.
// aliases names devices by ARN. A device that fails to read fails its region
// without dropping the region's other devices. The returned error is the same
// *IncompleteError as List.Err, so a partial list can't pass for a complete one.
func Collect(ctx context.Context, f pbraket.Factory, aliases Aliases) (*List, error) {
	inv, err := pbraket.Discover(ctx, f)
	if inv == nil {
		return nil, err
	}
	failed := map[string]error{}
	if ie, ok := pbraket.AsIncomplete(err); ok {
		for r, e := range ie.Failed {
			failed[r] = e
		}
	}
	byArn := map[string]string{}
	for a, arn := range aliases {
		byArn[arn] = a
	}

	type job struct {
		region string
		sum    types.DeviceSummary
	}
	var jobs []job
	for _, r := range inv.Regions() {
		for _, s := range inv.Devices[r] {
			arn := aws.ToString(s.DeviceArn)
			if dr := pbraket.RegionOf(arn); dr != "" && dr != r {
				continue // a QPU is read in its own region only
			}
			jobs = append(jobs, job{r, s})
		}
	}
	regionJobs := map[string][]job{}
	for _, j := range jobs {
		regionJobs[j.region] = append(regionJobs[j.region], j)
	}
	var regions []string
	for r := range regionJobs {
		regions = append(regions, r)
	}
	// Each region returns its own devices; results come back sorted by
	// region, and are merged in that order, so a simulator's merged fields
	// never depend on which goroutine finished first.
	results := pbraket.FanOut(ctx, regions, func(ctx context.Context, region string) ([]Device, error) {
		c, err := f.Braket(ctx, region)
		if err != nil {
			return nil, err
		}
		var out []Device
		var errs []error
		for _, j := range regionJobs[region] {
			arn := aws.ToString(j.sum.DeviceArn)
			if j.sum.DeviceStatus == types.DeviceStatusRetired {
				out = append(out, Device{Name: aws.ToString(j.sum.DeviceName), Provider: aws.ToString(j.sum.ProviderName), Arn: arn,
					Type: string(j.sum.DeviceType), Status: string(j.sum.DeviceStatus), Regions: []string{region},
					Calibration: Calibration{Missing: "retired: GetDevice is not read for retired devices"},
					Missing:     []string{"execution_windows", "paradigm", "qubits", "queues"}})
				continue
			}
			g, err := c.GetDevice(ctx, &braket.GetDeviceInput{DeviceArn: aws.String(arn)})
			if err != nil {
				errs = append(errs, fmt.Errorf("GetDevice %s: %w", aws.ToString(j.sum.DeviceName), err))
				continue
			}
			d, err := Parse(g, region)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			out = append(out, d)
		}
		// The devices that were read are kept; the region still fails.
		return out, errors.Join(errs...)
	})
	found := map[string]*Device{}
	var order []string
	for _, r := range results {
		if r.Err != nil {
			failed[r.Region] = r.Err
		}
		for _, d := range r.Value {
			if prev, ok := found[d.Arn]; ok {
				prev.Regions = append(prev.Regions, r.Region)
				if len(d.Queues) > 0 {
					prev.QueuesByRegion[r.Region] = d.Queues
				}
				if prev.StatusByRegion == nil {
					prev.StatusByRegion = map[string]string{prev.Regions[0]: prev.Status}
				}
				prev.StatusByRegion[r.Region] = d.Status
				continue
			}
			d := d
			d.Alias = byArn[d.Arn]
			d.ReplacedBy = Retired[d.Arn]
			if len(d.Queues) > 0 {
				d.QueuesByRegion = map[string][]Queue{r.Region: d.Queues}
			}
			found[d.Arn] = &d
			order = append(order, d.Arn)
		}
	}
	l := &List{}
	for _, arn := range order {
		l.Devices = append(l.Devices, *found[arn])
	}
	sort.Slice(l.Devices, func(i, j int) bool {
		a, b := l.Devices[i], l.Devices[j]
		if a.Type != b.Type {
			return a.Type < b.Type // QPU before SIMULATOR
		}
		return a.Name < b.Name
	})
	if len(failed) > 0 {
		l.Err = &pbraket.IncompleteError{Op: "device discovery", Failed: failed}
		return l, l.Err
	}
	return l, nil
}

// Resolve finds a device by alias, name (case-insensitive) or ARN in l. A
// retired device is an error naming its replacement, with a zero Device.
func (l *List) Resolve(s string, aliases Aliases) (Device, error) {
	want := s
	if arn, ok := aliases[strings.ToLower(s)]; ok {
		want = arn
	}
	for _, d := range l.Devices {
		if d.Arn == want || strings.EqualFold(d.Name, s) {
			if d.Status == string(types.DeviceStatusRetired) {
				repl := "no known replacement"
				if d.ReplacedBy != "" {
					repl = "replaced by " + d.ReplacedBy
				}
				return Device{}, fmt.Errorf("device %q is RETIRED (%s)", s, repl)
			}
			return d, nil
		}
	}
	if _, ok := aliases[strings.ToLower(s)]; ok {
		return Device{}, fmt.Errorf("device %q (%s) was not found in any region that answered", s, want)
	}
	return Device{}, fmt.Errorf("unknown device %q", s)
}
