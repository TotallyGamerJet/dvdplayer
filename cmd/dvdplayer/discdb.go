// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2026 The Media Authors

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
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
	// matches are the releases the disc belongs to, each with the disc's
	// listing, and err is what came instead of them.
	matches []discdb.Match
	// cover is the release's own artwork as a JPEG, or nil where there
	// was none to be had. It is fetched with the lookup rather than when
	// it is wanted, since what wants it is the frame loop.
	cover []byte
	err   error
}

// match returns the release to name the disc by, or nil where the lookup
// did not find one.
func (a discDbAnswer) match() *discdb.Match {
	if a.err != nil || len(a.matches) == 0 {
		return nil
	}
	return &a.matches[0]
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
// reads the disc through the same reader the navigator plays from and a
// [dvdread.Reader] is not safe for concurrent use: call this before the
// disc starts feeding the decoder.
//
// It returns nil where the disc cannot be hashed, since there is then
// nothing to look up.
func lookupDisc(nav *dvdnav.DVDNav, log *slog.Logger) *discLookup {
	hash, err := hashDisc(nav.Reader())
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
		l.answer = askDiscDb(ctx, hash, &discdb.Client{}, log)
	}()
	return l
}

// askDiscDb looks a hash up in TheDiscDb, which answers from the live
// database and gives the disc's listing in the same request.
//
// The client is passed in rather than made here so that a test can put
// it somewhere other than the internet.
func askDiscDb(ctx context.Context, hash string, api *discdb.Client, log *slog.Logger) discDbAnswer {
	a := discDbAnswer{hash: hash}
	matches, err := api.Lookup(ctx, hash)
	if err != nil {
		a.err = err
		return a
	}
	log.Debug("the disc database named the disc", "titles", len(matches[0].Disc.Titles))
	a.matches = matches
	if url := coverURL(matches[0].Release.ImageUrl); url != "" {
		a.cover = fetchCover(ctx, url, log)
	}
	return a
}

// coverWidth is the size asked for. The display it is for is small, and
// TheDiscDb resizes on request, so there is no reason to pull down the
// full sized artwork.
const coverWidth = 512

// coverURL is where a release's artwork is, or "" for a release that has
// none. TheDiscDb records the artwork as a path and resizes on request.
func coverURL(path string) string {
	if path == "" {
		return ""
	}
	return discdb.ImageBaseURL + path + "?width=" + strconv.Itoa(coverWidth)
}

// fetchCover gets the artwork at url. It reports nil where the artwork
// will not come: a disc named without its cover is worth more than no
// disc named at all.
func fetchCover(ctx context.Context, url string, log *slog.Logger) []byte {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		log.Debug("cannot ask for the cover", "err", err)
		return nil
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Debug("cannot fetch the cover", "url", url, "err", err)
		return nil
	}
	defer resp.Body.Close() //nolint:errcheck // nothing to report a failed close of a response body on
	if resp.StatusCode != http.StatusOK {
		log.Debug("cannot fetch the cover", "url", url, "status", resp.Status)
		return nil
	}
	jpeg, err := io.ReadAll(io.LimitReader(resp.Body, maxCover))
	if err != nil {
		log.Debug("cannot read the cover", "url", url, "err", err)
		return nil
	}
	log.Debug("fetched the release cover", "url", url, "bytes", len(jpeg))
	return jpeg
}

// maxCover caps what the cover may be. One resized to coverWidth runs to
// tens of kilobytes.
const maxCover = 8 << 20

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
// the sizes of the files of VIDEO_TS, in order of name.
//
// It reads them through the disc's own filesystem, which every disc has
// one of however it was opened — a device, an image, a directory — so a
// disc gives the same answer whichever way the player was pointed at it.
func hashDisc(r *dvdread.Reader) (string, error) {
	fsys, err := r.FS()
	if err != nil {
		return "", err
	}
	return discdb.HashMediaFS(fsys)
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
	printMatch(m)
	g.np.artwork(answer.cover)
}

