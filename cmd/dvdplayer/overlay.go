// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2026 The Media Authors

package main

import (
	"image"

	"github.com/hajimehoshi/ebiten/v2"

	"codeberg.org/totallygamerjet/media/dvdnav"
	"codeberg.org/totallygamerjet/media/dvdread"
	"codeberg.org/totallygamerjet/media/mpg"
	"codeberg.org/totallygamerjet/media/spu"
)

// An overlay draws what belongs on top of the picture: the disc's
// subtitles, and the menu buttons it draws as subpictures of their own.
//
// A menu is one overlay holding every button, drawn through a palette in
// which it is entirely clear; the button the viewer has picked out is
// the part of it redrawn through the colours the NAV packet gives. So
// the highlight moving is not new picture data — it is the same overlay
// painted again with one rectangle treated differently.
type overlay struct {
	d *disc
	p *player

	// flush is the player's jump count as of the last update: an
	// overlay belongs to the menu it was drawn for and to no other.
	flush int64

	// pending holds decoded units whose time has not come, and cur the
	// one on screen.
	pending []pendingSPU
	cur     *spu.Subpicture
	shownAt float64

	// img is what gets drawn, buf the pixels behind it, and stale says
	// they need painting again because the overlay, the highlight or
	// the palette has changed.
	img   *ebiten.Image
	buf   *image.RGBA
	stale bool

	// pressed is the button the viewer has just activated, drawn in its
	// own colours until the disc arrives where the button sends it. A
	// button carries two sets of colours: how it looks picked out, and
	// how it looks the moment it is pressed. The second is the only sign
	// the viewer gets that the press took, and it has to stay up for the
	// whole of the loading — take it down and the button simply vanishes
	// off the menu that is still on screen.
	pressed int32

	// held says img was painted for a picture the disc has since left.
	held bool

	// The state img was painted from. It is kept rather than read afresh
	// so that it stays with the picture it belongs to: the navigator has
	// moved on to the next menu long before its picture is on screen.
	pci    *dvdread.PCI
	button int32
	clut   [16]uint32
}

// A pendingSPU is a decoded unit waiting for its moment.
type pendingSPU struct {
	sub *spu.Subpicture
	pts float64
}

func newOverlay(d *disc, p *player) *overlay {
	return &overlay{d: d, p: p}
}

// press notes that the viewer has activated a button, so that it can be
// shown pressed while the disc gets where it is going.
func (o *overlay) press(button int32) { o.pressed = button }

// update takes in every subpicture the demuxer has reassembled and puts
// on screen the one whose time has come. gen is the jump count of the
// picture on screen: where that is not the one the disc is now on, the
// picture is being held while the disc loads and the overlay stays with
// it rather than moving on ahead of it.
func (o *overlay) update(now float64, gen int64) {
	if gen != o.p.flush.Load() {
		return
	}
	if g := o.p.flush.Load(); g != o.flush {
		o.flush = g
		o.pending, o.cur, o.stale, o.pressed = nil, nil, true, 0
	}

	for {
		u, ok := o.p.nextSubpicture()
		if !ok {
			break
		}
		sub, err := spu.Decode(u.data)
		if err != nil {
			// A unit this decoder cannot make sense of. Dropping it
			// costs a subtitle, not the film.
			continue
		}
		o.pending = append(o.pending, pendingSPU{sub, u.pts})
	}

	// A jump leaves behind overlays that belong somewhere else, and
	// their times will not come round again.
	if len(o.pending) > 0 && o.pending[0].pts != mpg.NoPTS && o.pending[0].pts > now+5 {
		o.pending = nil
	}

	for len(o.pending) > 0 {
		p := o.pending[0]
		if p.pts != mpg.NoPTS && p.pts+p.sub.Start.Seconds() > now {
			break
		}
		o.pending = o.pending[1:]
		o.cur, o.shownAt, o.stale = p.sub, p.pts, true
	}

	if o.cur != nil && o.cur.Stop >= 0 && o.shownAt != mpg.NoPTS &&
		now >= o.shownAt+o.cur.Stop.Seconds() {
		o.cur, o.stale = nil, true
	}

	s := o.d.status()
	if s.button != o.button || s.clut != o.clut || s.pci != o.pci {
		o.button, o.clut, o.pci, o.stale = s.button, s.clut, s.pci, true
	}
}

// draw puts the overlay over the picture, which is size pixels across
// and drawn into area on screen. gen is the jump count of the picture
// below it.
//
// The overlay stays with the picture it belongs to. The disc reaches a
// menu before its picture does — the navigator has the buttons and their
// colours as soon as it reads the NAV packet, while the picture they
// belong to is still being read and decoded — so an overlay that
// followed the navigator would put the next menu's highlight over the
// last menu's picture for the whole of the loading. What is drawn over a
// held picture is the overlay that was already on it, with the button
// the viewer pressed shown pressed.
func (o *overlay) draw(screen *ebiten.Image, area image.Rectangle, size image.Point, gen int64) {
	held := gen != o.p.flush.Load()
	if o.cur == nil || size.X <= 0 || size.Y <= 0 {
		// Nothing of the disc's own to draw, so nothing is drawn. A
		// button exists from the moment the NAV packet describes it, but
		// the picture that shows the viewer where it is may be seconds
		// behind — a disc will hold an option back until its artwork has
		// arrived. Marking the button anyway puts a box over blank
		// screen where the viewer can see nothing to press. It stays
		// selectable throughout; it is only invisible, which is what the
		// disc asked for.
		return
	}
	if !o.d.status().spuOn && !o.cur.Forced {
		return
	}

	if o.buf == nil || o.buf.Bounds().Max != size {
		o.buf = image.NewRGBA(image.Rectangle{Max: size})
		o.img = ebiten.NewImage(size.X, size.Y)
		o.stale = true
	}
	if o.stale || held != o.held {
		o.paint(held)
		o.held, o.stale = held, false
	}

	var op ebiten.DrawImageOptions
	op.GeoM.Scale(float64(area.Dx())/float64(size.X), float64(area.Dy())/float64(size.Y))
	op.GeoM.Translate(float64(area.Min.X), float64(area.Min.Y))
	op.Filter = ebiten.FilterLinear
	screen.DrawImage(o.img, &op)
}

// paint renders the overlay with the highlight the disc has selected, or
// with the button the viewer pressed shown pressed where the picture is
// being held while the disc loads.
func (o *overlay) paint(held bool) {
	clear(o.buf.Pix)
	var hl *spu.Highlight

	button, mode := o.button, int32(highlightSelected)
	if held {
		button, mode = o.pressed, highlightActivated
	}
	if o.pci != nil && o.pci.HLI.HlGI.HliSS != 0 && button >= 1 {
		if area, err := dvdnav.GetHighlightArea(o.pci, button, mode); err == nil {
			// The disc gives the last column and row of the button, not
			// the one past them.
			h := spu.ButtonHighlight(image.Rect(
				int(area.Sx), int(area.Sy), int(area.Ex)+1, int(area.Ey)+1,
			), area.Palette)
			hl = &h
		}
	}
	o.cur.Draw(o.buf, o.clut, hl)
	o.img.WritePixels(o.buf.Pix)
}

// highlightSelected and highlightActivated are the two sets of colours a
// button carries: how it looks while it is picked out, and how it looks
// the moment it is pressed.
const (
	highlightSelected  = 0
	highlightActivated = 1
)
