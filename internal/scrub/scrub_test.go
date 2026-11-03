package scrub

import (
	"errors"
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
		{"uuid with an all-digit tail untouched", `s3://b/puffball/r/d53c0e86-bf89-4ce0-9012-604162481234/`, `s3://b/puffball/r/d53c0e86-bf89-4ce0-9012-604162481234/`},
		{"uuid tail in a task arn", `arn:aws:braket:us-west-1:123456789012:quantum-task/00000000-0000-4000-8000-123456789012`, `arn:aws:braket:us-west-1:000000000000:quantum-task/00000000-0000-4000-8000-123456789012`},
		{"account after a non-uuid dash", `role-123456789012 x-1234-1234-1234-123456789012`, `role-000000000000 x-1234-1234-1234-000000000000`},
		{"longer hex before the prefix is not a uuid", `0d53c0e86-bf89-4ce0-9012-604162481234`, `0d53c0e86-bf89-4ce0-9012-000000000000`},
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

func TestError(t *testing.T) {
	if Error(nil) != nil {
		t.Error("Error(nil) != nil")
	}
	cause := errors.New("denied for arn:aws:iam::123456789012:user/bob")
	err := Error(cause)
	if strings.Contains(err.Error(), "123456789012") || strings.Contains(err.Error(), "bob") {
		t.Errorf("not scrubbed: %s", err)
	}
	if !errors.Is(err, cause) {
		t.Error("cause lost")
	}
}

func TestDisplay(t *testing.T) {
	cases := map[string]string{
		"arn:aws:braket:us-west-1:123456789012:quantum-task/x":    "arn:aws:braket:us-west-1:<account>:quantum-task/x",
		"amazon-braket-us-west-1-123456789012":                    "amazon-braket-us-west-1-<account>",
		"User: arn:aws:iam::123456789012:user/alice is not":       "User: arn:aws:iam::<account>:user/scrubbed is not",
		"task 00000000-0000-4000-8000-000000000000 was not found": "task 00000000-0000-4000-8000-000000000000 was not found",
		"accepted for 000000000000.":                              "accepted for <account>.",
		"two 123456789012 210987654321":                           "two <account> <account>",
		"float 0.123456789012345":                                 "float 0.123456789012345",
	}
	for in, want := range cases {
		if got := Display(in); got != want {
			t.Errorf("Display(%q) = %q, want %q", in, got, want)
		}
	}
}
