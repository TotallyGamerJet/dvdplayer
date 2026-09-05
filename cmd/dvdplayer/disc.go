// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2026 The Media Authors

package main

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"codeberg.org/totallygamerjet/media/dvdnav"
	"codeberg.org/totallygamerjet/media/dvdread"
	"codeberg.org/totallygamerjet/media/mpg"
)

// queueBlocks is the number of DVD blocks held between the navigator and
// the demuxer. It is deliberately small: the events the disc raises —
// a menu highlight, a still, a palette change — apply to the picture the
// viewer is about to see, and every queued block puts the navigator
// further ahead of the screen.
const queueBlocks = 8

// A disc drives a DVD through the navigator on a goroutine of its own.
// The program stream it walks past is handed to the demuxer through
// [disc.Read]; the events the disc raises along the way are folded into
// state the player reads while it draws.
type disc struct {
	nav *dvdnav.DVDNav
	log *slog.Logger

	mu   sync.Mutex
	cond *sync.Cond

	// The block queue is a ring of fixed buffers, so a disc playing at
	// full rate allocates nothing.
	ring              [queueBlocks][dvdread.DVDVideoLBLen]byte
	lens              [queueBlocks]int
	head, tail, count int
	off               int // how much of the block at head has been read

	// err is what the navigator stopped with, reported once the queue
	// has drained. closed is set by Close and unblocks both ends.
	err    error
	closed bool

	// started reports that the navigator's goroutine is running, and done
	// is closed when it returns. Close waits on it: tearing the navigator
	// down while that goroutine is still inside it would pull the disc out
	// from under a read in progress.
	started bool
	done    chan struct{}

	// jumped says the navigator has left for somewhere else and the
	// next read is the first of the new stream. It is handed to the
	// reader as errDiscontinuity so that what is buffered from before
	// the jump can be thrown away rather than played over the top of
	// what the viewer asked for.
	jumped bool

	// still records the still frame the navigator is holding on, if any.
	// The navigator's goroutine waits there until release says how the
	// still ended.
	still    bool
	stillLen int
	release  stillRelease

	// pci is the highlight and command information of the NAV packet the
	// navigator last read. It is replaced rather than written through, so
	// a reader may keep using the pointer after dropping the lock.
	pci *dvdread.PCI

	// clut is the subpicture colour lookup table of the current program
	// chain, 16 entries of opaque YCrCb.
	clut [16]uint32

	// button is the highlighted button, 1 to 36, or 0 for none.
	button int32

	// audio and spu are the physical stream numbers the disc asked for,
	// or -1 when the stream is switched off. spu holds the number for
	// each display mode: wide, letterbox and pan&scan.
	audio int
	spu   [3]int

	// audioID is the elementary stream the soundtrack is carried in, and
	// audioOK reports that it is known.
	audioID streamID
	audioOK bool

	// subLang is the language to start a title's subtitles in, packed
	// as a DVD stores it, or 0 to take whatever the disc offers first.
	// subLangTitle is the title it was last applied to, so that a title
	// gets it once and the viewer's own choice stands for the rest of
	// it.
	subLang      uint16
	subLangTitle int32

	// spuOn reports whether subpictures should be shown. It starts
	// false, the way a set-top player starts with its subtitles off, and
	// u turns them on.
	//
	// What that leaves on screen is what the disc marked for forced
	// display: the menu overlays a highlight is painted into, so that a
	// subtitle switch does not turn the menus off, and the subtitles a
	// disc means to be read whatever the setting — a warning card, a
	// line of dialogue in another language. On A Christmas Carol the
	// menu's subpicture is forced and the feature's subtitles are not,
	// which is the arrangement this relies on.
	spuOn bool

	// title, part and domain describe where on the disc we are.
	title, part int32
	inMenu      bool

	// pgcLength is the length of the current program chain and
	// streamTime how far into it the navigator has got, both in PTS
	// ticks. aspect is the shape of the picture: 0 for 4:3 and 3 for
	// 16:9.
	//
	// The navigator works these out from virtual machine state that its
	// own goroutine is busy changing, so they are read here rather than
	// asked for again while drawing.
	pgcLength  int64
	streamTime int64
	aspect     uint8
}

// A stillRelease says how a still frame ended.
type stillRelease int

