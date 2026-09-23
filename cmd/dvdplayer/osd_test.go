// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2026 The Media Authors

package main

import (
	"strings"
	"testing"
	"time"

	"codeberg.org/totallygamerjet/media/discdb"
)

// osdGame is a player with just enough of a disc to ask it where it is.
func osdGame() *game { return &game{d: &disc{}} }

// identified is a lookup that came back with the film named.
func identified(title string) discDbAnswer {
	return discDbAnswer{
		matches: []discdb.Match{{
			Title: discdb.Metadata{Title: title},
		}},
	}
}

// TestOSDHiddenByDefault checks that the status line is down until the
// viewer asks for it, and that the film's name goes down with it.
func TestOSDHiddenByDefault(t *testing.T) {
	g := osdGame()
	g.discDb = identified("A Christmas Carol")
	if g.osd {
		t.Error("the status line should start hidden")
	}
	if got := g.osdText(); got != "" {
		t.Errorf("with the status line down the screen says %q, want nothing", got)
	}

	g.osd = true
	got := g.osdText()
	if !strings.HasPrefix(got, "A Christmas Carol\n") {
		t.Errorf("the status line is %q, want it to start with the film's name", got)
	}
	if !strings.Contains(got, "title 0 chapter 0") {
		t.Errorf("the status line is %q, want it to say where the disc is", got)
	}
}

// TestOSDName checks what the status line calls the disc: what the
// database said where it answered, and the name the disc gives itself
// until then.
func TestOSDName(t *testing.T) {
	tests := []struct {
		name   string
		label  string
		answer discDbAnswer
		want   string
	}{
		{"the database answered", "CCS-0N-NW1.1_DES", identified("A Christmas Carol"), "A Christmas Carol"},
		{"not answered yet", "CCS-0N-NW1.1_DES", discDbAnswer{}, "CCS-0N-NW1.1_DES"},
		{"not in the database", "CCS-0N-NW1.1_DES", discDbAnswer{err: discdb.ErrNotFound}, "CCS-0N-NW1.1_DES"},
		{"nothing names it", "", discDbAnswer{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := osdGame()
			g.osd, g.label, g.discDb = true, tt.label, tt.answer
			if got := g.name(); got != tt.want {
				t.Errorf("name = %q, want %q", got, tt.want)
			}
			first, _, _ := strings.Cut(g.osdText(), "\n")
			if tt.want == "" {
				if strings.HasPrefix(first, "CCS") {
					t.Errorf("the status line starts %q, want no name at all", first)
				}
				return
			}
			if first != tt.want {
				t.Errorf("the status line starts %q, want %q", first, tt.want)
			}
		})
	}
}

// TestOSDNote checks that a passing note is shown whether or not the
// status line is up, and that it does not leave a blank line above
// itself when the status line is down.
func TestOSDNote(t *testing.T) {
	g := osdGame()
	g.discDb = identified("A Christmas Carol")
	g.notef("subtitles on")

	got := g.osdText()
	if got != "subtitles on" {
		t.Errorf("with the status line down the screen says %q, want just the note", got)
	}

	g.osd = true
	got = g.osdText()
	if !strings.HasPrefix(got, "A Christmas Carol\n") {
		t.Errorf("the status line is %q, want the film's name first", got)
	}
	if !strings.HasSuffix(got, "\nsubtitles on") {
		t.Errorf("the status line is %q, want the note last", got)
	}
}

// TestOSDNoteExpires checks that a note goes away while the name stays,
// which is the difference between the two.
func TestOSDNoteExpires(t *testing.T) {
	g := osdGame()
	g.osd = true
	g.discDb = identified("A Christmas Carol")
	g.message, g.messageUntil = "subtitles on", time.Now().Add(-time.Second)

	got := g.osdText()
	if strings.Contains(got, "subtitles on") {
		t.Errorf("the status line is %q, want the note gone", got)
	}
	if !strings.HasPrefix(got, "A Christmas Carol\n") {
		t.Errorf("the status line is %q, want the film's name to have stayed", got)
	}
}

