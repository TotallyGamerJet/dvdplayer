// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2026 The Media Authors

package main

import (
	"encoding/binary"
	"math"
	"sync"
	"time"

	"github.com/gen2brain/mpeg"

	"codeberg.org/totallygamerjet/media/ac3"
	"codeberg.org/totallygamerjet/media/dca"
	"codeberg.org/totallygamerjet/media/mpg"
)

// The audio device is fed interleaved stereo float32 at the one sample
// rate DVD audio is authored at.
const (
	sampleRate    = 48000
	channels      = 2
	bytesPerFrame = channels * 4
	bytesPerSec   = sampleRate * bytesPerFrame
)

// A clock tells the player which moment of the disc the viewer is
// hearing. Which part of the disc that is moves around, because the
// stream time stamps jump every time the disc jumps between a menu, a
// title and a cell, so the audio track leaves a mark wherever the stream
// time of the samples it is about to emit stops following on from the
// last, and the clock adopts each mark as the device reaches it.
//
// The clock runs on the wall clock and is steered towards where the
// sound says it should be, rather than simply reporting the sound's own
// position. What the sound reports is a derived figure — samples handed
// over, less what the device has yet to play, smoothed — and reading it
// straight through to the picture makes the picture inherit everything
// that happens to it. Where it stalls, the picture freezes; where it
// jumps, the picture runs away, which on a menu looks like the video
// tearing along with no sound. A wall clock cannot do either: it is the
// one thing that always advances at exactly one second per second, and
// the sound only has to say where in the disc that second falls.
type clock struct {
	mu    sync.Mutex
	marks []mark

	anchored bool
	out      float64 // device position the anchor was taken at
	pts      float64 // stream time at that position

	// epoch is where the wall clock is measured from and off the
	// correction that puts it on the disc's own time line, so the clock
	// reads epoch plus off. running says the two have been tied together
	// at least once.
	epoch   time.Time
	off     float64
	running bool

	// held is how far the wall clock had run when the viewer paused, and
	// paused says it is standing there.
	paused bool
	held   time.Duration
}

// start ties the wall clock to a stream time.
func (c *clock) start(pts float64) {
	c.epoch, c.off, c.running = time.Now(), pts, true
	c.held = 0
}

// wall reports how far the wall clock has run since the epoch, standing
// still for as long as the viewer has it paused. c.mu must be held.
func (c *clock) wall() float64 {
	if c.paused {
		return c.held.Seconds()
	}
	return time.Since(c.epoch).Seconds()
}

// pause stops the wall clock and resume starts it again from where it
// stopped.
//
// The device stops playing when the viewer pauses, so the position it
// reports stands still. The wall clock would otherwise run on through the
// pause, and the clock would come back however long the viewer was away
// further down the disc: every picture queued behind that time falls due
// at once and is dropped to catch up, which is the jump the viewer sees.
//
// Only the wall clock is touched. The anchor, the correction off it and
// the marks the device has yet to reach all measure against the device's
// position rather than the time of day, and the device is left exactly
// where it was, so they still hold and the sound comes back against the
// same picture it went away on.
func (c *clock) pause() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.paused {
		return
	}
	c.held, c.paused = time.Since(c.epoch), true
}

func (c *clock) resume() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.paused {
		return
	}
	c.epoch, c.paused = time.Now().Add(-c.held), false
}

// clockEase is how much of the error each reading takes out: a twentieth,
// which closes one in about a third of a second.
//
// Nothing else moves the clock in a hurry. A mark says the disc has
// jumped somewhere else and is taken at once, but the device's own
// position is only ever eased towards, however far it has moved. That
// figure is derived — samples handed over, less what the device has yet
// to play, smoothed over the ticks — and it can step. Following a step
// would put every queued picture past its time at once and the video
// would tear along at whatever rate it decodes, which is what this looks
// like when it goes wrong.
const clockEase = 0.05

type mark struct {
	out, pts float64
}

