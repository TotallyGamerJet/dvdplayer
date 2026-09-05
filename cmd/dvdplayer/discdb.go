// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2026 The Media Authors

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/hajimehoshi/ebiten/v2"

	"codeberg.org/totallygamerjet/media/discdb"
	"codeberg.org/totallygamerjet/media/dvdnav"
	"codeberg.org/totallygamerjet/media/dvdread"
)

// discDbTimeout is how long the lookup gives the network, across both
// the fetches it makes.
//
// It is generous because nothing waits on it: the disc plays whether the
// answer arrives in a second or not at all, and the only cost of waiting
// is a goroutine that outlives its usefulness. Being too short costs the
// feature outright, and there is nothing to retry with — a cold lookup
// of this host has been seen to take eleven seconds where a warm one
// takes a tenth of that.
const discDbTimeout = 30 * time.Second

// A discDbAnswer is what a finished lookup gave.
type discDbAnswer struct {
	// hash is the disc's content hash, which is worth keeping even when
	// the query failed: it is what to report, and what to retry with.
	hash string
	// info is discs/<hash>/info.json, and err what came instead of it.
	info *discdb.Info
	// disc is the full title, track and chapter listing behind
	// info's links.disc. It is nil where info.json alone was all that
	// could be had, so a reader must check it rather than assume it.
	disc *discdb.Disc
	err  error
}

// match returns the release the disc belongs to, or nil where the lookup
// did not find one.
func (a discDbAnswer) match() *discdb.Match {
	if a.err != nil || a.info == nil {
		return nil
	}
	return a.info.Primary()
}

// A discLookup is the DiscDb query for the disc being played: the content
// hash worked out from the disc's own files, and what the database had to
// say about it.
//
// The query runs on a goroutine of its own, because it waits on a network
// that may be slow or absent and nothing about playing a DVD should wait
// with it. [game.pollDiscDb] picks the answer up on a later frame.
type discLookup struct {
	// done is closed once answer is filled in, which is what publishes
	// it to the goroutine polling.
	done   chan struct{}
	answer discDbAnswer
	// cancel stops a query still in flight when the player quits.
	cancel context.CancelFunc
}

// lookupDisc hashes the disc the navigator has open and starts a DiscDb
// query for it.
//
// The hash is worked out here, on the caller's goroutine, because it
// stats the disc through the same reader the navigator plays from and a
// [dvdread.Reader] is not safe for concurrent use: call this before the
// disc starts feeding the decoder.
//
// It returns nil where the disc cannot be hashed, since there is then
// nothing to look up.
func lookupDisc(nav *dvdnav.DVDNav, log *slog.Logger) *discLookup {
	titleSets, err := nav.NumberOfTitleSets()
	if err != nil {
		log.Debug("cannot identify the disc", "err", nav.ErrToString())
		return nil
	}
	hash, err := hashDisc(nav.Reader(), int(titleSets))
	if err != nil {
		log.Debug("cannot identify the disc", "err", err)
		return nil
	}
	log.Debug("disc content hash", "hash", hash)

	ctx, cancel := context.WithTimeout(context.Background(), discDbTimeout)
	l := &discLookup{done: make(chan struct{}), cancel: cancel}
	l.answer.hash = hash
	go func() {
		defer close(l.done)
		defer cancel()

		var c discdb.Client
		l.answer.info, l.answer.err = c.Lookup(ctx, hash)
		m := l.answer.match()
		if m == nil {
			return
		}
		// info.json names only the disc's main feature, so naming
		// anything else on it — an episode, a deleted scene, a chapter
		// — takes the listing behind links.disc as well. Failing to get
		// it costs the detail, not the identification, so the answer
		// stands without it.
		disc, err := c.Disc(ctx, m.Links.Disc)
		if err != nil {
			log.Debug("cannot read the disc listing", "err", err)
			return
		}
		l.answer.disc = disc
	}()
	return l
}

// result reports what the query gave, and whether it has finished. It
// does not block, so a caller may ask on every frame.
func (l *discLookup) result() (discDbAnswer, bool) {
	select {
	case <-l.done:
		return l.answer, true
	default:
		return discDbAnswer{}, false
	}
}

// close abandons a query still in flight.
func (l *discLookup) close() {
	if l != nil {
		l.cancel()
	}
}

// hashDisc returns the DiscDb content hash of an open DVD: the MD5 over
// the sizes of the files of VIDEO_TS, in order of name. titleSets is how
// many video title sets the disc has, from
// [dvdnav.DVDNav.NumberOfTitleSets].
//
// [discdb.HashMediaDisc] wants a directory to glob, which a player given
// a device or an ISO image has not got. The files are taken from the
// disc's own filesystem instead, which gives the same answer for the same
// disc however it was opened.
func hashDisc(r discStatter, titleSets int) (string, error) {
	return discdb.HashFileSizes(videoTSSizes(r, titleSets))
}

// discStatter is the part of a [dvdread.Reader] that identifying a disc
// uses. It is an interface so that the file listing, whose whole content
// is the order it puts the files in, can be tested without a disc.
type discStatter interface {
	Stat(title int, dom dvdread.Domain) (dvdread.FileStat, error)
}

