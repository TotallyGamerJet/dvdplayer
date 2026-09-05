// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2026 The Media Authors

package main

import (
	"strconv"
	"time"

	"codeberg.org/totallygamerjet/media/discdb"
	"codeberg.org/totallygamerjet/media/dvdnav"
)

// nowPlayingState is what the operating system is told about the disc.
// Everything but the elapsed time identifies what is playing; the elapsed
// time says where in it the disc is.
type nowPlayingState struct {
	// title is what is playing — the episode or the featurette where the
	// disc names one, the film otherwise — and artist and album are what
	// it belongs to: the work and the release it was pressed for.
	title  string
	artist string
	album  string

	elapsed  time.Duration
	duration time.Duration
	playing  bool
}

// names reports whether two states say the same thing about what is
// playing, leaving aside how far into it the disc has got.
func (s nowPlayingState) names(t nowPlayingState) bool {
	return s.title == t.title && s.artist == t.artist && s.album == t.album &&
		s.duration == t.duration && s.playing == t.playing
}

// empty reports that there is nothing to call the disc, and so nothing
// worth putting on the system's display.
func (s nowPlayingState) empty() bool { return s.title == "" }

// A nowPlayingCommand is a transport control the system worked, rather
// than the viewer: a media key, or a button in the Now Playing panel.
type nowPlayingCommand int

const (
	npPlay nowPlayingCommand = iota
	npPause
	npToggle
	npNext
	npPrev
)

// nowPlaying is the operating system's own display of what is playing.
// Only macOS has one here; everywhere else newNowPlaying returns
// nopNowPlaying, whose methods do nothing, so the player has no platform
// tests around what it reports.
type nowPlaying interface {
	// update tells the system what is playing now. It is called only
	// when that has changed.
	update(nowPlayingState)
	// close takes the player back off the system's display.
	close()
}

// nopNowPlaying is what a platform with nowhere to put this gets.
type nopNowPlaying struct{}

func (nopNowPlaying) update(nowPlayingState) {}
func (nopNowPlaying) close()                 {}

// nowPlayingDrift is how far the system's idea of where the disc is may
// wander from the truth before it is told again. The system works the
// elapsed time out for itself from the rate it was last given, so it
// needs telling when the disc changes what it is doing, not on every
// frame — but a seek moves the disc without changing anything else, and
// this is what catches it.
const nowPlayingDrift = 2 * time.Second

// updateNowPlaying reports what is playing to the system, if it has
// changed since the last time it was told.
func (g *game) updateNowPlaying() {
	if g.np == nil {
		return
	}
	s := g.nowPlayingState()
	if s.empty() {
		// Nothing names this disc: it is not in the database and it
		// does not name itself. An empty panel reads as a broken one,
		// so there is none.
		return
	}
	if s.names(g.npLast) && !g.npDrifted(s) {
		return
	}
	// What the system does with this is not visible from in here, so
	// the only account of it is the one kept on the way out.
	g.npLog.Debug("now playing", "title", s.title, "artist", s.artist, "album", s.album,
		"elapsed", s.elapsed, "duration", s.duration, "playing", s.playing)
	g.np.update(s)
	g.npLast, g.npSent = s, time.Now()
}

// npDrifted reports whether the system would now be showing an elapsed
// time that the disc has left behind.
func (g *game) npDrifted(s nowPlayingState) bool {
	expect := g.npLast.elapsed
	if g.npLast.playing {
		expect += time.Since(g.npSent)
	}
	d := s.elapsed - expect
	if d < 0 {
		d = -d
	}
	return d > nowPlayingDrift
}