const (
	stillHolding  stillRelease = iota // the still is still on screen
	stillTimedOut                     // it ran its course, or the viewer skipped it
	stillResolved                     // an action left the cell, so no skip is needed
)

// openDisc opens the DVD at path — a device, an ISO image or a directory
// holding a VIDEO_TS tree — and starts walking it.
func openDisc(path string, logger *slog.Logger, useCSS, readahead bool) (*disc, error) {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	// Where the disc comes from is the one thing that differs between a
	// program with a filesystem under it and one running in a browser.
	nav, err := openNav(path, logger, useCSS)
	if err != nil {
		return nil, err
	}
	ra := int32(0)
	if readahead {
		ra = 1
	}
	if err := nav.SetReadAheadFlag(ra); err != nil {
		return nil, errors.Join(err, nav.Close())
	}

	d := &disc{
		nav:   nav,
		log:   logger,
		audio: -1,
		done:  make(chan struct{}),
	}
	d.spu = [3]int{-1, -1, -1}
	d.cond = sync.NewCond(&d.mu)
	return d, nil
}

// start begins playback. It is separate from openDisc so that the caller
// can position the disc — at a title, say — before the stream runs.
func (d *disc) start() {
	d.mu.Lock()
	d.started = true
	d.mu.Unlock()
	go d.run()
}

// Close stops the navigator and releases the disc. A blocked Read or a
// blocked navigator goroutine returns.
//
// It waits for the navigator's goroutine to come back before closing the
// navigator itself: that goroutine may be part way through a read, and
// closing underneath it takes away the reader it is using.
func (d *disc) Close() error {
	d.mu.Lock()
	d.closed = true
	started := d.started
	d.cond.Broadcast()
	d.mu.Unlock()

	if started {
		<-d.done
	}
	return d.nav.Close()
}

// errDiscontinuity is what [disc.Read] reports where the disc has jumped:
// everything after it belongs somewhere else on the disc, and what the
// reader still holds from before it is of no further use.
var errDiscontinuity = errors.New("dvdplay: the disc jumped")

// Read hands the demuxer the next of the program stream. It blocks while
// the navigator has nothing to give — during a still frame, say — and
// reports io.EOF once the disc has been played to its end, or
// errDiscontinuity where the disc has jumped.
func (d *disc) Read(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	for {
		switch {
		case d.closed:
			return 0, io.EOF
		case d.jumped:
			// Reported before any of the new stream, and only once.
			d.jumped = false
			return 0, errDiscontinuity
		case d.count > 0:
		case d.err != nil:
			return 0, d.err
		default:
			d.cond.Wait()
			continue
		}
		break
	}

	n := copy(p, d.ring[d.head][d.off:d.lens[d.head]])
	d.off += n
	if d.off == d.lens[d.head] {
		d.off = 0
		d.head = (d.head + 1) % queueBlocks
		d.count--
		d.cond.Broadcast()
	}
	return n, nil
}

