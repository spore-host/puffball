package ledger

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// cacheSchema versions a cache line; a line of another version is ignored.
const cacheSchema = "puffball.ledger-cache/v1"

// FileCache is the ledger's only local state: the billed durations read from
// S3 for finished simulator tasks, as JSON lines in one file, keyed by
// region and task UUID (no account ID). A finished
// task's duration doesn't change, so a cached one is never re-read. The file
// is only a cache. A missing, unreadable or corrupt one costs S3 reads, never
// a figure (law 4). A nil *FileCache caches nothing.
type FileCache struct {
	Path string

	mu     sync.Mutex
	loaded bool
	m      map[string]int64
	// Err is the first error loading or writing the cache, for a warning.
	Err error
}

type cacheLine struct {
	Schema     string `json:"schema"`
	Task       string `json:"task"` // <region>/<task UUID>
	DurationMs int64  `json:"duration_ms"`
}

// DefaultCachePath is $XDG_STATE_HOME/puffball/ledger/durations.jsonl,
// falling back to ~/.local/state/puffball/ledger/durations.jsonl.
func DefaultCachePath() (string, error) {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "puffball", "ledger", "durations.jsonl"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("ledger cache: %w", err)
	}
	return filepath.Join(home, ".local", "state", "puffball", "ledger", "durations.jsonl"), nil
}

func (c *FileCache) note(err error) {
	if c.Err == nil && err != nil {
		c.Err = err
	}
}

func (c *FileCache) load() {
	if c.loaded {
		return
	}
	c.loaded, c.m = true, map[string]int64{}
	f, err := os.Open(c.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		c.note(fmt.Errorf("ledger cache %s: %w", c.Path, err))
		return
	}
	defer func() { _ = f.Close() }()
	r := bufio.NewReader(f)
	for {
		b, err := r.ReadBytes('\n')
		var l cacheLine
		// A cache: a bad or torn line is re-read from S3.
		if json.Unmarshal(b, &l) == nil && l.Schema == cacheSchema && l.Task != "" && l.DurationMs >= 0 {
			c.m[l.Task] = l.DurationMs
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				c.note(fmt.Errorf("ledger cache %s: %w", c.Path, err))
			}
			return
		}
	}
}

// Get returns a cached duration.
func (c *FileCache) Get(key string) (int64, bool) {
	if c == nil {
		return 0, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.load()
	ms, ok := c.m[key]
	return ms, ok
}

// Put records a duration. A write failure is kept in Err, not returned: the
// figure doesn't depend on it.
func (c *FileCache) Put(key string, ms int64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.load()
	if old, ok := c.m[key]; ok && old == ms {
		return
	}
	c.m[key] = ms
	b, err := json.Marshal(cacheLine{Schema: cacheSchema, Task: key, DurationMs: ms})
	if err != nil {
		c.note(err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(c.Path), 0o700); err != nil {
		c.note(fmt.Errorf("ledger cache %s: %w", c.Path, err))
		return
	}
	f, err := os.OpenFile(c.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		c.note(fmt.Errorf("ledger cache %s: %w", c.Path, err))
		return
	}
	_, werr := f.Write(append(b, '\n'))
	c.note(werr)
	c.note(f.Close())
}
