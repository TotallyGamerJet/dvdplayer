// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2026 The Media Authors

package main

import (
	"flag"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"codeberg.org/totallygamerjet/media/dvdnav"
	"codeberg.org/totallygamerjet/media/spu"
)

var pictures = flag.Bool("pictures", false,
	"write what the player showed either side of the jump as PNGs")

// TestJumpToMenu presses a button on the disc's root menu and checks
// that the picture follows the disc to where it went.
//
// A menu is very often a still: a picture, a soundtrack that loops, and
// nothing more of the video stream for as long as the viewer stays. So
// the whole of the new menu's picture arrives in the first moment after
// the jump, and anything that costs the player those few packets leaves
// the previous menu on screen indefinitely — with the sound of the new
// one playing over it.
//
// Point DVD_TEST_DEVICE at a device node, an ISO image or a VIDEO_TS
// directory to run it, for example
//
//	DVD_TEST_DEVICE=/dev/rdisk4 go test ./cmd/dvdplay/ -pictures
//
// With -pictures it writes what it saw either side of the jump as PNGs,
// which is the quickest way to see what a disc actually does.
func TestJumpToMenu(t *testing.T) {
	path := os.Getenv("DVD_TEST_DEVICE")
	if path == "" {
		t.Skip("set DVD_TEST_DEVICE to a DVD device, image or VIDEO_TS directory")
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	d, err := openDisc(path, logger, true, true)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer d.Close()

	p := newPlayer(d)
	d.start()
	p.start()
	defer p.close()

	c := &clock{}
	track := newAudioTrack(p, c)
	sound := make([]byte, bytesPerSec/20)
	var played float64

	var shown, before, after *image.RGBA
	var atMenu, jumped time.Time
	start := time.Now()

	for time.Since(start) < 40*time.Second {
		if ahead := played - time.Since(start).Seconds(); ahead > 0 {
			time.Sleep(time.Duration(ahead * float64(time.Second)))
		}
		n, err := track.Read(sound)
		if err != nil {
			t.Fatalf("read sound: %v", err)
		}
		played += float64(n) / bytesPerSec

		now, ok := c.now(played)
		if head, have := p.peekFrame(); have && !ok {
			c.set(played, head)
			now, ok = head, true
		}
		if ok {
			for {
				f, more := p.nextFrame(now)
				if !more {
					break
				}
				shown = picture(f)
				p.recycle(f)
			}
		}

		s := d.status()
		switch {
		case atMenu.IsZero() && time.Since(start) > 6*time.Second:
			// Get to a menu with buttons on it.
			if err := d.menuCall(dvdnav.MenuRoot); err != nil {
				t.Fatalf("cannot call the root menu: %s", d.nav.ErrToString())
			}
			atMenu = time.Now()

		case jumped.IsZero() && !atMenu.IsZero() && s.menu() &&
			time.Since(atMenu) > 5*time.Second && shown != nil:
			if s.button < 1 {
				_ = d.nav.ButtonSelect(s.pci, 1)
				d.syncButton()
				s = d.status()
			}
			before = shown
			t.Logf("pressing button %d of %d", s.button, s.pci.HLI.HlGI.BtnNs)
			d.activate()
			jumped = time.Now()

		case after == nil && !jumped.IsZero() && time.Since(jumped) > 10*time.Second:
			after = shown
			drawOverlay(t, d, p, after)
		}
	}

	if before == nil || after == nil {
		t.Fatal("never reached a menu with buttons on it")
	}
	if *pictures {
		write(t, "before.png", before)
		write(t, "after.png", after)
	}

	// The two menus are different pictures. Anything that leaves the
	// old one on screen — a decoder reset that eats the packet carrying
	// the new sequence header, a picture decoded before the jump
	// arriving after it — shows up here as no change at all.
	if d := difference(before, after); d < 5 {
		t.Errorf("the picture changed by %.2f of 255 across the jump: "+
			"the menu jumped to is not being shown", d)
	} else {
		t.Logf("the picture changed by %.2f of 255 across the jump", d)
	}
}

// difference reports the mean absolute difference between two pictures.
func difference(a, b *image.RGBA) float64 {
	if a.Bounds() != b.Bounds() {
		return 255
	}
	var total int64
	for i := range a.Pix {
		d := int(a.Pix[i]) - int(b.Pix[i])
		if d < 0 {
			d = -d
		}
		total += int64(d)
	}
	return float64(total) / float64(len(a.Pix))
}

// picture turns a decoded frame into an image.
func picture(f *frame) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, f.pictureW, f.pictureH))
	for y := range f.pictureH {
		for x := range f.pictureW {
			i := 4 * (y*f.width + x)
			r, g, b := color.YCbCrToRGB(f.ycbcr[i], f.ycbcr[i+1], f.ycbcr[i+2])
			o := img.PixOffset(x, y)
			img.Pix[o], img.Pix[o+1], img.Pix[o+2], img.Pix[o+3] = r, g, b, 0xFF
		}
	}
	return img
}

// overlay draws the menu's subpicture over the picture the way the
// player does, so that a look at the PNG shows the highlight too.
func drawOverlay(t *testing.T, d *disc, p *player, img *image.RGBA) {
	t.Helper()
	var sub *spu.Subpicture
	for {
		u, ok := p.nextSubpicture()
		if !ok {
			break
		}
		s, err := spu.Decode(u.data)
		if err != nil {
			t.Logf("subpicture: %v", err)
			continue
		}
		sub = s
	}
	if sub == nil {
		t.Log("the menu drew no subpicture")
		return
	}

	s := d.status()
	var hl *spu.Highlight
	if s.menu() && s.button >= 1 {
		if a, err := dvdnav.GetHighlightArea(s.pci, s.button, highlightSelected); err == nil {
			h := spu.ButtonHighlight(image.Rect(
				int(a.Sx), int(a.Sy), int(a.Ex)+1, int(a.Ey)+1), a.Palette)
			hl = &h
			t.Logf("button %d covers %v", s.button, h.Rect)
		}
	}
	over := image.NewRGBA(img.Bounds())
	sub.Draw(over, s.clut, hl)
	draw.Draw(img, img.Bounds(), over, image.Point{}, draw.Over)
}

func write(t *testing.T, name string, img *image.RGBA) {
	t.Helper()
	name = filepath.Join(t.TempDir(), name)
	f, err := os.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s", name)
}