// videoTSSizes returns the sizes of the files of VIDEO_TS, sorted by
// name. A file a title set has not got — a menu VOB, most often — simply
// fails to stat and is left out, as it would be left out of a listing of
// the directory.
//
// Only the files are read, not the IFOs: the navigator has parsed those
// already, and reading VIDEO_TS.IFO again off an optical drive costs
// seconds that the viewer would spend looking at nothing.
func videoTSSizes(r discStatter, titleSets int) []int64 {
	type file struct {
		name string
		size int64
	}
	var files []file
	add := func(name string, title int, dom dvdread.Domain) {
		st, err := r.Stat(title, dom)
		if err != nil {
			return
		}
		files = append(files, file{name, st.Size})
	}

	add("VIDEO_TS.BUP", 0, dvdread.BackupFile)
	add("VIDEO_TS.IFO", 0, dvdread.InfoFile)
	add("VIDEO_TS.VOB", 0, dvdread.MenuVOBs)

	for vts := 1; vts <= titleSets; vts++ {
		add(fmt.Sprintf("VTS_%02d_0.BUP", vts), vts, dvdread.BackupFile)
		add(fmt.Sprintf("VTS_%02d_0.IFO", vts), vts, dvdread.InfoFile)
		add(fmt.Sprintf("VTS_%02d_0.VOB", vts), vts, dvdread.MenuVOBs)
		// The title VOBs are one file to dvdread and up to nine on the
		// disc, and it is the nine the hash counts.
		if st, err := r.Stat(vts, dvdread.TitleVOBs); err == nil {
			for part, size := range st.Parts {
				files = append(files, file{fmt.Sprintf("VTS_%02d_%d.VOB", vts, part+1), size})
			}
		}
	}

	slices.SortFunc(files, func(a, b file) int { return strings.Compare(a.name, b.name) })
	sizes := make([]int64, len(files))
	for i, f := range files {
		sizes[i] = f.size
	}
	return sizes
}

// pollDiscDb picks up the DiscDb answer once it has arrived and puts the
// name of what is playing in the window title, so that the disc is
// identified by what is on it rather than by its volume label.
func (g *game) pollDiscDb() {
	if g.look == nil {
		return
	}
	answer, done := g.look.result()
	if !done {
		return
	}
	g.look = nil
	g.discDb = answer

	if err := answer.err; err != nil && !errors.Is(err, discdb.ErrNotFound) {
		fmt.Printf("DiscDb:  %v\n\n", err)
		return
	}
	// A hash the database does not have is the ordinary answer for
	// anything but a well known release, and so is a hash it has filed
	// under nothing, which should not happen but says as little.
	m := answer.match()
	if m == nil {
		fmt.Printf("DiscDb:  %s is not in the database\n\n", answer.hash)
		return
	}
	printMatch(m, answer.disc)
	if name := discName(m); name != "" {
		ebiten.SetWindowTitle(name + " — dvdplay")
	}
}

// name is what to call what is playing, for the window title and the
// status line: what the database says where it has answered, and the
// name the disc gives itself until then. It stays on screen for as long
// as the status line is up rather than passing like a note, since it is
// the one thing there that does not change as the disc plays.
func (g *game) name() string {
	if m := g.discDb.match(); m != nil {
		if name := discName(m); name != "" {
			return name
		}
	}
	return g.label
}

// discName is what to call the disc: the film, or the series and the
// episode this disc opens with.
func discName(m *discdb.Match) string {
	name := m.Title.Title
	if name == "" {
		name = m.Title.FullTitle
	}
	// The feature of a film's disc is the film again, and of a disc that
	// upstream has not classified it is whatever the ripper called the
	// file, so only an episode is worth adding.
	if m.Disc.Feature.Type == "Episode" && m.Disc.Feature.Title != "" {
		name += " — " + m.Disc.Feature.Title
	}
	return name
}

func printMatch(m *discdb.Match, disc *discdb.Disc) {
	fmt.Printf("DiscDb:  %s", m.Title.Title)
	if m.Title.Year > 0 {
		fmt.Printf(" (%d)", m.Title.Year)
	}
	fmt.Println()
	fmt.Printf("Release: %s\n", m.Release.Title)
	fmt.Printf("Disc:    %s, %s\n", m.Disc.Name, m.Disc.Format)
	if f := m.Disc.Feature; f.Title != "" {
		fmt.Printf("Feature: %s (%s)\n", f.Title, f.Duration)
	}
	// The listing names what is on the disc besides the feature. Most of
	// its entries are unnamed — a ripper sees every playable title,
	// including the stubs a menu jumps through — so only the named ones
	// are worth showing.
	if disc != nil {
		named := 0
		for _, t := range disc.Titles {
			if t.Item.Title == "" {
				continue
			}
			named++
			fmt.Printf("  %-4s %-9s %s", t.SourceFile, t.Duration, t.Item.Title)
			if n := len(t.Item.Chapters); n > 0 {
				fmt.Printf(" (%d chapters)", n)
			}
			fmt.Println()
		}
		fmt.Printf("Titles:  %d named of %d\n", named, len(disc.Titles))
	}
	fmt.Println()
}