// nowPlayingState works out what to tell the system from where the disc
// is and what the database said about it.
//
// Until the lookup comes back — and afterwards, for a disc the database
// does not have — all there is to go on is the name the disc gives
// itself, which many discs leave blank.
func (g *game) nowPlayingState() nowPlayingState {
	s := g.d.status()
	np := nowPlayingState{
		title:    g.label,
		elapsed:  ticks(uint64(max(s.streamTime, 0))),
		duration: ticks(uint64(max(s.pgcLength, 0))),
		playing:  !g.paused && !s.still,
	}
	m := g.discDb.match()
	if m == nil {
		return np
	}

	work := m.Title.Title
	if work == "" {
		work = m.Title.FullTitle
	}
	np.title = work
	if np.album = m.Release.Title; np.album == "" {
		np.album = m.Disc.Name
	}
	if s.inMenu {
		// A menu belongs to the disc rather than to anything on it, so
		// the work is all there is to say.
		return np
	}

	seg := segmentOf(g.discDb.disc, s.title)
	if seg == nil {
		return np
	}
	if name := seg.Item.Title; name != "" && name != work {
		// An episode, a deleted scene, a featurette: the thing playing
		// is what to show, and the work is what it belongs to.
		np.title, np.artist = name, work
	}
	if np.artist == "" {
		// A film has nothing to put under its own name, so the chapter
		// goes there where the disc names its chapters.
		np.artist = g.chapterName(seg, s.title, s.part)
	}
	return np
}

// segmentOf returns the entry of the disc's listing for the DVD title
// being played, or nil where there is none.
//
// TheDiscDb records a DVD title's SourceFile as its title number, in
// decimal and sometimes zero padded. On a Blu-ray it is a playlist file
// name instead, which no DVD title number parses as, so a Blu-ray
// listing simply matches nothing.
func segmentOf(disc *discdb.Disc, title int32) *discdb.Title {
	if disc == nil || title <= 0 {
		return nil
	}
	for i := range disc.Titles {
		n, err := strconv.Atoi(disc.Titles[i].SourceFile)
		if err == nil && int32(n) == title {
			return &disc.Titles[i]
		}
	}
	return nil
}

// chapterName names the chapter being played, or returns "" where the
// database's names cannot be lined up with the disc's own chapters.
//
// The names carry an index, but nothing in the database says that index
// is the DVD's chapter number, and at least one disc names fewer
// chapters than it has: A Christmas Carol's feature runs to eighteen
// chapters and is given seventeen names. Where the two counts agree the
// names are in step with the disc; where they do not, naming a chapter
// would be guessing, so nothing is named.
//
// The count is asked of the navigator once per title rather than once
// per frame, since it is the same answer until the disc moves on.
func (g *game) chapterName(seg *discdb.Title, title, part int32) string {
	if g.npChapterOf != title {
		g.npChapterOf, g.npChapters = title, trustedChapters(g.d.nav, seg, title)
	}
	for _, c := range g.npChapters {
		if int32(c.Index) == part {
			return c.Title
		}
	}
	return ""
}

func trustedChapters(nav *dvdnav.DVDNav, seg *discdb.Title, title int32) []discdb.Chapter {
	if len(seg.Item.Chapters) == 0 {
		return nil
	}
	parts, err := nav.NumberOfParts(title)
	if err != nil || int(parts) != len(seg.Item.Chapters) {
		return nil
	}
	return seg.Item.Chapters
}

// runNowPlayingCommands carries out whatever the system's transport
// controls asked for since the last frame. The handlers run on a thread
// of the system's own, so they leave their work here rather than
// reaching into the player from it.
func (g *game) runNowPlayingCommands() {
	for {
		select {
		case cmd := <-g.npCommands:
			g.doNowPlayingCommand(cmd)
		default:
			return
		}
	}
}

func (g *game) doNowPlayingCommand(cmd nowPlayingCommand) {
	switch cmd {
	case npPlay:
		g.pause(false)
	case npPause:
		g.pause(true)
	case npToggle:
		g.pause(!g.paused)
	case npNext:
		if err := g.d.nextPart(); err != nil {
			g.notef("no next chapter")
		}
	case npPrev:
		if err := g.d.prevPart(); err != nil {
			g.notef("no previous chapter")
		}
	}
}
