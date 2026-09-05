// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2026 The Media Authors

package main

import (
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"codeberg.org/totallygamerjet/media/mpg"
)

// TestPlayDisc walks a real disc through the whole pipeline — the
// navigator, the demuxer, the video decoder and the audio track — and
// checks that pictures and sound come out of it. The window and the
// audio device are left out, so it needs neither.
//
// Point DVD_TEST_DEVICE at a device node, an ISO image or a VIDEO_TS
// directory to run it, for example
//
//	DVD_TEST_DEVICE=/dev/rdisk4 go test ./cmd/dvdplay/
//
// A CSS protected disc has to be named by its raw device: authentication
// does not happen through a mount point.
func TestPlayDisc(t *testing.T) {
	path := os.Getenv("DVD_TEST_DEVICE")
	if path == "" {
		t.Skip("set DVD_TEST_DEVICE to a DVD device, image or VIDEO_TS directory")
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	d, err := openDisc(path, logger, true, true)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() {
		if err := d.Close(); err != nil {
			t.Errorf("closing the disc: %v", err)
		}
	}()

	p := newPlayer(d)
	d.start()
	p.start()
	defer p.close()

	c := &clock{}
	track := newAudioTrack(p, c)

	// Stand in for the audio device: take sound at the rate it would be
	// played at, which is what moves the clock on. The pacing matters —
	// a device that swallowed the stream as fast as it was produced
	// would put the clock ahead of everything and every picture would
	// look overdue. So does the size: a device asks for more than one
	// AC3 frame's worth at a time, which is what the track has to fill.
	sound := make([]byte, bytesPerSec/20)
	var played float64

	var reads, silent int
	var frames int
	// A disc jumps — out of the first play program, into a menu, out of
	// a still — and its time stamps jump with it, so the stream time
	// covered is the sum of the steps between consecutive pictures
	// rather than the difference between the first and the last.
	var streamTime, silentTime float64
	var prev float64 = -1
	start := time.Now()
	deadline := start.Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if ahead := played - time.Since(start).Seconds(); ahead > 0 {
			time.Sleep(time.Duration(ahead * float64(time.Second)))
		}
		n, err := track.Read(sound)
		if err != nil {
			t.Fatalf("read sound: %v", err)
		}
		reads++
		if allZero(sound[:n]) {
			silent++
			silentTime += float64(n) / bytesPerSec
		}
		played += float64(n) / bytesPerSec

		now, ok := c.now(played)
		if head, have := p.peekFrame(); have && head != mpg.NoPTS && !ok {
			c.set(played, head)
			now, ok = head, true
		}
		if !ok {
			continue
		}
		for {
			f, more := p.nextFrame(now)
			if !more {
				break
			}
			if frames == 0 {
				t.Logf("first picture: %dx%d coded, %dx%d shown, at %.3fs",
					f.width, f.height, f.displayW, f.displayH, f.pts)
			}
			if step := f.pts - prev; prev >= 0 && step > 0 && step < 0.5 {
				streamTime += step
			}
			prev = f.pts
			frames++
			p.recycle(f)
		}
		if done, err := p.done(); done {
			t.Logf("the disc ended: %v", err)
			break
		}
	}

	if frames == 0 {
		t.Fatal("no pictures decoded")
	}
	heard := played - silentTime
	t.Logf("%d pictures covering %.2fs of stream against %.2fs of sound in %.2fs of playing, %d of %d reads silent",
		frames, streamTime, heard, played, silent, reads)

	// The picture cannot legitimately cover more of the stream than
	// there was time to play it in. If it does, the clock is running
	// away and pictures are being thrown at the screen as fast as they
	// are decoded.
	if streamTime > played+2 {
		t.Errorf("%.2fs of picture in %.2fs of playing: the clock is running away",
			streamTime, played)
	}
	// How much of the run is sound depends on the disc — a menu holding
	// on a still is silent, and so are the gaps between programs — but a
	// disc that never makes a sound means the soundtrack is not being
	// found at all.
	if heard == 0 {
		t.Error("no sound was decoded")
	}
}

func allZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}