// add notes that the samples emitted at device position out belong to
// stream time pts.
func (c *clock) add(out, pts float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.marks = append(c.marks, mark{out, pts})
}

// now reports the stream time to show, given how much the device says it
// has played. It reports false until the stream time is known at all.
func (c *clock) now(played float64) (float64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	marked := false
	for len(c.marks) > 0 && c.marks[0].out <= played {
		c.out, c.pts = c.marks[0].out, c.marks[0].pts
		c.anchored = true
		c.marks = c.marks[1:]
		marked = true
	}
	if !c.anchored {
		return 0, false
	}

	// Where the sound says we are, and where the wall clock has got to.
	heard := c.pts + played - c.out
	if !c.running {
		c.start(heard)
		return heard, true
	}
	wall := c.wall()

	err := heard - (wall + c.off)
	if marked {
		// The sound now being heard comes from somewhere else on the
		// disc, so there is nothing to ease towards: go there.
		c.off += err
	} else {
		c.off += clockEase * err
	}
	return wall + c.off, true
}

// set moves the clock to a stream time without waiting for the device,
// which is how a picture that arrives with no sound to place it — a
// silent menu — puts the clock where it belongs. Marks the device has
// yet to reach still stand: sound that is on its way knows better.
func (c *clock) set(played, pts float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for len(c.marks) > 0 && c.marks[0].out <= played {
		c.marks = c.marks[1:]
	}
	c.anchored = true
	c.out, c.pts = played, pts
	c.start(pts)
}

// An audioTrack is the source the audio device pulls from. It stays for
// the life of the player: a DVD switches between soundtracks, and turns
// the sound off altogether for a silent menu, so the track hands out
// silence rather than ever reporting an end.
type audioTrack struct {
	p     *player
	clock *clock

	// stream is what the decoder below was made for; when the disc
	// switches, the decoder is thrown away and built again.
	stream  streamID
	haveDec bool

	ac3  *ac3.State
	dca  *dca.State
	mp2  *mpeg.Audio
	lpcm lpcmState

	// in holds the compressed bytes of the current stream that no frame
	// has been decoded from yet.
	in []byte

	// out holds decoded bytes the device has not taken, and outPos
	// counts the seconds of sound emitted so far, silence included.
	out    []byte
	outPos float64

	// nextPTS is the stream time the sound now being decoded runs on
	// from, used to notice when the disc's time stamps jump.
	nextPTS float64

	// flush is the player's jump count as of the last read, so that a
	// jump throws away the sound decoded from where the disc was.
	flush int64

	// frame is scratch holding the AC3 frame being decoded, and samples
	// scratch for what comes out of one of its blocks.
	frame   [ac3.MaxFrameSize]byte
	samples []byte
}

func newAudioTrack(p *player, c *clock) *audioTrack {
	return &audioTrack{p: p, clock: c, nextPTS: mpg.NoPTS}
}

// Read fills buf with sound. It never blocks on the disc and never
// reports an end: where there is nothing at all to play — during a
// still, or in a menu with no soundtrack — it returns silence, and the
// clock's marks keep the picture lined up with the sound across the gap.
//
// Where it has some sound but not enough to fill buf it hands back what
// it has and lets the device ask again. Padding a short read out with
// silence would be far worse than a short read: the device counts those
// samples as time played, so the sound would come out in bursts and the
// clock would drag the picture back at every one of them.
func (t *audioTrack) Read(buf []byte) (int, error) {
	// The device asks for whole sample frames; keep it that way so the
	// stereo pairs never come apart.
	buf = buf[:len(buf)-len(buf)%bytesPerFrame]

	if g := t.p.flush.Load(); g != t.flush {
		// The disc has jumped. Sound decoded from where it was would
		// otherwise play on over what the viewer asked for, which on a
		// menu means the music carrying on into the feature.
		t.flush = g
		t.in, t.out = t.in[:0], nil
		t.nextPTS = mpg.NoPTS
		t.resetDecoders()
	}

	for len(t.out) < len(buf) {
		if !t.decodeSome() {
			break
		}
	}

	n := copy(buf, t.out)
	t.out = t.out[n:]
	if n == 0 {
		// Nothing to play at all: hold the line with silence. The disc
		// is on a still, or a slow drive has not kept up. Either way the
		// stream time stops running on from what was last heard, so the
		// sound that does arrive has to place itself afresh.
		clear(buf)
		n = len(buf)
		t.nextPTS = mpg.NoPTS
	}
	t.outPos += float64(n) / bytesPerSec
	return n, nil
}