// name is what to call what is playing, for the window title and the
// status line: the work, and the episode or the extra being played where
// it is one, as the database has them; and the name the disc gives
// itself until the database answers, or for a disc it does not have.
//
// It follows playback, so a viewer moving from one episode to the next
// sees the name change with it. Now Playing names things by the same
// rule, and the three should never disagree.
func (g *game) name() string {
	m := g.discDb.match()
	if m == nil {
		return g.label
	}
	name := workName(m)
	if _, what := playing(m, g.d.status()); what != "" {
		name += " — " + what
	}
	if name == "" {
		return g.label
	}
	return name
}

// retitle keeps the window's title on what is playing. It changes when
// that does — a title or a chapter boundary, not a frame — so the title
// bar is written only then.
func (g *game) retitle() {
	name := g.name()
	if name == g.titled {
		return
	}
	g.titled = name
	title := "dvdplay"
	if name != "" {
		title = name + " — dvdplay"
	}
	ebiten.SetWindowTitle(title)
}

// discLabel is what the disc calls itself: the name in its video
// manager, or failing that the label of the volume it was pressed as.
// Most discs leave the first blank and all of them have the second,
// though it is often no more than the title in capitals with its spaces
// taken out, and sometimes a studio's catalogue number.
func discLabel(nav *dvdnav.DVDNav) string {
	if s := strings.TrimSpace(nav.GetTitleString()); s != "" {
		return s
	}
	if s, err := nav.GetVolIDString(); err == nil {
		return strings.TrimSpace(s)
	}
	return ""
}

// workName is what to call the film or the series the disc belongs to.
func workName(m *discdb.Match) string {
	if m.Title.Title != "" {
		return m.Title.Title
	}
	return m.Title.FullTitle
}

// playing reports the entry of the disc's listing for the DVD title in
// progress, and what to call it where that is something other than the
// work itself — an episode, a featurette, a deleted scene. On a film's
// disc the film is named after the work and so gets no name of its own.
//
// It is nil in a menu, which belongs to the disc rather than to anything
// on it, and on a title the listing has no entry for. Naming what is
// being played, rather than picking one title to stand for the disc, is
// the whole of the rule: a disc may carry no film, one, or several, and
// an episode disc carries a different episode every forty minutes.
func playing(m *discdb.Match, s status) (entry *discdb.Title, name string) {
	if s.inMenu {
		return nil, ""
	}
	entry = segmentOf(&m.Disc, s.title)
	if entry == nil {
		return nil, ""
	}
	if name = entry.Item.Name(); name == workName(m) {
		name = ""
	}
	return entry, name
}

func printMatch(m *discdb.Match) {
	fmt.Printf("DiscDb:  %s", m.Title.Title)
	if m.Title.Year > 0 {
		fmt.Printf(" (%d)", m.Title.Year)
	}
	fmt.Println()
	fmt.Printf("Release: %s\n", m.Release.Title)
	fmt.Printf("Disc:    %s, %s\n", m.Disc.Name, m.Disc.Format)
	// TheDiscDb marks a film by its type, and a disc may carry none — a
	// disc of extras, or of episodes — or several, where a collection
	// puts two or three films on one.
	for _, t := range m.Disc.Titles {
		if t.Item.Type == "MainMovie" {
			fmt.Printf("Film:    %s (%s)\n", t.Item.Name(), t.Duration)
		}
	}
	// The listing names what is on the disc besides the films. Most of
	// its entries are unnamed — a ripper sees every playable title,
	// including the stubs a menu jumps through — so only the named ones
	// are worth showing.
	named := 0
	for _, t := range m.Disc.Titles {
		if t.Item.Title == "" {
			continue
		}
		named++
		fmt.Printf("  %-4s %-9s %s", t.SourceFile, t.Duration, t.Item.Name())
		if n := len(t.Item.Chapters); n > 0 {
			fmt.Printf(" (%d chapters)", n)
		}
		fmt.Println()
	}
	fmt.Printf("Titles:  %d named of %d\n", named, len(m.Disc.Titles))
	fmt.Println()
}
