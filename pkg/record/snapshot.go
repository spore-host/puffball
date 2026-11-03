package record

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// SnapshotSchema versions the device snapshot puff run stores at
// submission.
const SnapshotSchema = "puffball.device-snapshot/v0"

// SubmissionDir is where puff run stores what it submitted, under the
// task's results prefix, per device alias: puffball/<run-id>/submission/<alias>/.
// One run id can submit to several devices (the gate's key is
// <run-id>/<alias>), so each gets its own.
func SubmissionDir(prefix, alias string) string { return prefix + "/submission/" + alias + "/" }

// SnapshotMaxAge is how long before a task's creation its submission
// snapshot may have been read. puff run reads the device seconds before
// submitting; an older snapshot is one an earlier, failed attempt stored,
// and isn't the calibration at this submission.
const SnapshotMaxAge = 15 * time.Minute

// Submission object names, under SubmissionDir.
const (
	DeviceObject = "device.json" // a Snapshot of the device, read just before submission
	ActionObject = "action.json" // the action JSON exactly as submitted
)

// Snapshot is a device's capabilities document as GetDevice returned it, and
// when it was read.
type Snapshot struct {
	Schema    string    `json:"schema"`
	ReadAt    time.Time `json:"read_at"`
	DeviceArn string    `json:"device_arn"`
	Region    string    `json:"region"`
	// CapabilitiesSHA256 is the SHA-256 of Capabilities' bytes.
	CapabilitiesSHA256 string `json:"device_capabilities_sha256"`
	Capabilities       string `json:"device_capabilities"`
}

// NewSnapshot encodes a Snapshot.
func NewSnapshot(deviceArn, region, capabilities string, readAt time.Time) ([]byte, error) {
	if capabilities == "" {
		return nil, errors.New("device snapshot: no capabilities document")
	}
	return encode(Snapshot{Schema: SnapshotSchema, ReadAt: readAt.UTC(), DeviceArn: deviceArn, Region: region,
		CapabilitiesSHA256: Sum([]byte(capabilities)), Capabilities: capabilities}, false)
}

// ParseSnapshot decodes a Snapshot and checks its hash.
func ParseSnapshot(b []byte) (Snapshot, error) {
	var s Snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		return Snapshot{}, fmt.Errorf("device snapshot: %w", err)
	}
	if s.Schema != SnapshotSchema {
		return Snapshot{}, fmt.Errorf("device snapshot: schema %q, want %q", s.Schema, SnapshotSchema)
	}
	if Sum([]byte(s.Capabilities)) != s.CapabilitiesSHA256 {
		return Snapshot{}, errors.New("device snapshot: capabilities don't match their SHA-256")
	}
	return s, nil
}

// Sum is the hex SHA-256 of b.
func Sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// encode is JSON with HTML escaping off, so OpenQASM's < and & stay as
// written, indented or compact, without a trailing newline.
func encode(v any, indent bool) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if indent {
		enc.SetIndent("", "  ")
	}
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