// decodeSome turns whatever the demuxer has queued into sound. It
// reports whether it made progress.
func (t *audioTrack) decodeSome() bool {
	chunk, ok := t.take()
	if !ok {
		return false
	}

	if chunk.stream != t.stream || !t.haveDec {
		t.reset(chunk.stream)
	}
	if chunk.pts != mpg.NoPTS && t.note(chunk.pts) {
		// The sound does not follow on from what came before: the disc
		// has jumped, or left a gap. What is left in the buffer is the
		// tail of a frame from before the break, and splicing it onto
		// what comes next would make a frame that never existed: a sync
		// word with a length that describes neither piece.
		t.in = t.in[:0]
		t.resetDecoders()
	}
	t.in = append(t.in, chunk.data...)

	switch {
	case chunk.stream.substream >= mpg.SubstreamAC3Min && chunk.stream.substream <= mpg.SubstreamAC3Max:
		t.decodeAC3()
	case chunk.stream.substream >= mpg.SubstreamDTSMin && chunk.stream.substream <= mpg.SubstreamDTSMax:
		t.decodeDTS()
	case chunk.stream.substream >= mpg.SubstreamLPCMMin && chunk.stream.substream <= mpg.SubstreamLPCMMax:
		t.decodeLPCM()
	case chunk.stream.id >= mpg.StreamAudioMin && chunk.stream.id <= mpg.StreamAudioMax:
		t.decodeMP2()
	default:
		// A soundtrack this player cannot decode, SDDS say. Drop it: the
		// silence Read hands out stands in for it.
		t.in = t.in[:0]
	}
	return true
}

// take pulls the next queued chunk of the audio stream, if there is one.
func (t *audioTrack) take() (audioChunk, bool) {
	p := t.p
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.audioQ) == 0 {
		return audioChunk{}, false
	}
	c := p.audioQ[0]
	p.audioQ = p.audioQ[1:]
	p.cond.Broadcast()
	return c, true
}

// reset throws away the decoder and everything buffered for it, which is
// what a switch to another soundtrack calls for.
func (t *audioTrack) reset(s streamID) {
	t.stream, t.haveDec = s, true
	t.ac3, t.dca, t.mp2 = nil, nil, nil
	t.lpcm.Reset()
	t.in, t.out = t.in[:0], nil
	t.nextPTS = mpg.NoPTS

	switch {
	case s.substream >= mpg.SubstreamAC3Min && s.substream <= mpg.SubstreamAC3Max:
		t.ac3 = ac3.NewState()
	case s.substream >= mpg.SubstreamDTSMin && s.substream <= mpg.SubstreamDTSMax:
		t.dca = dca.NewState()
	case s.id >= mpg.StreamAudioMin && s.id <= mpg.StreamAudioMax:
		if buf, err := mpeg.NewBuffer(nil); err == nil {
			t.mp2 = mpeg.NewAudio(buf)
		}
	}
}

// maxAudioGap is the longest gap in the sound's time stamps that is
// played as the silence it is, rather than marked for the clock to jump
// across. See [audioTrack.note].
const maxAudioGap = 2.0 // seconds

