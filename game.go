// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2026 The Media Authors

package main

import (
	"errors"
	"fmt"
	"image"
	"io"
	"math"
	"strings"
	"time"

	"github.com/hajimehoshi/ebiten/v2"
	"github.com/hajimehoshi/ebiten/v2/audio"
	"github.com/hajimehoshi/ebiten/v2/ebitenutil"
	"github.com/hajimehoshi/ebiten/v2/inpututil"

	"codeberg.org/totallygamerjet/media/dvdnav"
	"codeberg.org/totallygamerjet/media/mpg"
)

// yCbCrShader turns the decoder's YCbCr into RGB on the GPU, which is far
// cheaper than doing it a pixel at a time on the way out of the decoder.
const yCbCrShader = `package main

//kage:unit pixels

func Fragment(dstPos vec4, srcPos vec2, color vec4) vec4 {
	// For this calculation, see the comment in the standard library
	// color.YCbCrToRGB function.
	c := imageSrc0UnsafeAt(srcPos)
	return vec4(
		c.x + 1.40200 * (c.z-0.5),
		c.x - 0.34414 * (c.y-0.5) - 0.71414 * (c.z-0.5),
		c.x + 1.77200 * (c.y-0.5),
		1,
	)
}
`

// game is the player's front end: it shows what the decoders produce and
// turns what the viewer does into navigation.
type game struct {
	d     *disc
	p     *player
	clock *clock
	track *audioTrack
	sound *audio.Player

	// cur is the picture on screen and fresh says it has not been put
	// on the GPU yet. A DVD runs at 25 or 30 pictures a second and the
	// screen is redrawn far more often than that, so the same picture is
	// drawn again and again; uploading and converting it once is worth
	// the flag.
	cur   *frame
	fresh bool

	// ycbcr holds the current picture as the shader wants it and rgb the
	// result of running the shader over it.
	ycbcr  *ebiten.Image
	rgb    *ebiten.Image
	shader *ebiten.Shader

	overlay *overlay

	// area is where the picture was last drawn, which is what turns a
	// mouse position into a position on the disc's menu, and cursor is
	// where the mouse was, so that it only moves the highlight when it
	// has actually moved.
	area   image.Rectangle
	cursor image.Point

	paused bool
	osd    bool
	err    error

	// message is a note to the viewer — a stream change, say — and
	// messageUntil when it stops being shown.
	message      string
	messageUntil time.Time
}

func newGame(d *disc, p *player) (*game, error) {
	ctx := audio.NewContext(sampleRate)
	c := &clock{}
	track := newAudioTrack(p, c)
	sound, err := ctx.NewPlayerF32(track)
	if err != nil {
		return nil, err
	}
	// Enough of a buffer that a hitch in decoding does not reach the
	// speakers, and little enough that a menu still answers promptly:
	// the clock is taken from how much of this the device has played,
	// so what is left in it is how far behind the sound runs.
	sound.SetBufferSize(200 * time.Millisecond)

	shader, err := ebiten.NewShader([]byte(yCbCrShader))
	if err != nil {
		return nil, err
	}

	g := &game{d: d, p: p, clock: c, track: track, sound: sound, shader: shader, osd: true}
	g.overlay = newOverlay(d, p)
	go sound.Play()
	return g, nil
}

func (g *game) Update() error {
	if g.err != nil {
		return g.err
	}
	g.input()

	if g.paused {
		return nil
	}

	now, ok := g.time()
	if ok {
		for {
			f, more := g.p.nextFrame(now)
			if !more {
				break
			}
			if g.cur != nil {
				g.p.recycle(g.cur)
			}
			g.cur, g.fresh = f, true
		}
		if g.cur != nil {
			g.overlay.update(now, g.cur.gen)
		}
	}

	if done, err := g.p.done(); done {
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		return ebiten.Termination
	}
	return nil
}

// time reports where in the disc's stream the player is. The sound
// leaving the device drives it; a picture that arrives from somewhere
// the sound cannot place — a menu with no soundtrack, or a jump — sets
// it instead.
func (g *game) time() (float64, bool) {
	played := g.sound.Position().Seconds()
	now, ok := g.clock.now(played)

	head, have := g.p.peekFrame()
	if have && head != mpg.NoPTS && (!ok || math.Abs(head-now) > 5) {
		g.clock.set(played, head)
		return head, true
	}
	return now, ok
}