// series is 1923's second disc as TheDiscDb lists it: four episodes, the
// first of them shorter than the second. Picking the longest title to
// stand for the disc named E5 whatever was playing, and on most episode
// discs the longest episode is not the first.
func series() discDbAnswer {
	ep := func(n, dvdTitle, duration, name string) discdb.Title {
		return discdb.Title{SourceFile: dvdTitle, Duration: duration,
			Item: discdb.Item{Title: name, Type: "Episode", Season: "1", Episode: n}}
	}
	return discDbAnswer{matches: []discdb.Match{{
		Title: discdb.Metadata{Title: "1923", Type: "Series"},
		Disc: discdb.Disc{Titles: []discdb.Title{
			ep("4", "1", "0:58:10", "War and the Turquoise Tide"),
			ep("5", "2", "1:09:59", "Ghost of Zebrina"),
			ep("6", "3", "0:52:03", "One Ocean Closer to God"),
		}},
	}}}
}

// films is a collection with more than one film on the disc, which is
// why there is no such thing as the disc's one main title.
func films() discDbAnswer {
	film := func(dvdTitle, name string) discdb.Title {
		return discdb.Title{SourceFile: dvdTitle, Item: discdb.Item{Title: name, Type: "MainMovie"}}
	}
	return discDbAnswer{matches: []discdb.Match{{
		Title: discdb.Metadata{Title: "Zatoichi: The Blind Swordsman", Type: "Movie"},
		Disc: discdb.Disc{Titles: []discdb.Title{
			film("1", "Zatoichi's Flashing Sword"),
			film("2", "Fight, Zatoichi, Fight"),
			film("3", "Adventures of Zatoichi"),
		}},
	}}}
}

// TestNameFollowsPlayback checks that the window title and the status
// line name what is playing — the episode or film the DVD title in
// progress holds — rather than one title chosen to stand for the disc.
func TestNameFollowsPlayback(t *testing.T) {
	tests := []struct {
		name   string
		answer discDbAnswer
		title  int32 // the DVD title playing
		menu   bool
		want   string
	}{
		{"the first episode, not the longest", series(), 1, false, "1923 — S01E04 War and the Turquoise Tide"},
		{"moving on to the next", series(), 2, false, "1923 — S01E05 Ghost of Zebrina"},
		{"a menu names no episode", series(), 2, true, "1923"},
		{"a title the listing has not got", series(), 9, false, "1923"},
		{"a film is named once, not twice", identifiedFilm(), 27, false, "A Christmas Carol"},
		{"the film playing of several", films(), 3, false, "Zatoichi: The Blind Swordsman — Adventures of Zatoichi"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := osdGame()
			g.discDb = tt.answer
			g.d.title, g.d.inMenu = tt.title, tt.menu
			if got := g.name(); got != tt.want {
				t.Errorf("name() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestNowPlayingAgreesWithName checks the two names a viewer sees - the
// window's and the Now Playing panel's - are worked out by one rule.
func TestNowPlayingAgreesWithName(t *testing.T) {
	g := osdGame()
	g.discDb = series()
	g.d.title = 2
	np := g.nowPlayingState()
	if np.title != "S01E05 Ghost of Zebrina" || np.artist != "1923" {
		t.Errorf("Now Playing = %q by %q, want the episode playing by its series", np.title, np.artist)
	}
	if want := np.artist + " — " + np.title; g.name() != want {
		t.Errorf("name() = %q, but Now Playing says %q", g.name(), want)
	}
}

// identifiedFilm is A Christmas Carol's disc, where the film is the work
// and so has no name of its own to add.
func identifiedFilm() discDbAnswer {
	return discDbAnswer{matches: []discdb.Match{{
		Title: discdb.Metadata{Title: "A Christmas Carol", Type: "Movie"},
		Disc: discdb.Disc{Titles: []discdb.Title{
			{SourceFile: "27", Item: discdb.Item{Title: "A Christmas Carol", Type: "MainMovie"}},
			{SourceFile: "47", Item: discdb.Item{Title: "Hearse", Type: "DeletedScene"}},
		}},
	}}}
}