// note marks the stream time of the sound about to be decoded when it
// does not simply follow on from what came before, and reports whether
// it had to.
//
// Sound that starts a little after the last ended is a gap the disc
// meant to be heard as silence — a second of a title with no soundtrack,
// say, between two that have one — and it is played as silence. Marked
// instead, the clock would jump the gap the moment the device got there,
// and every picture in it would fall due at once: the video skips ahead
// to catch up, and whatever is drawn by the clock rather than with the
// picture, a menu's buttons, turns up over a picture it does not belong
// to.
func (t *audioTrack) note(pts float64) bool {
	if t.nextPTS != mpg.NoPTS {
		gap := pts - t.nextPTS
		if math.Abs(gap) < 0.1 {
			return false
		}
		if gap > 0 && gap <= maxAudioGap {
			t.emit(make([]byte, int(gap*sampleRate+0.5)*bytesPerFrame))
			return true
		}
	}
	t.nextPTS = pts
	// The samples decoded from here play once the device has worked
	// through everything already handed to it.
	t.clock.add(t.outPos+float64(len(t.out))/bytesPerSec, pts)
	return true
}

// emit appends decoded sound and advances the running stream time.
func (t *audioTrack) emit(b []byte) {
	t.out = append(t.out, b...)
	if t.nextPTS != mpg.NoPTS {
		t.nextPTS += float64(len(b)) / bytesPerSec
	}
}

// decodeAC3 decodes every complete AC3 frame buffered.
func (t *audioTrack) decodeAC3() {
	for {
		// Find a sync word with a full frame behind it.
		i := 0
		for ; i+7 < len(t.in); i++ {
			if t.in[i] == 0x0B && t.in[i+1] == 0x77 {
				break
			}
		}
		if i+7 >= len(t.in) {
			t.in = append(t.in[:0], t.in[i:]...)
			return
		}
		length, rate, _, _ := ac3.SyncInfo(t.in[i:])
		if length == 0 {
			// A false sync; step over it and look again.
			t.in = append(t.in[:0], t.in[i+2:]...)
			continue
		}
		if i+int(length) > len(t.in) {
			t.in = append(t.in[:0], t.in[i:]...)
			return
		}
		if int(rate) == sampleRate {
			// The decoder reads ahead of the frame it is given, so it
			// takes a buffer that can hold the largest frame there is
			// whatever the size of this one.
			n := copy(t.frame[:], t.in[i:i+int(length)])
			clear(t.frame[n:])
			t.decodeAC3Frame()
		}
		t.in = append(t.in[:0], t.in[i+int(length):]...)
	}
}

// decodeAC3Frame decodes the frame in t.frame, downmixed to stereo.
func (t *audioTrack) decodeAC3Frame() {
	const (
		samplesPerBlock = 256
		blocksPerFrame  = 6
		bias            = 384
	)
	flags := int32(ac3.Stereo | ac3.AdjustLevel)
	level := ac3.Level(1)
	if err := t.ac3.Frame(t.frame[:], &flags, &level, bias); err != nil {
		return
	}
	if flags&ac3.ChannelMask != ac3.Stereo && flags&ac3.ChannelMask != ac3.AudioDolby {
		return
	}

	if n := samplesPerBlock * bytesPerFrame; cap(t.samples) < n {
		t.samples = make([]byte, n)
	} else {
		t.samples = t.samples[:n]
	}
	for range blocksPerFrame {
		if err := t.ac3.Block(); err != nil {
			return
		}
		// The decoder hands out one plane per speaker, biased so that
		// the sample is the low bits of the float's representation.
		s := t.ac3.Samples()
		left, right := s[:samplesPerBlock], s[samplesPerBlock:2*samplesPerBlock]
		for i := range samplesPerBlock {
			binary.LittleEndian.PutUint32(t.samples[bytesPerFrame*i:], debias(left[i]))
			binary.LittleEndian.PutUint32(t.samples[bytesPerFrame*i+4:], debias(right[i]))
		}
		t.emit(t.samples)
	}
}

