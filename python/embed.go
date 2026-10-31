// Package python embeds puff's Python local-rung runner, so the puff binary
// carries it (go:embed can't reach a parent directory from pkg/local).
package python

import _ "embed"

// Runner is puff_local.py: run an OpenQASM 3 file on the Braket default
// simulator and print one JSON object of counts and versions.
//
//go:embed puff_local.py
var Runner []byte