// run is the navigator's goroutine: it walks the disc block by block and
// acts on everything the disc asks for.
func (d *disc) run() {
	defer close(d.done)

	var buf [dvdread.DVDVideoLBLen]byte
	for {
		block, err := d.nav.GetNextBlock(buf[:])
		if err != nil {
			d.finish(err)
			return
		}

		switch block.Event {
		case dvdnav.EventBlockOK, dvdnav.EventNavPacket:
			// The NAV packet is part of the stream too, so it goes to
			// the demuxer along with everything else; it is a private
			// stream the demuxer skips over.
			if block.Event == dvdnav.EventNavPacket {
				d.setPCI()
				d.setTime()
			}
			if !d.push(block.Data[:block.Len]) {
				return
			}

		case dvdnav.EventStillFrame:
			still := block.Payload.(*dvdnav.StillEvent)
			d.log.Debug("still frame", "hold", stillLength(still.Length))
			if !d.holdStill(still.Length) {
				return
			}

		case dvdnav.EventWait:
			// The player buffers little and the queue drains on its
			// own, so there is nothing left to wait for.
			d.log.Debug("waiting for the player to catch up")
			if !d.drain() {
				return
			}
			if err := d.nav.WaitSkip(); err != nil {
				d.log.Debug("skipping the wait", "error", err)
			}

		case dvdnav.EventHopChannel:
			// A jump: what is queued belongs to where we came from, and
			// so does everything the player has decoded from it.
			if !d.drain() {
				return
			}
			d.log.Debug("the disc jumped: dropping what was buffered")
			d.mu.Lock()
			d.jumped = true
			d.cond.Broadcast()
			d.mu.Unlock()

		case dvdnav.EventSPUCLUTChange:
			clut := block.Payload.([]uint32)
			d.log.Debug("subpicture palette changed")
			d.mu.Lock()
			copy(d.clut[:], clut)
			d.mu.Unlock()

		case dvdnav.EventSPUStreamChange:
			e := block.Payload.(*dvdnav.SPUStreamChangeEvent)
			d.log.Debug("subpicture stream changed", "logical", e.Logical,
				"wide", e.PhysicalWide, "letterbox", e.PhysicalLetterbox,
				"panscan", e.PhysicalPanScan)
			d.mu.Lock()
			d.spu = [3]int{e.PhysicalWide, e.PhysicalLetterbox, e.PhysicalPanScan}
			d.mu.Unlock()

		case dvdnav.EventAudioStreamChange:
			e := block.Payload.(*dvdnav.AudioStreamChangeEvent)
			d.setAudioStream(e.Physical)
			id, ok := d.audioStream()
			d.log.Debug("soundtrack changed", "logical", e.Logical, "physical", e.Physical,
				"stream", fmt.Sprintf("%#02x/%#02x", id.id, id.substream), "known", ok)

		case dvdnav.EventHighlight:
			e := block.Payload.(*dvdnav.HighlightEvent)
			d.log.Debug("button highlighted", "button", e.ButtonN)
			d.mu.Lock()
			d.button = int32(e.ButtonN)
			d.mu.Unlock()

		case dvdnav.EventCellChange:
			e := block.Payload.(*dvdnav.CellChangeEvent)
			title, part, err := d.nav.CurrentTitleInfo()
			d.mu.Lock()
			d.pgcLength = e.PgcLength
			if err == nil {
				d.title, d.part, d.inMenu = title, part, title == 0
			}
			d.mu.Unlock()
			if err == nil && title > 0 && title != d.subLangTitle {
				// A title is given the subtitles asked for as it is
				// reached, and left alone after that: within a title
				// the viewer's own choice is the one that stands.
				d.subLangTitle = title
				d.chooseSubpicture()
			}
			d.setTime()
			where := "a menu"
			if err == nil && title > 0 {
				where = fmt.Sprintf("title %d chapter %d", title, part)
			}
			d.log.Debug("cell changed", "playing", where, "cell", e.CellN,
				"program", e.PgN, "sectors", e.CellLength,
				"chain", ticks(uint64(e.PgcLength)))

		case dvdnav.EventVTSChange:
			e := block.Payload.(*dvdnav.VTSChangeEvent)
			// The shape of the picture only changes here.
			aspect, err := d.nav.GetVideoAspect()
			if err == nil {
				d.mu.Lock()
				d.aspect = aspect
				d.mu.Unlock()
			}
			d.log.Debug("video title set changed",
				"from", fmt.Sprintf("%d (%s)", e.OldVTSN, e.OldDomain),
				"to", fmt.Sprintf("%d (%s)", e.NewVTSN, e.NewDomain),
				"aspect", aspectName(aspect))

		case dvdnav.EventNop:

		case dvdnav.EventStop:
			d.log.Debug("the disc has been played to its end")
			d.finish(io.EOF)
			return
		}
	}
}

// push queues one block for the demuxer, waiting for room. It reports
// whether the disc is still open.
func (d *disc) push(block []byte) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	for d.count == queueBlocks {
		if d.closed {
			return false
		}
		d.cond.Wait()
	}
	d.lens[d.tail] = copy(d.ring[d.tail][:], block)
	d.tail = (d.tail + 1) % queueBlocks
	d.count++
	d.cond.Broadcast()
	return true
}

// drain waits for the demuxer to take everything queued, so that what
// the navigator does next lines up with what is on screen. It reports
// whether the disc is still open.
func (d *disc) drain() bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	for d.count > 0 {
		if d.closed {
			return false
		}
		d.cond.Wait()
	}
	return true
}

