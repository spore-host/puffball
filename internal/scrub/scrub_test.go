package scrub

import (
	"strings"
	"testing"
)

func TestScrub(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"task arn", `"arn:aws:braket:us-west-1:123456789012:quantum-task/abc"`, `"arn:aws:braket:us-west-1:000000000000:quantum-task/abc"`},
		{"device arn untouched", `"arn:aws:braket:us-west-1::device/qpu/rigetti/Cepheus-1-108Q"`, `"arn:aws:braket:us-west-1::device/qpu/rigetti/Cepheus-1-108Q"`},
		{"results bucket", `"outputS3Bucket": "amazon-braket-us-west-1-123456789012"`, `"outputS3Bucket": "amazon-braket-us-west-1-000000000000"`},
		{"iam user", `arn:aws:iam::123456789012:user/alice"`, `arn:aws:iam::000000000000:user/scrubbed"`},
		{"sts role", `arn:aws:sts::123456789012:assumed-role/Admin/session"`, `arn:aws:sts::000000000000:assumed-role/scrubbed"`},
		{"json-escaped principal", `"{\"arn\": \"arn:aws:iam::123456789012:user/bob\"}"`, `"{\"arn\": \"arn:aws:iam::000000000000:user/scrubbed\"}"`},
		{"message text", `User agreement has not been accepted for 123456789012. Please`, `User agreement has not been accepted for 000000000000. Please`},
		{"json account field", `{"Account": "123456789012"}`, `{"Account": "000000000000"}`},
		{"float mantissa untouched", `"value": 3.305363491528e-05`, `"value": 3.305363491528e-05`},
		{"float fraction untouched", `0.998565742563,`, `0.998565742563,`},
		{"13 digits untouched", `"t": 1761321273107`, `"t": 1761321273107`},
		{"two adjacent", `123456789012 210987654321`, `000000000000 000000000000`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(Scrub([]byte(tc.in))); got != tc.want {
				t.Errorf("Scrub(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestFind(t *testing.T) {
	dirty := `arn:aws:braket:us-west-1:123456789012:quantum-task/x arn:aws:iam::000000000000:user/bob for 210987654321.`
	got := Find([]byte(dirty))
	want := []string{"123456789012", "210987654321", "arn:aws:braket:us-west-1:123456789012:", "arn:aws:iam::000000000000:user/bob"}
	for _, w := range want {
		found := false
		for _, g := range got {
			if strings.Contains(g, w) {
				found = true
			}
		}
		if !found {
			t.Errorf("Find missed %q (got %q)", w, got)
		}
	}
	if f := Find(Scrub([]byte(dirty))); len(f) != 0 {
		t.Errorf("Find on scrubbed input = %q, want nothing", f)
	}
	if f := Find([]byte(`0.305363491528e-05 1.598492347235E-3`)); len(f) != 0 {
		t.Errorf("Find flagged float mantissas: %q", f)
	}
}
