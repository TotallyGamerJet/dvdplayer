// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2026 The Media Authors

package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"codeberg.org/totallygamerjet/media/discdb"
	"codeberg.org/totallygamerjet/media/dvdnav"
	"codeberg.org/totallygamerjet/media/dvdread"
)

// A file is one file of VIDEO_TS, as a listing of the directory would
// give it.
type file struct {
	name string
	size int64
}

// videoTS is a VIDEO_TS awkward in the ways that matter to the hash: a
// title set with no menu VOB and one with no title VOBs at all, a title
// set split into several parts, and enough title sets that the numbers
// are only in order while they stay zero padded.
var videoTS = []file{
	{"VIDEO_TS.BUP", 262144},
	{"VIDEO_TS.IFO", 262144},
	{"VIDEO_TS.VOB", 1714176},
	{"VTS_01_0.BUP", 32768}, // no menu VOB
	{"VTS_01_0.IFO", 32768},
	{"VTS_01_1.VOB", 163840},
	{"VTS_02_0.BUP", 131072},
	{"VTS_02_0.IFO", 131072},
	{"VTS_02_0.VOB", 16384},
	{"VTS_02_1.VOB", 1073739776},
	{"VTS_02_2.VOB", 1073739776},
	{"VTS_02_3.VOB", 545832960},
	{"VTS_09_0.BUP", 112640},
	{"VTS_09_0.IFO", 112640},
	{"VTS_09_0.VOB", 16384},
	{"VTS_09_1.VOB", 10240},
	{"VTS_10_0.BUP", 98304},
	{"VTS_10_0.IFO", 98304},
	{"VTS_10_0.VOB", 16398336}, // no title VOBs
	{"VTS_11_0.BUP", 18432},
	{"VTS_11_0.IFO", 18432},
	{"VTS_11_0.VOB", 16384},
	{"VTS_11_1.VOB", 114688},
}

// TestHashDisc checks that hashing a disc through the filesystem it
// hands out gives what hashing the same tree as a directory gives. The
// two have to agree or the lookup finds nothing.
func TestHashDisc(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "TEST_DISC")
	videoTSDir := filepath.Join(dir, "VIDEO_TS")
	if err := os.MkdirAll(videoTSDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range videoTS {
		w, err := os.Create(filepath.Join(videoTSDir, f.name))
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Truncate(f.size); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}

	want, err := discdb.HashMediaDisc(dir)
	if err != nil {
		t.Fatal(err)
	}

	r, err := dvdread.Open(dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("opening the disc: %v", err)
	}
	defer func() {
		if err := r.Close(); err != nil {
			t.Errorf("closing the disc: %v", err)
		}
	}()
	got, err := hashDisc(r)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("hashDisc = %q, but hashing the directory gives %q", got, want)
	}
}

// TestHashDiscEmpty checks that a disc with nothing on it is reported
// rather than hashed to the MD5 of nothing, which would be looked up and
// could only ever be wrong.
func TestHashDiscEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "VIDEO_TS"), 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := dvdread.Open(dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Skipf("a disc with an empty VIDEO_TS does not open: %v", err)
	}
	defer r.Close() //nolint:errcheck // the test is over either way
	if _, err := hashDisc(r); err == nil {
		t.Error("hashing a disc with no files should fail")
	}
}

// TestHashDiscOnDisc hashes a real disc the way the player does — through
// the navigator that is about to play it — and checks it against the same
// disc's VIDEO_TS listed as a directory. There is no fixture for this: a
// DVD is too big to keep in the repository.
//
// DVD_TEST_DIR is a directory holding a VIDEO_TS tree — a mounted DVD, or
// a copy of one. DVD_TEST_DEVICE, if it is set as well, is that same disc
// as a device or an ISO image, which is read through its own UDF
// filesystem rather than through the mount.
func TestHashDiscOnDisc(t *testing.T) {
	dir := os.Getenv("DVD_TEST_DIR")
	if dir == "" {
		t.Skip("set DVD_TEST_DIR to a directory holding a VIDEO_TS tree")
	}
	want, err := discdb.HashMediaDisc(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s hashes to %s", dir, want)

	paths := []string{dir}
	if dev := os.Getenv("DVD_TEST_DEVICE"); dev != "" {
		paths = append(paths, dev)
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			nav, err := dvdnav.Open(path, slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := nav.Close(); err != nil {
					t.Errorf("closing the navigator: %v", err)
				}
			}()
			got, err := hashDisc(nav.Reader())
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Errorf("hashDisc = %q, want %q", got, want)
			}
		})
	}
}
