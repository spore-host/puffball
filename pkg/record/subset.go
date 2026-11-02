package record

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// subset takes the calibration for qubits out of a capabilities document,
// verbatim: standardized.oneQubitProperties and twoQubitProperties entries
// for those qubits and pairs of them, and provider.specs.benchmarks sites
// whose node_ids are all among them. The timestamps are those sites'
// characteristics' (Rigetti), or standardized.updatedAt (AQT, IonQ) (#2).
func subset(doc string, qubits []int) (*Calibration, error) {
	if doc == "" {
		return nil, errors.New("no capabilities document")
	}
	if len(qubits) == 0 {
		return nil, errors.New("no physical qubits, so no calibration to take")
	}
	var c struct {
		Standardized struct {
			UpdatedAt string                     `json:"updatedAt"`
			One       map[string]json.RawMessage `json:"oneQubitProperties"`
			Two       map[string]json.RawMessage `json:"twoQubitProperties"`
		} `json:"standardized"`
		Provider struct {
			Specs struct {
				Benchmarks []struct {
					Name  string            `json:"name"`
					Sites []json.RawMessage `json:"sites"`
				} `json:"benchmarks"`
			} `json:"specs"`
		} `json:"provider"`
	}
	if err := json.Unmarshal([]byte(doc), &c); err != nil {
		return nil, fmt.Errorf("capabilities document: %w", err)
	}
	in := map[int]bool{}
	for _, q := range qubits {
		in[q] = true
	}
	cal := &Calibration{DocumentSHA256: Sum([]byte(doc)), Qubits: map[string]json.RawMessage{},
		Pairs: map[string]json.RawMessage{}, Benchmarks: map[string][]json.RawMessage{}}
	for k, v := range c.Standardized.One {
		if n, err := strconv.Atoi(k); err == nil && in[n] {
			cal.Qubits[k] = compact(v)
		}
	}
	for k, v := range c.Standardized.Two {
		a, b, ok := strings.Cut(k, "-")
		na, ea := strconv.Atoi(a)
		nb, eb := strconv.Atoi(b)
		if ok && ea == nil && eb == nil && in[na] && in[nb] {
			cal.Pairs[k] = compact(v)
		}
	}
	var times []time.Time
	for _, bm := range c.Provider.Specs.Benchmarks {
		for _, raw := range bm.Sites {
			var site struct {
				Nodes []int `json:"node_ids"`
				Chars []struct {
					Timestamp string `json:"timestamp"`
				} `json:"characteristics"`
			}
			if json.Unmarshal(raw, &site) != nil || len(site.Nodes) == 0 {
				continue
			}
			all := true
			for _, n := range site.Nodes {
				all = all && in[n]
			}
			if !all {
				continue
			}
			cal.Benchmarks[bm.Name] = append(cal.Benchmarks[bm.Name], compact(raw))
			for _, ch := range site.Chars {
				if t, err := time.Parse(time.RFC3339Nano, ch.Timestamp); err == nil {
					times = append(times, t.UTC())
				}
			}
		}
	}
	if len(times) == 0 {
		if t, err := time.Parse(time.RFC3339Nano, c.Standardized.UpdatedAt); err == nil {
			times = append(times, t.UTC())
		}
	}
	for i := range times {
		t := times[i]
		if cal.CalibratedFrom == nil || t.Before(*cal.CalibratedFrom) {
			cal.CalibratedFrom = &t
		}
		if cal.CalibratedTo == nil || t.After(*cal.CalibratedTo) {
			cal.CalibratedTo = &t
		}
	}
	sum, err := SubsetSum(cal)
	if err != nil {
		return nil, err
	}
	cal.SubsetSHA256 = sum
	return cal, nil
}

func compact(raw json.RawMessage) json.RawMessage {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return raw
	}
	return buf.Bytes()
}

// SubsetSum is the SHA-256 of a calibration's qubits, pairs and benchmarks,
// encoded as compact JSON with sorted keys and HTML escaping off: the bytes
// [Verify] recomputes from a record.
func SubsetSum(c *Calibration) (string, error) {
	norm := func(m map[string]json.RawMessage) map[string]json.RawMessage {
		out := make(map[string]json.RawMessage, len(m))
		for k, v := range m {
			out[k] = compact(v)
		}
		return out
	}
	bms := make(map[string][]json.RawMessage, len(c.Benchmarks))
	for k, vs := range c.Benchmarks {
		for _, v := range vs {
			bms[k] = append(bms[k], compact(v))
		}
	}
	b, err := encode(struct {
		Benchmarks map[string][]json.RawMessage `json:"benchmarks"`
		Pairs      map[string]json.RawMessage   `json:"pairs"`
		Qubits     map[string]json.RawMessage   `json:"qubits"`
	}{bms, norm(c.Pairs), norm(c.Qubits)}, false)
	if err != nil {
		return "", err
	}
	return Sum(b), nil
}
