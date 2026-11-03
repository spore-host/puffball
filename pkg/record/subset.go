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
// verbatim:
//   - standardized.oneQubitProperties and twoQubitProperties entries for
//     those qubits and pairs of them;
//   - provider.specs.benchmarks[] and instructions[] sites whose node_ids are
//     all among them.
//
// CalibratedFrom and CalibratedTo span the timestamps of every provider site
// that touches any of the qubits, including device-wide ones such as
// simultaneous benchmarks, since those figures feed the standardized
// properties. Without such timestamps (AQT, IonQ) they are
// standardized.updatedAt (#2).
func subset(doc string, qubits []int) (*Calibration, error) {
	if doc == "" {
		return nil, errors.New("no capabilities document")
	}
	if len(qubits) == 0 {
		return nil, errors.New("no physical qubits, so no calibration to take")
	}
	type group struct {
		Name  string            `json:"name"`
		Sites []json.RawMessage `json:"sites"`
	}
	var c struct {
		Standardized struct {
			UpdatedAt string                     `json:"updatedAt"`
			One       map[string]json.RawMessage `json:"oneQubitProperties"`
			Two       map[string]json.RawMessage `json:"twoQubitProperties"`
		} `json:"standardized"`
		Provider struct {
			Specs struct {
				Benchmarks   []group `json:"benchmarks"`
				Instructions []group `json:"instructions"`
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
		Pairs: map[string]json.RawMessage{}, Benchmarks: map[string][]json.RawMessage{}, Instructions: map[string][]json.RawMessage{}}
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
	take := func(groups []group, into map[string][]json.RawMessage) {
		for _, g := range groups {
			for _, raw := range g.Sites {
				var site struct {
					Nodes []int `json:"node_ids"`
					Chars []struct {
						Timestamp string `json:"timestamp"`
					} `json:"characteristics"`
				}
				if json.Unmarshal(raw, &site) != nil || len(site.Nodes) == 0 {
					continue
				}
				all, touches := true, false
				for _, n := range site.Nodes {
					all = all && in[n]
					touches = touches || in[n]
				}
				if !touches {
					continue
				}
				if all {
					into[g.Name] = append(into[g.Name], compact(raw))
				}
				for _, ch := range site.Chars {
					if t, err := time.Parse(time.RFC3339Nano, ch.Timestamp); err == nil {
						times = append(times, t.UTC())
					}
				}
			}
		}
	}
	take(c.Provider.Specs.Benchmarks, cal.Benchmarks)
	take(c.Provider.Specs.Instructions, cal.Instructions)
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

// SubsetSum is the SHA-256 of a calibration's qubits, pairs, benchmarks and
// instructions in canonical JSON: keys sorted at every level, no
// whitespace, numbers exactly as written, HTML escaping off. Any re-encoding
// that keeps values and number text (key order and whitespace may change)
// gives the same sum.
func SubsetSum(c *Calibration) (string, error) {
	b, err := Canonical(map[string]any{"benchmarks": c.Benchmarks, "instructions": c.Instructions, "pairs": c.Pairs, "qubits": c.Qubits})
	if err != nil {
		return "", err
	}
	return Sum(b), nil
}

// Canonical is v as canonical JSON: decoded with numbers kept as written,
// re-encoded with map keys sorted (encoding/json sorts them), no whitespace
// and HTML escaping off.
func Canonical(v any) ([]byte, error) {
	raw, err := encode(v, false)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var x any
	if err := dec.Decode(&x); err != nil {
		return nil, fmt.Errorf("canonical: %w", err)
	}
	return encode(x, false)
}
