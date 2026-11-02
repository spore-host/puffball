package record

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
)

// SchemaJSON is the puffball.record/v0 JSON Schema (draft 2020-12).
//
//go:embed schema/record-v0.schema.json
var SchemaJSON []byte

// Verify checks a record offline:
//   - it conforms to [SchemaJSON];
//   - every field left empty or absent is named in missing[] (law 6; see
//     [Unfilled]);
//   - each executable's counts add up to its shots, and those to
//     shots.measured;
//   - every hash it can recompute matches (program sources, compiled
//     programs, calibration subsets).
//
// Object hashes link to S3 and need the objects to check. It returns each
// problem found; none means the record checks.
func Verify(b []byte) []string {
	var doc any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return []string{"not JSON: " + err.Error()}
	}
	var schema map[string]any
	if err := json.Unmarshal(SchemaJSON, &schema); err != nil {
		return []string{"the embedded schema is not JSON: " + err.Error()}
	}
	v := validator{root: schema}
	v.check("$", schema, doc)
	if len(v.problems) > 0 {
		return v.problems
	}
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		return []string{"record: " + err.Error()}
	}
	var p []string
	for _, f := range Unfilled(&r) {
		p = append(p, f+": empty or absent, and not named in missing[]")
	}
	var measured int64
	for _, pr := range r.Results {
		for _, ex := range pr.Executables {
			var n int64
			for _, c := range ex.Counts {
				n += c
			}
			if n != ex.Shots {
				p = append(p, fmt.Sprintf("results[%d].executables[%d]: counts add up to %d, not its %d shots", pr.Program, ex.Executable, n, ex.Shots))
			}
			measured += ex.Shots
		}
	}
	if measured != r.Shots.Measured {
		p = append(p, fmt.Sprintf("shots.measured is %d, but the executables' shots add up to %d", r.Shots.Measured, measured))
	}
	for i, t := range r.Program.Sources {
		if Sum([]byte(t.Text)) != t.SHA256 {
			p = append(p, fmt.Sprintf("program.sources[%d]: sha256 doesn't match its text", i))
		}
	}
	for i, t := range r.CompiledPrograms {
		if Sum([]byte(t.Text)) != t.SHA256 {
			p = append(p, fmt.Sprintf("compiled_programs[%d]: sha256 doesn't match its text", i))
		}
	}
	for name, c := range map[string]*Calibration{"at_submission": r.Calibration.AtSubmission, "at_completion": r.Calibration.AtCompletion, "at_record": r.Calibration.AtRecord} {
		if c == nil {
			continue
		}
		if sum, err := SubsetSum(c); err != nil || sum != c.SubsetSHA256 {
			p = append(p, "calibration."+name+": subset_sha256 doesn't match its qubits, pairs and benchmarks")
		}
	}
	sort.Strings(p)
	return p
}

// Unfilled returns the record's fields that are empty or absent with no
// missing[] entry naming them (or a part of them, such as
// compiled_programs[1]).
func Unfilled(r *Record) []string {
	named := func(f string) bool {
		for _, m := range r.Missing {
			if m.Field == f || strings.HasPrefix(m.Field, f+".") || strings.HasPrefix(m.Field, f+"[") {
				return true
			}
		}
		return false
	}
	checks := []struct {
		field string
		empty bool
	}{
		{"task.created_at", r.Task.CreatedAt == nil},
		{"task.ended_at", r.Task.EndedAt == nil},
		{"program.sources", len(r.Program.Sources) == 0},
		{"program.submitted_action_sha256", r.Program.SubmittedSHA256 == ""},
		{"shots.successful", r.Shots.Successful == nil},
		{"shots.measured", len(r.Results) == 0},
		{"physical_qubits", len(r.PhysicalQubits) == 0},
		{"compiled_programs", len(r.CompiledPrograms) == 0},
		{"results", len(r.Results) == 0},
		{"calibration.at_submission", r.Calibration.AtSubmission == nil},
		{"calibration.at_completion", r.Calibration.AtCompletion == nil},
		{"cost", r.Cost == nil},
	}
	for _, pr := range r.Results {
		checks = append(checks, struct {
			field string
			empty bool
		}{fmt.Sprintf("results[%d].executables", pr.Program), len(pr.Executables) == 0})
	}
	var out []string
	for _, c := range checks {
		if c.empty && !named(c.field) {
			out = append(out, c.field)
		}
	}
	return out
}

