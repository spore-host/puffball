package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/spore-host/puffball/internal/scrub"
)

// TestFixturesCarryNoAccountIDs is A-scrub's fixture half (#13): no file under
// testdata/ carries an account ID or an IAM principal name (law 7). Fixtures
// come from live API responses through scripts/scrub-fixtures; this catches one
// added any other way.
func TestFixturesCarryNoAccountIDs(t *testing.T) {
	files := 0
	err := filepath.WalkDir("testdata", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		files++
		b, err := os.ReadFile(path) //nolint:gosec // the repo's own fixtures
		if err != nil {
			return err
		}
		if found := scrub.Find(b); len(found) > 0 {
			t.Errorf("%s carries %d account-like value(s); run it through scripts/scrub-fixtures", path, len(found))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files == 0 {
		t.Fatal("no files under testdata/; a scrub check that reads nothing must not pass")
	}
}
