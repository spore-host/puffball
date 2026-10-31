package main

import (
	"path/filepath"
	"testing"
)

func TestRegionOf(t *testing.T) {
	cases := map[string]string{
		"arn:aws:braket:us-west-1::device/qpu/rigetti/Cepheus-1-108Q": "us-west-1",
		"arn:aws:braket:::device/quantum-simulator/amazon/sv1":        "",
		"not-an-arn": "",
	}
	for arn, want := range cases {
		if got := regionOf(arn); got != want {
			t.Errorf("regionOf(%q) = %q, want %q", arn, got, want)
		}
	}
}

func TestMoney(t *testing.T) {
	cases := map[float64]string{0.00375: "$0.00375", 0.3425: "$0.3425", 0.385: "$0.385", 0.725: "$0.725", 2: "$2.00", 0.3: "$0.30"}
	for v, want := range cases {
		if got := money(v); got != want {
			t.Errorf("money(%v) = %q, want %q", v, got, want)
		}
	}
}

func TestDeviceFile(t *testing.T) {
	cases := map[[2]string]string{
		{"Rigetti", "Cepheus-1-108Q"}:             "rigetti_cepheus-1-108q",
		{"Amazon Braket", "SV1"}:                  "amazon-braket_sv1",
		{"D-Wave Systems", "Advantage_system4.1"}: "d-wave-systems_advantage-system4-1",
	}
	for in, want := range cases {
		if got := deviceFile(in[0], in[1]); got != want {
			t.Errorf("deviceFile(%q, %q) = %q, want %q", in[0], in[1], got, want)
		}
	}
}

func TestPuffballTagged(t *testing.T) {
	if !puffballTagged(map[string]string{"puffball:project": "x"}) {
		t.Error("puffball:project not recognised")
	}
	if puffballTagged(map[string]string{"project": "x", "puffballish": "y"}) {
		t.Error("non-puffball tags recognised")
	}
}

// TestPlanEstimatesMatchPricingPage pins G0(c)'s arithmetic to the figures in
// the design (#1): $0.3425, $0.385 and the 3 s SV1 minimum.
func TestPlanEstimatesMatchPricingPage(t *testing.T) {
	if got := perTaskUSD + 100*cepheusShotUSD; money(got) != "$0.3425" {
		t.Errorf("Cepheus 100 shots = %s", money(got))
	}
	if got := perTaskUSD + 200*cepheusShotUSD; money(got) != "$0.385" {
		t.Errorf("Cepheus program set 2x100 = %s", money(got))
	}
	if got := simPerMinuteUSD * simMinSeconds / 60; money(got) != "$0.00375" {
		t.Errorf("SV1 minimum = %s", money(got))
	}
}

func TestResultPath(t *testing.T) {
	dir := "puffball/run/task"
	cases := map[string]string{
		dir + "/results.json":                  "results.json",
		dir + "/programs/0/results.json":       "programs/0/results.json",
		dir + "/programs/1/executables/0.json": "programs/1/executables/0.json",
	}
	for key, want := range cases {
		got, err := resultPath(dir, key)
		if err != nil || got != filepath.FromSlash(want) {
			t.Errorf("resultPath(%q) = %q, %v; want %q", key, got, err, want)
		}
	}
	for _, key := range []string{dir, dir + "/", dir + "/../escape.json", "elsewhere/x.json/../../../y"} {
		if got, err := resultPath(dir, key); err == nil {
			t.Errorf("resultPath(%q) = %q, want an error", key, got)
		}
	}
}
