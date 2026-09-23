// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2026 The Media Authors

package main

import (
	"log/slog"
	"testing"
	"time"

	"codeberg.org/totallygamerjet/media/discdb"
)

// christmasCarol is the shape of a real DVD listing: SourceFile is the
// DVD title number, zero padded on the low ones, and most entries are
// unnamed stubs a menu jumps through.
var christmasCarol = &discdb.Disc{
	Titles: []discdb.Title{
		{SourceFile: "01", Duration: "0:00:02"},
		{SourceFile: "27", Duration: "1:35:39", Item: discdb.Item{Title: "A Christmas Carol", Type: "MainMovie"}},
		{SourceFile: "45", Duration: "0:00:11", Item: discdb.Item{Title: "Robert Zemekis Introduction to Deleted Scenes", Type: "DeletedScene"}},
		{SourceFile: "47", Duration: "0:01:14", Item: discdb.Item{Title: "Hearse", Type: "DeletedScene"}},
	},
}

func TestSegmentOf(t *testing.T) {
	tests := []struct {
		name  string
		disc  *discdb.Disc
		title int32
		want  string // the Item title, or "" for no match
	}{
		{"the feature", christmasCarol, 27, "A Christmas Carol"},
		{"a deleted scene", christmasCarol, 47, "Hearse"},
		{"a zero padded number", christmasCarol, 1, ""},
		{"a title the listing has not got", christmasCarol, 28, ""},
		{"no listing", nil, 27, ""},
		{"no title yet", christmasCarol, 0, ""},
		{
			// A Blu-ray records a playlist file rather than a number,
			// so a DVD title number matches nothing on one.
			"a Blu-ray listing",
			&discdb.Disc{Titles: []discdb.Title{{SourceFile: "00020.mpls", Item: discdb.Item{Title: "Fackham Hall"}}}},
			20,
			"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seg := segmentOf(tt.disc, tt.title)
			switch {
			case seg == nil && tt.want != "":
				t.Errorf("no entry for title %d, want %q", tt.title, tt.want)
			case seg != nil && seg.Item.Title != tt.want:
				t.Errorf("title %d is %q, want %q", tt.title, seg.Item.Title, tt.want)
			}
		})
	}
}

// TestSegmentOfZeroPadded checks the entry the listing writes as "01" is
// found by the number it is, not only by the string it was written as.
func TestSegmentOfZeroPadded(t *testing.T) {
	disc := &discdb.Disc{Titles: []discdb.Title{
		{SourceFile: "01", Item: discdb.Item{Title: "Warning"}},
		{SourceFile: "09", Item: discdb.Item{Title: "Trailer"}},
	}}
	for title, want := range map[int32]string{1: "Warning", 9: "Trailer"} {
		seg := segmentOf(disc, title)
		if seg == nil {
			t.Errorf("no entry for title %d, want %q", title, want)
			continue
		}
		if seg.Item.Title != want {
			t.Errorf("title %d is %q, want %q", title, seg.Item.Title, want)
		}
	}
}

func TestNowPlayingNames(t *testing.T) {
	base := nowPlayingState{
		title: "Hearse", artist: "A Christmas Carol", album: "2010-DVD",
		duration: 74 * time.Second, elapsed: 10 * time.Second, playing: true,
	}

	same := base
	same.elapsed = 60 * time.Second
	if !base.names(same) {
		t.Error("two states differing only in elapsed time should name the same thing")
	}
	for _, change := range []func(*nowPlayingState){
		func(s *nowPlayingState) { s.title = "Clothesline" },
		func(s *nowPlayingState) { s.artist = "" },
		func(s *nowPlayingState) { s.album = "2011-DVD" },
		func(s *nowPlayingState) { s.duration = 75 * time.Second },
		func(s *nowPlayingState) { s.playing = false },
	} {
		other := base
		change(&other)
		if base.names(other) {
			t.Errorf("%+v should not name the same thing as %+v", other, base)
		}
	}
}

// TestNowPlayingDrift checks the rule that catches a seek: the system
// counts the elapsed time on by itself while the disc is playing, so it
// only needs telling when the disc has gone somewhere that counting
// would not have reached.
func TestNowPlayingDrift(t *testing.T) {
	tests := []struct {
		name    string
		playing bool
		since   time.Duration
		now     time.Duration
		want    bool
	}{
		{"playing on undisturbed", true, 5 * time.Second, 15 * time.Second, false},
		{"paused and still there", false, 5 * time.Second, 10 * time.Second, false},
		{"paused but moved", false, 5 * time.Second, 40 * time.Second, true},
		{"seeked forwards", true, 1 * time.Second, 5 * time.Minute, true},
		{"seeked backwards", true, 1 * time.Second, 0, true},
		{"a second of slop", true, 5 * time.Second, 16 * time.Second, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := &game{
				npLast: nowPlayingState{elapsed: 10 * time.Second, playing: tt.playing},
				npSent: time.Now().Add(-tt.since),
			}
			if got := g.npDrifted(nowPlayingState{elapsed: tt.now}); got != tt.want {
				t.Errorf("npDrifted = %v, want %v", got, tt.want)
			}
		})
	}
}

// recordingNowPlaying keeps what the system was told.
type recordingNowPlaying struct{ got []nowPlayingState }

func (r *recordingNowPlaying) update(s nowPlayingState) { r.got = append(r.got, s) }
func (r *recordingNowPlaying) artwork([]byte)           {}
func (r *recordingNowPlaying) close()                   {}

// TestNowPlayingWithoutAName checks that a disc nothing names still gets
// its transport and its clock onto the system's display. A disc ripped
// to an image, or one the database has not got, is still being played,
// and the play state and elapsed time are worth having with or without
// a title.
func TestNowPlayingWithoutAName(t *testing.T) {
	for _, label := range []string{"", "THE_LAST_JEDI"} {
		t.Run("label "+label, func(t *testing.T) {
			rec := &recordingNowPlaying{}
			g := &game{d: &disc{}, np: rec, npLog: slog.New(slog.DiscardHandler), label: label}
			g.d.streamTime, g.d.pgcLength = 90000*42, 90000*9102

			g.updateNowPlaying()
			if len(rec.got) != 1 {
				t.Fatalf("the system was told %d times, want once", len(rec.got))
			}
			s := rec.got[0]
			if s.title != label {
				t.Errorf("title = %q, want what the disc calls itself, %q", s.title, label)
			}
			if !s.playing || s.elapsed != 42*time.Second || s.duration != 9102*time.Second {
				t.Errorf("state = %+v, want playing at 42s of 2h31m42s", s)
			}
		})
	}
}