func (g *game) Draw(screen *ebiten.Image) {
	if g.cur == nil {
		return
	}
	f := g.cur
	if g.ycbcr == nil || g.ycbcr.Bounds().Dx() != f.width || g.ycbcr.Bounds().Dy() != f.height {
		g.ycbcr = ebiten.NewImage(f.width, f.height)
		g.rgb = ebiten.NewImage(f.width, f.height)
		g.fresh = true
	}
	if g.fresh {
		g.ycbcr.WritePixels(f.ycbcr)
		op := &ebiten.DrawRectShaderOptions{}
		op.Images[0] = g.ycbcr
		op.Blend = ebiten.BlendCopy
		g.rgb.DrawRectShader(f.width, f.height, g.shader, op)
		g.fresh = false
	}

	g.area = g.fit(screen.Bounds(), f, g.d.status())
	picture := g.rgb.SubImage(image.Rect(0, 0, f.pictureW, f.pictureH)).(*ebiten.Image)

	var dop ebiten.DrawImageOptions
	dop.GeoM.Scale(
		float64(g.area.Dx())/float64(f.pictureW),
		float64(g.area.Dy())/float64(f.pictureH),
	)
	dop.GeoM.Translate(float64(g.area.Min.X), float64(g.area.Min.Y))
	dop.Filter = ebiten.FilterLinear
	screen.DrawImage(picture, &dop)

	g.overlay.draw(screen, g.area, image.Pt(f.pictureW, f.pictureH), f.gen)
	g.drawOSD(screen)
}

// fit works out where the picture goes on screen: as large as it will go
// without cropping and without distorting it. A DVD stores 16:9 material
// squeezed into the same pixel count as 4:3, so the frame's display size
// rather than its coded size decides the shape.
func (g *game) fit(screen image.Rectangle, f *frame, s status) image.Rectangle {
	dw, dh := float64(f.displayW), float64(f.displayH)
	if s.aspect == 3 {
		dw = dh * 16 / 9
	}
	scale := min(float64(screen.Dx())/dw, float64(screen.Dy())/dh)
	w, h := int(dw*scale), int(dh*scale)
	x := screen.Min.X + (screen.Dx()-w)/2
	y := screen.Min.Y + (screen.Dy()-h)/2
	return image.Rect(x, y, x+w, y+h)
}

func (g *game) Layout(outsideWidth, outsideHeight int) (int, int) {
	return outsideWidth, outsideHeight
}

// notef puts a short note on screen.
func (g *game) notef(format string, args ...any) {
	g.message = fmt.Sprintf(format, args...)
	g.messageUntil = time.Now().Add(3 * time.Second)
}

func (g *game) drawOSD(screen *ebiten.Image) {
	var b strings.Builder
	if g.osd {
		s := g.d.status()
		where := fmt.Sprintf("title %d chapter %d", s.title, s.part)
		if s.inMenu {
			where = "menu " + dvdnav.MenuID(s.part).String()
		}
		fmt.Fprintf(&b, "%s   %s", where, ticks(uint64(s.streamTime)))
		if s.pgcLength > 0 {
			fmt.Fprintf(&b, " / %s", ticks(uint64(s.pgcLength)))
		}
		if s.still {
			b.WriteString("   still")
		}
		if g.paused {
			b.WriteString("   paused")
		}
		fmt.Fprintf(&b, "\n%.0f fps", ebiten.ActualFPS())
	}
	if time.Now().Before(g.messageUntil) {
		fmt.Fprintf(&b, "\n%s", g.message)
	}
	if b.Len() > 0 {
		ebitenutil.DebugPrint(screen, b.String())
	}
}

