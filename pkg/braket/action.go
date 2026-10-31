package braket

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
)

// Action JSON follows amazon-braket-schemas-python at 8aa7ca9:
// src/braket/ir/openqasm/program_v1.py, program_set_v1.py and
// src/braket/ir/ahs/program_v1.py. Validation here catches malformed input
// before a billable call; the service remains the authority on what a device
// accepts (law 5).

// Action types, as they appear in braketSchemaHeader.name and in a device's
// capabilities "action" map.
const (
	ActionOpenQASM   = "braket.ir.openqasm.program"
	ActionProgramSet = "braket.ir.openqasm.program_set"
	ActionAHS        = "braket.ir.ahs.program"
)

type schemaHeader struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Program is one OpenQASM 3 program and its inputs. An input value is a
// number, a bit string, or (in a program set) a list of them.
type Program struct {
	Source string
	Inputs map[string]any
}

type programIR struct {
	Header schemaHeader   `json:"braketSchemaHeader"`
	Source string         `json:"source"`
	Inputs map[string]any `json:"inputs,omitempty"`
}

type programSetIR struct {
	Header   schemaHeader `json:"braketSchemaHeader"`
	Programs []programIR  `json:"programs"`
}

func (p Program) ir() (programIR, error) {
	if strings.TrimSpace(p.Source) == "" {
		return programIR{}, errors.New("OpenQASM program has no source")
	}
	for k, v := range p.Inputs {
		if k == "" {
			return programIR{}, errors.New("OpenQASM program has an input with an empty name")
		}
		if err := checkInput(v); err != nil {
			return programIR{}, fmt.Errorf("input %q: %w", k, err)
		}
	}
	return programIR{Header: schemaHeader{ActionOpenQASM, "1"}, Source: p.Source, Inputs: p.Inputs}, nil
}

// bitString is program_v1.py's string leaf: constr(regex="^[01]+$").
var bitString = regexp.MustCompile(`^[01]+$`)

// checkInput accepts program_v1.py's io_type: a leaf (bit string, float or
// int) or a list of leaves. Booleans and other strings are rejected.
func checkInput(v any) error {
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array {
		for i := 0; i < rv.Len(); i++ {
			if err := checkLeaf(rv.Index(i).Interface()); err != nil {
				return fmt.Errorf("element %d: %w", i, err)
			}
		}
		return nil
	}
	return checkLeaf(v)
}

func checkLeaf(v any) error {
	switch x := v.(type) {
	case string:
		if !bitString.MatchString(x) {
			return fmt.Errorf("string %q is not a bit string (^[01]+$)", x)
		}
		return nil
	case json.Number:
		return nil
	case bool, nil:
		return fmt.Errorf("%v is not a number or bit string", v)
	}
	switch reflect.ValueOf(v).Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return nil
	}
	return fmt.Errorf("%T is not a number or bit string", v)
}

// marshal encodes v without HTML escaping, so OpenQASM's < and & stay
// readable in records and diffs.
func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// OpenQASMAction returns the CreateQuantumTask action JSON for one program.
func OpenQASMAction(p Program) ([]byte, error) {
	ir, err := p.ir()
	if err != nil {
		return nil, err
	}
	return marshal(ir)
}

// ProgramSetAction returns the action JSON for a program set and its number
// of executables. A program with no inputs is one executable; otherwise every
// input must be a list, all of the same length, and that length is its
// executable count (program_set_v1.py). Shots for the task are the total
// across executables (#4). Per-device limits (maximumExecutables,
// maximumTotalShots) are checked by the caller against the device's
// capabilities.
func ProgramSetAction(programs []Program) ([]byte, int, error) {
	if len(programs) == 0 {
		return nil, 0, errors.New("program set has no programs")
	}
	set := programSetIR{Header: schemaHeader{ActionProgramSet, "1"}}
	total := 0
	for i, p := range programs {
		ir, err := p.ir()
		if err != nil {
			return nil, 0, fmt.Errorf("program %d: %w", i, err)
		}
		n, err := executables(p.Inputs)
		if err != nil {
			return nil, 0, fmt.Errorf("program %d: %w", i, err)
		}
		total += n
		set.Programs = append(set.Programs, ir)
	}
	b, err := marshal(set)
	if err != nil {
		return nil, 0, err
	}
	return b, total, nil
}

func executables(inputs map[string]any) (int, error) {
	if len(inputs) == 0 {
		return 1, nil
	}
	n := -1
	for k, v := range inputs {
		rv := reflect.ValueOf(v)
		if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array {
			return 0, fmt.Errorf("input %q is not a list; every input in a program set must be a list", k)
		}
		if n >= 0 && rv.Len() != n {
			return 0, errors.New("inputs have different lengths; every input in a program set must have the same length")
		}
		n = rv.Len()
	}
	if n == 0 {
		return 0, errors.New("inputs are empty lists")
	}
	return n, nil
}

// AHSAction validates an AHS program (braket.ir.ahs.program v1, as written by
// the Braket SDK's to_ir()) and returns it with the schema header set.
// Numbers keep their exact text: atom coordinates are decimals and must not be
// rounded through float64.
func AHSAction(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var prog map[string]any
	if err := dec.Decode(&prog); err != nil {
		return nil, fmt.Errorf("AHS program is not a JSON object: %w", err)
	}
	if dec.More() {
		return nil, errors.New("AHS program has trailing data after the JSON object")
	}
	if h, ok := prog["braketSchemaHeader"]; ok {
		hm, _ := h.(map[string]any)
		if hm["name"] != ActionAHS || hm["version"] != "1" {
			return nil, fmt.Errorf("braketSchemaHeader is %v, want %s version 1", h, ActionAHS)
		}
	}
	setup, _ := prog["setup"].(map[string]any)
	reg, _ := setup["ahs_register"].(map[string]any)
	if _, ok := reg["sites"].([]any); !ok {
		return nil, errors.New("AHS program has no setup.ahs_register.sites list")
	}
	if _, ok := reg["filling"].([]any); !ok {
		return nil, errors.New("AHS program has no setup.ahs_register.filling list")
	}
	ham, _ := prog["hamiltonian"].(map[string]any)
	if _, ok := ham["drivingFields"].([]any); !ok {
		return nil, errors.New("AHS program has no hamiltonian.drivingFields list")
	}
	// The schema names it localDetuning, with shiftingFields as its alias.
	_, ld := ham["localDetuning"].([]any)
	_, sf := ham["shiftingFields"].([]any)
	if !ld && !sf {
		return nil, errors.New("AHS program has no hamiltonian.localDetuning (or shiftingFields) list")
	}
	prog["braketSchemaHeader"] = schemaHeader{ActionAHS, "1"}
	return marshal(prog)
}
