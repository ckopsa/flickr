package main

// One truth, two languages.
//
// The renderers in web/ are pure functions of an envelope, and the only way
// to know they draw the document the SERVER writes is to feed them the
// server's own bytes. So the goldens are copied — not re-invented — into
// web/testdata/hyper/, and this test fails the moment the copy and the
// original disagree. Under `go test ./cmd/server -update` the copy is written
// by golden() beside the server's own half, in the same call: one run
// refreshes both, and no test file has to run before another for that to
// hold. All this test does under -update is drop a copy the server has
// stopped writing.
//
// A copy rather than a symlink or a relative import: `node --test` reads
// JSON off disk with no build step and no knowledge of Go's layout, and a
// checked-in copy is also the diff a reviewer sees when a document changes
// shape under the client's feet.

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// sharedGoldensDir is where the JS suites read the documents from.
const sharedGoldensDir = "../../web/testdata/hyper"

// shareGolden writes the client's copy of one golden. golden() calls it as it
// writes the server's half, so a single -update run leaves the two identical
// whatever order the test files happen to run in.
func shareGolden(t *testing.T, name string, doc []byte) {
	t.Helper()
	if err := os.MkdirAll(sharedGoldensDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sharedGoldensDir, name+".json"), doc, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestGoldensAreSharedWithTheClient(t *testing.T) {
	src := filepath.Join("testdata", "hyper")
	names, err := filepath.Glob(filepath.Join(src, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(names) == 0 {
		t.Fatalf("no goldens under %s", src)
	}
	if *update {
		// The copies are golden()'s work. What is left here is the golden
		// that has been RENAMED or dropped, which must not linger on the
		// client's side pretending to be a document the server still writes.
		stale, err := filepath.Glob(filepath.Join(sharedGoldensDir, "*.json"))
		if err != nil {
			t.Fatal(err)
		}
		keep := map[string]bool{}
		for _, n := range names {
			keep[filepath.Base(n)] = true
		}
		for _, n := range stale {
			if !keep[filepath.Base(n)] {
				if err := os.Remove(n); err != nil {
					t.Fatal(err)
				}
			}
		}
		return
	}
	for _, name := range names {
		base := filepath.Base(name)
		t.Run(base, func(t *testing.T) {
			want, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(sharedGoldensDir, base))
			if err != nil {
				t.Fatalf("%s is not shared with the client: %v\n"+
					"run: go test ./cmd/server -update", base, err)
			}
			if !bytes.Equal(want, got) {
				t.Errorf("%s has drifted from the server's golden — the JS renderers "+
					"are being tested against a document the server no longer writes.\n"+
					"run: go test ./cmd/server -update", base)
			}
		})
	}
}
