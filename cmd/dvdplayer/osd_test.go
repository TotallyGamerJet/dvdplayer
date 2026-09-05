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
		info: &discdb.Info{Matches: []discdb.Match{{
			Title: discdb.Metadata{Title: title},
		}}},
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