// holdStill parks the navigator on a still frame for length seconds, or
// until the viewer moves on when length is 0xff. It reports whether the
// disc is still open.
func (d *disc) holdStill(length int) bool {
	if !d.drain() {
		return false
	}

	d.mu.Lock()
	d.still, d.stillLen, d.release = true, length, stillHolding
	if length != 0xff {
		// Time the still from the moment the queue ran dry, which is
		// about when its frame reaches the screen.
		t := time.AfterFunc(time.Duration(length)*time.Second, func() {
			d.mu.Lock()
			defer d.mu.Unlock()
			if d.release == stillHolding {
				d.release = stillTimedOut
				d.cond.Broadcast()
			}
		})
		defer t.Stop()
	}
	for d.release == stillHolding {
		if d.closed {
			d.mu.Unlock()
			return false
		}
		d.cond.Wait()
	}
	release := d.release
	d.still = false
	d.mu.Unlock()

	// An action that left the cell has cleared the still already; asking
	// to skip it again would eat the next one.
	if release == stillTimedOut {
		if err := d.nav.StillSkip(); err != nil {
			d.log.Debug("skipping the still", "error", err)
		}
	}
	return true
}

// finish records why the navigator stopped. Read reports it once the
// queue has drained.
func (d *disc) finish(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.err == nil {
		d.err = err
	}
	d.cond.Broadcast()
}

// setTime reads how far into the program chain the navigator has got.
// It must be called from the navigator's goroutine: the reckoning walks
// virtual machine state under no lock of its own.
func (d *disc) setTime() {
	t := d.nav.GetCurrentTime()
	d.mu.Lock()
	defer d.mu.Unlock()
	d.streamTime = t
}

// setPCI takes a copy of the navigator's current NAV packet, and with it
// the button the navigator has highlighted.
//
// The button is read back rather than left to [dvdnav.EventHighlight],
// which is raised only when the navigator's own button moves. That is
// not the same as when the player needs to know it. A menu selects its
// button as it is entered, before the NAV packets carrying its highlight
// information arrive; the packets in between carry none, which clears
// the highlight here — and it would then stay cleared for the whole of
// the menu, because the navigator's button has not moved and so raises
// nothing further. The viewer is left on a menu with no highlight on it
// until something else happens to go and ask, which is what moving the
// mouse does.
//
// It must be called from the navigator's goroutine, as the rest of the
// event handling is.
func (d *disc) setPCI() {
	pci := *d.nav.GetCurrentNavPCI()
	button, err := d.nav.GetCurrentHighlight()
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pci = &pci
	switch {
	case pci.HLI.HlGI.HliSS == 0:
		// Nothing on screen to highlight.
		d.button = 0
	case err == nil:
		d.button = button
	}
}