// validator implements the part of JSON Schema SchemaJSON uses: type, const,
// enum, pattern, required, properties, additionalProperties, items and
// local $ref.
type validator struct {
	root     map[string]any
	problems []string
}

func (v *validator) fail(path, format string, args ...any) {
	v.problems = append(v.problems, path+": "+fmt.Sprintf(format, args...))
}

func (v *validator) resolve(s map[string]any) map[string]any {
	ref, ok := s["$ref"].(string)
	if !ok {
		return s
	}
	cur := any(v.root)
	for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		m, ok := cur.(map[string]any)
		if !ok {
			return map[string]any{}
		}
		cur = m[part]
	}
	m, _ := cur.(map[string]any)
	return m
}

func typeOf(x any) string {
	switch t := x.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case json.Number:
		if f, err := t.Float64(); err == nil && f == math.Trunc(f) && !strings.ContainsAny(t.String(), ".eE") {
			return "integer"
		}
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return "unknown"
}

func typeOK(want any, got string) bool {
	match := func(w string) bool { return w == got || (w == "number" && got == "integer") }
	switch w := want.(type) {
	case string:
		return match(w)
	case []any:
		for _, x := range w {
			if s, ok := x.(string); ok && match(s) {
				return true
			}
		}
	}
	return false
}

func (v *validator) check(path string, s map[string]any, x any) {
	s = v.resolve(s)
	got := typeOf(x)
	if t, ok := s["type"]; ok && !typeOK(t, got) {
		v.fail(path, "is %s, want %v", got, t)
		return
	}
	if c, ok := s["const"]; ok && fmt.Sprint(c) != fmt.Sprint(x) {
		v.fail(path, "is %v, want %v", x, c)
	}
	if e, ok := s["enum"].([]any); ok {
		found := false
		for _, want := range e {
			found = found || fmt.Sprint(want) == fmt.Sprint(x)
		}
		if !found {
			v.fail(path, "%v is not one of %v", x, e)
		}
	}
	if pat, ok := s["pattern"].(string); ok {
		if str, ok := x.(string); ok {
			if re, err := regexp.Compile(pat); err != nil || !re.MatchString(str) {
				v.fail(path, "%q doesn't match %s", str, pat)
			}
		}
	}
	switch val := x.(type) {
	case map[string]any:
		for _, r := range asList(s["required"]) {
			if _, ok := val[r]; !ok {
				v.fail(path, "lacks required %q", r)
			}
		}
		props, _ := s["properties"].(map[string]any)
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if ps, ok := props[k].(map[string]any); ok {
				v.check(path+"."+k, ps, val[k])
				continue
			}
			switch ap := s["additionalProperties"].(type) {
			case bool:
				if !ap {
					v.fail(path, "has unknown field %q", k)
				}
			case map[string]any:
				v.check(path+"."+k, ap, val[k])
			}
		}
	case []any:
		if is, ok := s["items"].(map[string]any); ok {
			for i, e := range val {
				v.check(fmt.Sprintf("%s[%d]", path, i), is, e)
			}
		}
	}
}

func asList(x any) []string {
	l, _ := x.([]any)
	out := make([]string, 0, len(l))
	for _, e := range l {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// Encode is a record as puff writes it: indented JSON, HTML escaping off.
func Encode(r *Record) ([]byte, error) {
	b, err := encode(r, true)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