// decodeDTS decodes every complete DTS frame buffered. A DVD's are 16-bit
// big-endian, but the decoder reads any packing, and the frames are found
// by the same header check whichever they are.
func (t *audioTrack) decodeDTS() {
	if t.dca == nil {
		t.in = t.in[:0]
		return
	}
	for {
		// Find a frame header, with the whole of the frame behind it.
		i := 0
		length, rate, blocks := 0, 0, 0
		for ; i+dca.HeaderSize <= len(t.in); i++ {
			var frameLength int
			if length, _, rate, _, frameLength = dca.SyncInfo(t.in[i:]); length != 0 {
				blocks = frameLength / dca.SamplesPerBlock
				break
			}
		}
		if length == 0 {
			// Nothing here yet; keep the end, which may be the start of
			// the header of a frame the next packet finishes.
			t.in = append(t.in[:0], t.in[i:]...)
			return
		}
		if i+length > len(t.in) {
			t.in = append(t.in[:0], t.in[i:]...)
			return
		}
		if rate == sampleRate {
			t.decodeDTSFrame(t.in[i:i+length], blocks)
		}
		t.in = append(t.in[:0], t.in[i+length:]...)
	}
}

// decodeDTSFrame decodes a DTS frame of blocks blocks of 256 samples,
// downmixed to stereo. A frame that cannot be decoded, wholly or in part,
// plays as silence for what is lost, so that the sound that follows still
// arrives when the disc says it should.
func (t *audioTrack) decodeDTSFrame(frame []byte, blocks int) {
	flags, level := dca.Stereo, float32(1)
	if t.dca.Frame(frame, &flags, &level, 0) != nil {
		t.silence(blocks)
		t.dca.Reset()
		return
	}

	if n := dca.SamplesPerBlock * bytesPerFrame; cap(t.samples) < n {
		t.samples = make([]byte, n)
	} else {
		t.samples = t.samples[:n]
	}
	for done := range t.dca.BlocksNum() {
		if t.dca.Block() != nil {
			t.silence(blocks - done)
			t.dca.Reset()
			return
		}
		// The decoder hands out one plane per channel, in float already.
		s := t.dca.Samples()
		left, right := s[:dca.SamplesPerBlock], s[dca.SamplesPerBlock:2*dca.SamplesPerBlock]
		if flags&dca.ChannelMask == dca.Mono {
			right = left
		}
		for i := range dca.SamplesPerBlock {
			binary.LittleEndian.PutUint32(t.samples[bytesPerFrame*i:], math.Float32bits(max(-1, min(1, left[i]))))
			binary.LittleEndian.PutUint32(t.samples[bytesPerFrame*i+4:], math.Float32bits(max(-1, min(1, right[i]))))
		}
		t.emit(t.samples)
	}
}

// silence emits blocks blocks of 256 samples of silence.
func (t *audioTrack) silence(blocks int) {
	t.emit(make([]byte, blocks*dca.SamplesPerBlock*bytesPerFrame))
}

// resetDecoders forgets what the decoders carried over from the sound
// before a jump, which would be heard at the start of what follows.
func (t *audioTrack) resetDecoders() {
	if t.dca != nil {
		t.dca.Reset()
	}
	t.lpcm.Reset()
}

// debias turns one of the AC3 decoder's biased samples into the float32
// bits the audio device wants. The decoder adds 384 to a 16 bit sample
// and stores the result as a float, which is how it avoids a conversion
// per sample while decoding.
func debias(s float32) uint32 {
	v := int32(math.Float32bits(s)) - 0x43C00000
	if v > math.MaxInt16 {
		v = math.MaxInt16
	} else if v < math.MinInt16 {
		v = math.MinInt16
	}
	return math.Float32bits(float32(v) / 32768)
}

// decodeMP2 decodes every complete MPEG audio frame buffered.
func (t *audioTrack) decodeMP2() {
	if t.mp2 == nil {
		t.in = t.in[:0]
		return
	}
	t.mp2.Buffer().Write(t.in)
	t.in = t.in[:0]
	for {
		s := t.mp2.Decode()
		if s == nil {
			return
		}
		if t.mp2.Channels() != channels || t.mp2.Samplerate() != sampleRate {
			return
		}
		b := make([]byte, len(s.Interleaved)*4)
		for i, v := range s.Interleaved {
			binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(v))
		}
		t.emit(b)
	}
}