// setAudioStream works out which elementary stream carries the
// soundtrack the disc has selected. Which one it is depends on the
// coding: a DVD packs AC3, DTS and linear PCM into private stream 1,
// each at its own offset, and MPEG audio into a stream of its own.
func (d *disc) setAudioStream(physical int) {
	var id streamID
	ok := false
	if physical >= 0 && physical < 8 {
		if logical, err := d.nav.GetAudioLogicalStream(uint8(physical)); err == nil && logical >= 0 {
			ok = true
			switch d.nav.AudioStreamFormat(uint8(logical)) {
			case dvdnav.AudioFormatAC3:
				id = streamID{mpg.StreamPrivate1, mpg.SubstreamAC3Min + byte(physical)}
			case dvdnav.AudioFormatDTS:
				id = streamID{mpg.StreamPrivate1, mpg.SubstreamDTSMin + byte(physical)}
			case dvdnav.AudioFormatLPCM:
				id = streamID{mpg.StreamPrivate1, mpg.SubstreamLPCMMin + byte(physical)}
			case dvdnav.AudioFormatMPEG, dvdnav.AudioFormatMPEG2Ext:
				id = streamID{mpg.StreamAudioMin + byte(physical), 0}
			default:
				ok = false
			}
		}
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	d.audio = physical
	d.audioID, d.audioOK = id, ok
}

// audioStream reports which elementary stream the soundtrack is carried
// in, if the disc has said.
func (d *disc) audioStream() (streamID, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.audioID, d.audioOK
}

// subpictureStream reports the substream id of the subpicture the disc
// has selected, and whether there is one. Widescreen subpictures are
// asked for: the picture is drawn over the coded frame, before the
// aspect ratio is applied, and for 4:3 material the disc gives the same
// stream whichever display mode is asked about.
func (d *disc) subpictureStream() (byte, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.spu[0] < 0 {
		return 0, false
	}
	// The navigator sets bit 7 to say the subpictures are to be held
	// back, with only forced ones shown. That is a matter of what gets
	// drawn rather than of what gets decoded — a forced subtitle is a
	// unit of the same stream — so the bit comes off here and the
	// overlay decides. Leaving it on read as no stream at all, and the
	// player fell back to whichever subpicture turned up first, which is
	// not the one the disc chose and need not even be its language.
	stream := d.spu[0] &^ 0x80
	if stream > 31 {
		return 0, false
	}
	return mpg.SubstreamSubpictureMin + byte(stream), true
}

// chooseSubpicture picks the subpicture stream a title starts on.
//
// A disc that names none leaves the navigator to take the first that
// exists, which is whichever the disc happens to list first: A Christmas
// Carol lists French, then Spanish, then English. Where the disc has
// named one, that one is already selected and this finds it again.
//
// It must be called from the navigator's goroutine, as the rest of the
// event handling is. A language the disc has not got leaves the
// navigator's choice alone.
func (d *disc) chooseSubpicture() {
	if d.subLang == 0 {
		return
	}
	// The count is of streams the program chain offers, but the numbers
	// it offers them under are not necessarily the first few, so every
	// number is tried and SetActiveStream turns away the ones the chain
	// has not got.
	for stream := range uint8(32) {
		attr, err := d.nav.GetSPUAttr(stream)
		if err != nil || attr.LangCode != d.subLang {
			continue
		}
		if err := d.nav.SetActiveStream(stream, dvdnav.SubtitleStream); err != nil {
			continue
		}
		d.log.Debug("subtitles selected by language",
			"language", langName(d.subLang), "stream", stream)
		return
	}
	d.log.Debug("no subtitles in the language asked for", "language", langName(d.subLang))
}

// langCode packs a two letter ISO 639 code the way a DVD stores one, or
// returns 0 for anything that is not one.
func langCode(s string) uint16 {
	if len(s) != 2 || s[0] < 'a' || s[0] > 'z' || s[1] < 'a' || s[1] > 'z' {
		return 0
	}
	return uint16(s[0])<<8 | uint16(s[1])
}

// langName unpacks what [langCode] packed.
func langName(code uint16) string {
	if code == 0 {
		return ""
	}
	return string([]byte{byte(code >> 8), byte(code)})
}

// endStill releases a still the navigator is holding on. resolved says
// that an action has already moved the disc off the cell.
func (d *disc) endStill(resolved bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.release != stillHolding {
		return
	}
	d.release = stillTimedOut
	if resolved {
		d.release = stillResolved
	}
	d.cond.Broadcast()
}

// status is a snapshot of where the disc is, for the player to draw and
// to act on.
type status struct {
	pci         *dvdread.PCI
	clut        [16]uint32
	button      int32
	audio       int
	spu         [3]int
	spuOn       bool
	still       bool
	stillLen    int
	title, part int32
	inMenu      bool
	pgcLength   int64
	streamTime  int64
	aspect      uint8
}

// status reports where the disc is.
func (d *disc) status() status {
	d.mu.Lock()
	defer d.mu.Unlock()
	return status{
		pci:        d.pci,
		clut:       d.clut,
		button:     d.button,
		audio:      d.audio,
		spu:        d.spu,
		spuOn:      d.spuOn,
		still:      d.still,
		stillLen:   d.stillLen,
		title:      d.title,
		part:       d.part,
		inMenu:     d.inMenu,
		pgcLength:  d.pgcLength,
		streamTime: d.streamTime,
		aspect:     d.aspect,
	}
}

// menu reports whether the current NAV packet defines buttons, that is
// whether the viewer is looking at a menu.
func (s status) menu() bool {
	return s.pci != nil && s.pci.HLI.HlGI.HliSS != 0 && s.pci.HLI.HlGI.BtnNs > 0
}

// setSPUVisible turns subpicture display on or off.
func (d *disc) setSPUVisible(on bool) {
	d.mu.Lock()
	d.spuOn = on
	d.mu.Unlock()
}

// A direction is one of the four ways a viewer can move between the
// buttons of a menu.
//
//go:generate go tool stringer -type=direction
type direction int

const (
	dirUp direction = iota
	dirDown
	dirLeft
	dirRight
)

// selectButton moves the menu highlight. It does nothing when the disc
// is not showing a menu.
func (d *disc) selectButton(dir direction) {
	s := d.status()
	if !s.menu() {
		return
	}
	var err error
	switch dir {
	case dirUp:
		err = d.nav.UpperButtonSelect(s.pci)
	case dirDown:
		err = d.nav.LowerButtonSelect(s.pci)
	case dirLeft:
		err = d.nav.LeftButtonSelect(s.pci)
	case dirRight:
		err = d.nav.RightButtonSelect(s.pci)
	}
	// A menu whose buttons do not join up in the direction pressed simply
	// leaves the highlight where it was; the keypress is not an error.
	if err != nil {
		d.log.Debug("moving the highlight", "direction", dir, "error", err)
	}
	d.syncButton()
}

// activate presses the highlighted button, or moves past the still when
// there is no menu to press. It reports the button pressed, or 0 where
// there was none, so that it can be shown pressed while the disc gets
// where the button sends it.
func (d *disc) activate() int32 {
	s := d.status()
	if s.menu() {
		if err := d.nav.ButtonActivate(s.pci); err == nil {
			d.endStill(true)
			return s.button
		}
	}
	d.endStill(false)
	return 0
}

// pointAt moves the highlight to the button under the given position in
// the 720x480 button coordinate space, and presses it when press is set.
// It reports the button pressed, as [disc.activate] does.
func (d *disc) pointAt(x, y int32, press bool) int32 {
	s := d.status()
	if !s.menu() {
		return 0
	}
	if !press {
		if err := d.nav.MouseSelect(s.pci, x, y); err != nil {
			d.log.Debug("moving the highlight to the pointer", "x", x, "y", y, "error", err)
		}
		d.syncButton()
		return 0
	}
	// The button under the pointer is the one being pressed, and the
	// activation moves the highlight to it before running its command.
	if err := d.nav.MouseActivate(s.pci, x, y); err != nil {
		d.syncButton()
		return 0
	}
	d.endStill(true)
	d.syncButton()
	return d.status().button
}

// syncButton picks up a highlight change the navigator made on the
// caller's goroutine, which raises no event of its own.
func (d *disc) syncButton() {
	button, err := d.nav.GetCurrentHighlight()
	if err != nil {
		return
	}
	d.mu.Lock()
	d.button = button
	d.mu.Unlock()
}

// jump runs a navigation command that moves the disc somewhere else,
// releasing any still that is holding the navigator.
func (d *disc) jump(cmd func() error) error {
	err := cmd()
	if err == nil {
		d.endStill(true)
	}
	return err
}

func (d *disc) menuCall(id dvdnav.MenuID) error {
	return d.jump(func() error { return d.nav.MenuCall(id) })
}
func (d *disc) nextPart() error { return d.jump(d.nav.NextPGSearch) }
func (d *disc) prevPart() error { return d.jump(d.nav.PrevPGSearch) }
func (d *disc) goUp() error     { return d.jump(d.nav.GoUp) }

// seek moves the given number of seconds through the current title.
func (d *disc) seek(delta time.Duration) error {
	now := max(d.nav.GetCurrentTime()+int64(delta.Seconds()*90000), 0)
	return d.jump(func() error { return d.nav.TimeSearch(uint64(now)) })
}

// stillLength describes how long a still frame is to be held.
func stillLength(n int) string {
	if n == 0xff {
		return "until the viewer moves on"
	}
	return fmt.Sprintf("%ds", n)
}

// aspectName describes the shape of the picture the disc reports.
func aspectName(aspect uint8) string {
	switch aspect {
	case 0:
		return "4:3"
	case 3:
		return "16:9"
	}
	return fmt.Sprintf("unknown (%d)", aspect)
}
