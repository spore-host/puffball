package gate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// TokenRecord is a billable call's idempotency state.
type TokenRecord struct {
	Key         string `json:"key"`
	Region      string `json:"region"`
	ClientToken string `json:"client_token"`
	TaskArn     string `json:"task_arn,omitempty"`
	// InputHash is a SHA-256 of the request without its client token. The
	// service refuses a token reused with different input (409
	// ConflictException, measured in #35); the gate refuses it first.
	InputHash string `json:"input_hash"`
}

// TokenStore keeps TokenRecords across runs.
type TokenStore interface {
	// Claim stores r if no record exists for r.Key, and returns the record
	// that is now stored: r, or the existing one unchanged. It is atomic
	// across processes, so concurrent runs of one key share one token.
	Claim(r TokenRecord) (TokenRecord, error)
	// Put overwrites the record for r.Key (to add the task ARN).
	Put(r TokenRecord) error
}

// FileTokens stores one JSON file per key under dir. Each write goes to a
// synced temp file that is linked (Claim: fails if the key exists) or renamed
// (Put) into place, then the directory is synced, so a crash leaves the old
// record or the new one. On macOS, fsync does not force the drive's write
// cache (F_FULLFSYNC); a power loss there can still lose the last write.
//
// The default dir is [DefaultTokenDir]. The records are local state: deleting
// them loses only the retry protection for calls still in flight; tasks are
// found from the service (law 4).
type FileTokens struct{ Dir string }

// DefaultTokenDir is $XDG_STATE_HOME/puffball/tokens, falling back to
// ~/.local/state/puffball/tokens.
func DefaultTokenDir() (string, error) {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "puffball", "tokens"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("token dir: %w", err)
	}
	return filepath.Join(home, ".local", "state", "puffball", "tokens"), nil
}

func (f FileTokens) path(key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(f.Dir, hex.EncodeToString(sum[:])+".json")
}

// Get reads the record for key.
func (f FileTokens) Get(key string) (TokenRecord, bool, error) {
	b, err := os.ReadFile(f.path(key))
	if errors.Is(err, fs.ErrNotExist) {
		return TokenRecord{}, false, nil
	}
	if err != nil {
		return TokenRecord{}, false, fmt.Errorf("read client token: %w", err)
	}
	var r TokenRecord
	if err := json.Unmarshal(b, &r); err != nil {
		return TokenRecord{}, false, fmt.Errorf("read client token %s: %w", f.path(key), err)
	}
	if r.Key != key {
		return TokenRecord{}, false, fmt.Errorf("client token file %s is for key %q, not %q", f.path(key), r.Key, key)
	}
	return r, true, nil
}

// writeTemp writes r to a synced temp file in the store and returns its path.
func (f FileTokens) writeTemp(r TokenRecord) (string, error) {
	if err := os.MkdirAll(f.Dir, 0o700); err != nil {
		return "", fmt.Errorf("create token dir: %w", err)
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(f.Dir, ".tok-*")
	if err != nil {
		return "", fmt.Errorf("write client token: %w", err)
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return "", fmt.Errorf("write client token: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return "", fmt.Errorf("sync client token: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return "", fmt.Errorf("write client token: %w", err)
	}
	return tmp.Name(), nil
}

func (f FileTokens) syncDir() error {
	d, err := os.Open(f.Dir)
	if err != nil {
		return fmt.Errorf("sync token dir: %w", err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync token dir: %w", err)
	}
	return nil
}

// staleTemp is how old a leftover temp file (from a crash mid-write) must be
// before Claim removes it.
const staleTemp = time.Hour

// sweepTemps removes temp files a crashed run left behind. Best effort.
func (f FileTokens) sweepTemps() {
	old, _ := filepath.Glob(filepath.Join(f.Dir, ".tok-*"))
	for _, p := range old {
		if fi, err := os.Stat(p); err == nil && time.Since(fi.ModTime()) > staleTemp {
			_ = os.Remove(p)
		}
	}
}

// Claim links a new record into place only if none exists.
func (f FileTokens) Claim(r TokenRecord) (TokenRecord, error) {
	f.sweepTemps()
	tmp, err := f.writeTemp(r)
	if err != nil {
		return TokenRecord{}, err
	}
	defer func() { _ = os.Remove(tmp) }()
	if err := os.Link(tmp, f.path(r.Key)); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			// Hard links are how two runs agree on one token; FAT and exFAT
			// don't have them.
			return TokenRecord{}, fmt.Errorf("claim client token: the token dir %s must be on a filesystem with hard links (set XDG_STATE_HOME elsewhere): %w", f.Dir, err)
		}
		existing, ok, err := f.Get(r.Key)
		if err != nil {
			return TokenRecord{}, err
		}
		if !ok {
			return TokenRecord{}, fmt.Errorf("client token for %q vanished while claiming it", r.Key)
		}
		return existing, nil
	}
	if err := f.syncDir(); err != nil {
		return TokenRecord{}, err
	}
	return r, nil
}

// Put replaces the record for r.Key durably.
func (f FileTokens) Put(r TokenRecord) error {
	tmp, err := f.writeTemp(r)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, f.path(r.Key)); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write client token: %w", err)
	}
	return f.syncDir()
}

// MemTokens is an in-memory TokenStore for tests.
type MemTokens struct {
	mu   sync.Mutex
	recs map[string]TokenRecord
	Err  error // returned by Claim and Put when set
}

// Get reads the record for key.
func (m *MemTokens) Get(key string) (TokenRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.recs[key]
	return r, ok, nil
}

// Claim stores r unless key has a record, and returns the stored one.
func (m *MemTokens) Claim(r TokenRecord) (TokenRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return TokenRecord{}, m.Err
	}
	if old, ok := m.recs[r.Key]; ok {
		return old, nil
	}
	if m.recs == nil {
		m.recs = map[string]TokenRecord{}
	}
	m.recs[r.Key] = r
	return r, nil
}

// Put stores r, or returns Err.
func (m *MemTokens) Put(r TokenRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return m.Err
	}
	if m.recs == nil {
		m.recs = map[string]TokenRecord{}
	}
	m.recs[r.Key] = r
	return nil
}