// input turns key presses and mouse movement into navigation. The arrow
// keys move between a menu's buttons where there is a menu, and seek
// through the film where there is not.
func (g *game) input() {
	menu := g.d.status().menu()

	switch {
	case inpututil.IsKeyJustPressed(ebiten.KeyQ):
		g.err = ebiten.Termination
	case inpututil.IsKeyJustPressed(ebiten.KeyF):
		ebiten.SetFullscreen(!ebiten.IsFullscreen())
	case inpututil.IsKeyJustPressed(ebiten.KeyI):
		g.osd = !g.osd
	}

	for _, k := range []struct {
		key ebiten.Key
		dir direction
	}{
		{ebiten.KeyArrowUp, dirUp},
		{ebiten.KeyArrowDown, dirDown},
		{ebiten.KeyArrowLeft, dirLeft},
		{ebiten.KeyArrowRight, dirRight},
	} {
		if !inpututil.IsKeyJustPressed(k.key) {
			continue
		}
		if menu {
			g.d.selectButton(k.dir)
			continue
		}
		switch k.dir {
		case dirLeft:
			g.seek(-10 * time.Second)
		case dirRight:
			g.seek(10 * time.Second)
		}
	}

	if inpututil.IsKeyJustPressed(ebiten.KeyEnter) || inpututil.IsKeyJustPressed(ebiten.KeyNumpadEnter) {
		g.overlay.press(g.d.activate())
	}
	if inpututil.IsKeyJustPressed(ebiten.KeySpace) {
		if menu || g.d.status().still {
			g.overlay.press(g.d.activate())
		} else {
			g.pause(!g.paused)
		}
	}

	if inpututil.IsKeyJustPressed(ebiten.KeyEscape) {
		g.call(dvdnav.MenuEscape)
	}
	if inpututil.IsKeyJustPressed(ebiten.KeyM) {
		g.call(dvdnav.MenuRoot)
	}
	if inpututil.IsKeyJustPressed(ebiten.KeyT) {
		g.call(dvdnav.MenuTitle)
	}
	if inpututil.IsKeyJustPressed(ebiten.KeyBackspace) {
		if err := g.d.goUp(); err != nil {
			g.notef("cannot go up: %v", g.d.nav.ErrToString())
		}
	}

	if inpututil.IsKeyJustPressed(ebiten.KeyN) || inpututil.IsKeyJustPressed(ebiten.KeyPageDown) {
		if err := g.d.nextPart(); err != nil {
			g.notef("no next chapter")
		}
	}
	if inpututil.IsKeyJustPressed(ebiten.KeyP) || inpututil.IsKeyJustPressed(ebiten.KeyPageUp) {
		if err := g.d.prevPart(); err != nil {
			g.notef("no previous chapter")
		}
	}

	if inpututil.IsKeyJustPressed(ebiten.KeyA) {
		g.cycleStream(dvdnav.AudioStream)
	}
	if inpututil.IsKeyJustPressed(ebiten.KeyS) {
		g.cycleStream(dvdnav.SubtitleStream)
	}
	if inpututil.IsKeyJustPressed(ebiten.KeyU) {
		s := g.d.status()
		g.d.setSPUVisible(!s.spuOn)
		g.notef("subtitles %s", onOff(!s.spuOn))
	}

	g.mouse(menu)
}

// mouse points at a menu's buttons.
func (g *game) mouse(menu bool) {
	if !menu || g.cur == nil || g.area.Empty() {
		return
	}
	x, y := ebiten.CursorPosition()
	moved := image.Pt(x, y) != g.cursor
	g.cursor = image.Pt(x, y)
	if !g.cursor.In(g.area) {
		return
	}
	press := inpututil.IsMouseButtonJustPressed(ebiten.MouseButtonLeft)
	if !press && !moved {
		// Leave the highlight where the keyboard put it while the mouse
		// is sitting still.
		return
	}

	// The button coordinates are in the coded picture's space, which is
	// what the picture on screen was scaled from.
	bx := int32((x - g.area.Min.X) * g.cur.pictureW / g.area.Dx())
	by := int32((y - g.area.Min.Y) * g.cur.pictureH / g.area.Dy())
	if b := g.d.pointAt(bx, by, press); b > 0 {
		g.overlay.press(b)
	}
}

func (g *game) pause(on bool) {
	g.paused = on
	if on {
		g.clock.pause()
		g.sound.Pause()
	} else {
		g.clock.resume()
		go g.sound.Play()
	}
}

func (g *game) seek(delta time.Duration) {
	if err := g.d.seek(delta); err != nil {
		g.notef("cannot seek: %s", g.d.nav.ErrToString())
		return
	}
	g.notef("%+ds", int(delta.Seconds()))
}

func (g *game) call(id dvdnav.MenuID) {
	if err := g.d.menuCall(id); err != nil {
		g.notef("no %s menu", id)
	}
}

// cycleStream moves to the next soundtrack or subtitle the disc offers.
func (g *game) cycleStream(kind dvdnav.StreamType) {
	n, err := g.d.nav.NumberOfStreams(kind)
	if err != nil || n <= 0 {
		g.notef("no streams to choose from")
		return
	}
	var cur int8
	name := "audio"
	if kind == dvdnav.SubtitleStream {
		name = "subtitles"
		cur, err = g.d.nav.GetActiveSPUStream()
	} else {
		cur, err = g.d.nav.GetActiveAudioStream()
	}
	if err != nil {
		g.notef("cannot read the current %s stream", name)
		return
	}
	next := uint8((int(cur) + 1) % int(n))
	if err := g.d.nav.SetActiveStream(next, kind); err != nil {
		g.notef("cannot select %s stream %d", name, next)
		return
	}
	g.notef("%s stream %d of %d", name, next+1, n)
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}
