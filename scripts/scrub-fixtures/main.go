// Command scrub-fixtures copies raw probe output into testdata/ with account
// IDs and IAM principal names removed (law 7), and refuses to write a file that
// still carries one.
//
//	scrub-fixtures --in .probe/devices --out testdata/devices
//
// For a devices directory (one with summary.json), the full GetDevice responses
// of RETIRED devices are left out: they are large and add nothing the
// SearchDevices pages (which still list them) don't.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spore-host/puffball/internal/scrub"
)

func main() {
	in := flag.String("in", "", "raw probe output directory")
	out := flag.String("out", "", "fixture directory to write")
	flag.Parse()
	if *in == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "usage: scrub-fixtures --in DIR --out DIR")
		os.Exit(2)
	}
	n, err := run(*in, *out)
	if err != nil {
		fmt.Fprintln(os.Stderr, "scrub-fixtures:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "wrote %d scrubbed files to %s\n", n, *out)
}

// retiredFiles returns the GetDevice files of RETIRED devices listed in a
// devices summary.json, relative to the directory, or nil if there is none.
func retiredFiles(in string) (map[string]bool, error) {
	b, err := os.ReadFile(filepath.Join(in, "summary.json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s struct {
		Devices []struct{ Status, File string } `json:"devices"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parse summary.json: %w", err)
	}
	skip := map[string]bool{}
	for _, d := range s.Devices {
		if d.Status == "RETIRED" {
			skip[d.File] = true
			skip[strings.TrimSuffix(d.File, ".json")+".capabilities.json"] = true
		}
	}
	return skip, nil
}

func run(in, out string) (int, error) {
	skip, err := retiredFiles(in)
	if err != nil {
		return 0, err
	}
	n := 0
	err = filepath.WalkDir(in, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(in, path)
		if err != nil {
			return err
		}
		// The parsed capabilities copies duplicate the string inside each
		// GetDevice response; the fixture keeps only the raw response.
		if skip[rel] || strings.HasSuffix(rel, ".capabilities.json") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		clean := scrub.Scrub(b)
		if left := scrub.Find(clean); len(left) > 0 {
			return fmt.Errorf("%s still carries %d account-like value(s) after scrubbing; not written", rel, len(left))
		}
		dst := filepath.Join(out, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, clean, 0o644); err != nil {
			return err
		}
		n++
		return nil
	})
	return n, err
}
