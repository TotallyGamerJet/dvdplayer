// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2026 The Media Authors

package main

import (
	"bytes"
	"fmt"
	"io"
	"math/rand"
	"os"
	"sync"
	"testing"

	"codeberg.org/totallygamerjet/media/ac3"
	"codeberg.org/totallygamerjet/media/mpg"
)

// TestAudioTrackAC3 checks the track's frame at a time AC3 decoding
// against the ac3 package's own stream decoder. The track cannot use
// that decoder directly — it reads until it has a frame, and a DVD stops
// giving out sound whenever it reaches a still — so the two have to be
// held to the same output.
//
// The device decides how much it asks for at a time, and the sizes below
// straddle the 6144 bytes one AC3 frame decodes to. A track that filled
// only as far as its first frame and padded out the rest of the buffer
// would pass at the smallest size and fail at the others, which is
// exactly how it sounded: a burst of sound, then silence, over and over.
func TestAudioTrackAC3(t *testing.T) {
	for _, size := range []int{4096, 6144, 8192, bytesPerSec / 5} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			testAudioTrackAC3(t, size)
		})
	}
}

func testAudioTrackAC3(t *testing.T, size int) {
	const sample = "../../ac3/testdata/sample-1-6c-48kz.ac3"
	data, err := os.ReadFile(sample)
	if err != nil {
		t.Fatal(err)
	}

	want, err := io.ReadAll(ac3.NewDecoderF32(bytes.NewReader(data), ac3.Stereo, sampleRate))
	if err != nil {
		t.Fatalf("decode %s: %v", sample, err)
	}
	if len(want) == 0 {
		t.Fatalf("%s decoded to nothing", sample)
	}

	// Hand the track the stream in pieces, the way the demuxer would.
	p := &player{}
	p.cond = sync.NewCond(&p.mu)
	stream := streamID{mpg.StreamPrivate1, mpg.SubstreamAC3Min}
	for _, chunk := range slicesOf(data, 2013) {
		p.audioQ = append(p.audioQ, audioChunk{data: chunk, pts: mpg.NoPTS, stream: stream})
	}

	track := newAudioTrack(p, &clock{})
	var got []byte
	buf := make([]byte, size)
	for len(got) < len(want) {
		n, err := track.Read(buf)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if n == 0 {
			break
		}
		got = append(got, buf[:n]...)
	}

	if len(got) < len(want) {
		t.Fatalf("decoded %d bytes, want %d", len(got), len(want))
	}
	if !bytes.Equal(got[:len(want)], want) {
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("sample %d of %d differs: got %#02x, want %#02x",
					i/bytesPerFrame, len(want)/bytesPerFrame, got[i], want[i])
			}
		}
	}
	// Everything past the last whole frame should be the silence the
	// track hands out when there is nothing left to play.
	if rest := got[len(want):]; !allZero(rest) {
		t.Errorf("%d bytes past the end of the stream are not silence", len(rest))
	}
}

// queue makes a track fed from the chunks given.
func queue(chunks ...audioChunk) *audioTrack {
	p := &player{}
	p.cond = sync.NewCond(&p.mu)
	p.audioQ = append(p.audioQ, chunks...)
	return newAudioTrack(p, &clock{})
}

// drain reads the track until it has n bytes or runs out of sound.
func drain(t *testing.T, track *audioTrack, n int) []byte {
	t.Helper()
	var got []byte
	buf := make([]byte, 8192)
	for len(got) < n {
		read, err := track.Read(buf)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if read == 0 || allZero(buf[:read]) {
			break
		}
		got = append(got, buf[:read]...)
	}
	return got
}

func ac3Chunks(t *testing.T, pts float64) ([]audioChunk, []byte) {
	t.Helper()
	const sample = "../../ac3/testdata/sample-1-6c-48kz.ac3"
	data, err := os.ReadFile(sample)
	if err != nil {
		t.Fatal(err)
	}
	want, err := io.ReadAll(ac3.NewDecoderF32(bytes.NewReader(data), ac3.Stereo, sampleRate))
	if err != nil {
		t.Fatal(err)
	}

	stream := streamID{mpg.StreamPrivate1, mpg.SubstreamAC3Min}
	var chunks []audioChunk
	for i, c := range slicesOf(data, 2013) {
		// Only the first packet of a run carries a time stamp.
		p := mpg.NoPTS
		if i == 0 {
			p = pts
		}
		chunks = append(chunks, audioChunk{data: c, pts: p, stream: stream})
	}
	return chunks, want
}

// TestAudioTrackRubbish feeds the track bytes that are not a soundtrack
// but hold something that looks like the start of one. A DVD leaves such
// bytes behind whenever it jumps: the tail of a frame from where it was,
// with the start of the new stream spliced onto it.
//
// The claimed length of a frame like that describes neither piece, and
// it can claim as little as 128 bytes. Decoding it out of a buffer cut
// to that length read off the end of it, which is how pressing a menu
// button used to bring the player down.
func TestAudioTrackRubbish(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	rubbish := make([]byte, 4096)
	r.Read(rubbish)
	// Plant something the sync search will believe.
	rubbish[100], rubbish[101] = 0x0B, 0x77

	chunks, want := ac3Chunks(t, 1)
	track := queue(append([]audioChunk{{
		data:   rubbish,
		pts:    mpg.NoPTS,
		stream: streamID{mpg.StreamPrivate1, mpg.SubstreamAC3Min},
	}}, chunks...)...)

	got := drain(t, track, len(want))

	// The sound after the rubbish still has to come out right. Allow for
	// the sync search having swallowed the first frames along with it.
	const frame = 6 * 256 * bytesPerFrame
	if !bytes.Contains(got, want[8*frame:12*frame]) {
		t.Errorf("the track did not pick the soundtrack back up: %d bytes decoded", len(got))
	}
}

// TestAudioTrackJump checks that a jump does not cost the sound it lands
// on. The bytes left over from where the disc was are no use once it has
// gone somewhere else, and joining them to what follows makes a frame
// that was never in either.
func TestAudioTrackJump(t *testing.T) {
	first, want := ac3Chunks(t, 1)
	second, _ := ac3Chunks(t, 4000)

	// Cut the first stream off inside its opening frame. What is left in
	// the buffer is a sync word claiming a frame far longer than the
	// bytes behind it, so joining the next stream on would let that
	// claim eat the frame the disc jumped to.
	first = first[:1]

	track := queue(append(first, second...)...)
	got := drain(t, track, len(want))

	// The stream after the jump has to decode from its very first
	// frame: nothing of it is lost to the leftovers.
	if len(got) < len(want) {
		t.Fatalf("decoded %d bytes, want %d", len(got), len(want))
	}
	if !bytes.Contains(got, want[:16*1024]) {
		t.Error("the stream the disc jumped to did not decode from its start")
	}
}

// slicesOf cuts b into pieces of at most n bytes.
func slicesOf(b []byte, n int) [][]byte {
	var out [][]byte
	for len(b) > n {
		out = append(out, b[:n])
		b = b[n:]
	}
	return append(out, b)
}
