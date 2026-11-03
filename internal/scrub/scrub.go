// Package scrub removes AWS account identifiers from captured API responses
// before they become fixtures (law 7: no account numbers in code, fixtures,
// issues, PRs or posted logs).
//
// Account IDs are matched by context — the account field of an ARN, the
// account suffix of a Braket results bucket, a JSON account field, a free
// 12-digit run that is not part of a decimal number — because device
// capabilities are full of 12-digit float mantissas (0.305363491528e-05) that
// must survive untouched.
package scrub

import (
	"regexp"
	"strings"
)

// Placeholder replaces every account ID.
const Placeholder = "000000000000"

// PrincipalPlaceholder replaces IAM user and role names.
const PrincipalPlaceholder = "scrubbed"

var (
	// arn:<partition>:<service>:<region>:<account>:<resource>
	arnAccount = regexp.MustCompile(`(arn:aws[a-z-]*:[a-z0-9-]*:[a-z0-9-]*:)[0-9]{12}(:)`)
	// amazon-braket-<region>-<account>, the SDK's default results bucket.
	bucketAccount = regexp.MustCompile(`(amazon-braket-[a-z]+-[a-z]+-[0-9]-)[0-9]{12}`)
	// IAM/STS principal names after the (already scrubbed) account field.
	principal = regexp.MustCompile(`(arn:aws[a-z-]*:(?:iam|sts)::` + Placeholder + `:(?:user|role|assumed-role|federated-user)/)[^"\\\s,}]+`)
	// Any IAM/STS principal, for Find.
	principalAny = regexp.MustCompile(`(arn:aws[a-z-]*:(?:iam|sts)::[0-9]{12}:(?:user|role|assumed-role|federated-user)/)([^"\\\s,}]+)`)
)

// freeRuns returns the [start, end) offsets of every free-standing 12-digit run
// in b: a maximal run of exactly 12 digits not preceded by a decimal point (a
// float mantissa) and not followed by an exponent. Go regexps have no
// lookaround, and a pattern that consumes the neighbouring bytes misses the
// second of two adjacent IDs, so this scans instead.
func freeRuns(b []byte) [][2]int {
	var runs [][2]int
	isDigit := func(c byte) bool { return c >= '0' && c <= '9' }
	for i := 0; i < len(b); {
		if !isDigit(b[i]) {
			i++
			continue
		}
		j := i
		for j < len(b) && isDigit(b[j]) {
			j++
		}
		mantissa := i > 0 && b[i-1] == '.'
		exponent := j < len(b) && (b[j] == 'e' || b[j] == 'E')
		if j-i == 12 && !mantissa && !exponent && !uuidTail(b, i, j) {
			runs = append(runs, [2]int{i, j})
		}
		i = j
	}
	return runs
}

// uuidPrefix is the 8-4-4-4- part of a UUID.
var uuidPrefix = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-$`)

// uuidTail reports whether the digit run b[i:j] is a UUID's last group (a
// task or job ID whose final 12 hex characters happen to be digits, about 1
// UUID in 280), not an account ID.
func uuidTail(b []byte, i, j int) bool {
	isHex := func(c byte) bool {
		return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
	}
	if i < 24 || (i > 24 && isHex(b[i-25])) || (j < len(b) && isHex(b[j])) {
		return false
	}
	return uuidPrefix.Match(b[i-24 : i])
}

// Scrub returns b with account IDs and IAM principal names replaced.
func Scrub(b []byte) []byte {
	b = arnAccount.ReplaceAll(b, []byte("${1}"+Placeholder+"${2}"))
	b = bucketAccount.ReplaceAll(b, []byte("${1}"+Placeholder))
	b = principal.ReplaceAll(b, []byte("${1}"+PrincipalPlaceholder))
	out := append([]byte(nil), b...)
	for _, r := range freeRuns(b) {
		copy(out[r[0]:r[1]], Placeholder)
	}
	return out
}

// Find returns every account-ID-like value in b other than the placeholder.
// A scrubbed file returns nothing.
func Find(b []byte) []string {
	var found []string
	for _, r := range freeRuns(b) {
		if v := string(b[r[0]:r[1]]); v != Placeholder {
			found = append(found, v)
		}
	}
	for _, m := range arnAccount.FindAllSubmatch(b, -1) {
		if v := string(m[0]); !strings.Contains(v, Placeholder) {
			found = append(found, v)
		}
	}
	for _, m := range principalAny.FindAllSubmatch(b, -1) {
		if string(m[2]) != PrincipalPlaceholder {
			found = append(found, string(m[0]))
		}
	}
	return found
}

// Error wraps err so its message is scrubbed while the cause stays reachable
// through errors.Is and errors.As. Nil stays nil.
func Error(err error) error {
	if err == nil {
		return nil
	}
	return scrubbedError{err}
}

type scrubbedError struct{ err error }

func (s scrubbedError) Error() string { return string(Scrub([]byte(s.err.Error()))) }
func (s scrubbedError) Unwrap() error { return s.err }

// Display is s scrubbed, with each account ID shown as "<account>" for
// people to read. Only placeholders in account positions are replaced: a
// UUID whose last group happens to be zeros stays as it is.
func Display(s string) string {
	b := Scrub([]byte(s))
	var out []byte
	last := 0
	for _, r := range freeRuns(b) {
		if string(b[r[0]:r[1]]) != Placeholder {
			continue
		}
		out = append(out, b[last:r[0]]...)
		out = append(out, "<account>"...)
		last = r[1]
	}
	return string(append(out, b[last:]...))
}
