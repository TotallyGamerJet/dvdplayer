// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2026 The Media Authors

package main

import (
	"encoding/binary"
	"errors"
	"math"
	"sync"
	"sync/atomic"

	"codeberg.org/totallygamerjet/media/mpeg2"
	"codeberg.org/totallygamerjet/media/mpg"
)

// Queue bounds. The demuxer waits on the video and the audio queue, and
// between them they are what hold the disc back: it runs no further
// ahead than the fuller of the two allows.
//
// Both are needed. A menu is often a single still picture over a piece
// of music that loops, and with nothing to hold it the disc will go
// round that loop as fast as the drive can read — which is what it
// sounds like, the music racing round and round. Video alone is no
// throttle when there is no video.
//
// The sound is held to a span of stream time rather than a count of
// packets, because a count is a different amount of time on every disc:
// forty eight packets is under two seconds of a 448 kbit/s soundtrack and
// over four seconds of a 192 kbit/s one.
//
// That span is what keeps the picture with the sound. The clock is taken
// from what the audio device has played, so a picture is due when the
// sound beside it is being heard — and the queue of decoded pictures is a
// fraction of a second deep. Let the demuxer run seconds ahead and the
// pictures it decodes are seconds early: they sit undisplayable until the
// clock reaches them, the queue behind them fills, and the picture stops
// while the sound plays on. Hold the demuxer to a lead the picture can
// cover and the two stay together.
//
// The video queue has to be roomy enough that it is never what the
// demuxer stops on while sound is flowing. Packets arrive in the order
// the disc holds them, so a demuxer waiting on a full video queue is a
// demuxer not queueing sound either — and the sound is what moves the
// clock that lets pictures off the queue in the first place. Let video
// be the constraint and the two hold each other still: the picture waits
// for a clock that waits for sound that waits for the picture. It has to
// hold every video packet that can arrive within maxAudioLead, which at
// the fastest a DVD is allowed to be read is around three hundred.
//
// The counts are hard caps for the case the span cannot be worked out,
// where a stream carries no time stamps at all.
const (
	maxAudioLead    = 0.5 // seconds of stream time
	maxVideoPackets = 768
	maxAudioChunks  = 48
	maxSubpictures  = 8
	maxFrames       = 4
)

// A frame is one decoded picture waiting to be shown.
type frame struct {
	// pts is the presentation time in stream seconds, mpg.NoPTS when the
	// picture carried no time stamp.
	pts float64

	// gen is the jump count the picture was decoded under, so that what
	// is drawn over it can tell whether it is still the picture the disc
	// is on.
	gen int64

	// width and height are the coded size, a multiple of 16; pictureW
	// and pictureH the displayable part of it; displayW and displayH the
	// size on screen once the pixel aspect ratio is applied.
	width, height      int
	pictureW, pictureH int
	displayW, displayH int

	// ycbcr holds the picture as four bytes per pixel: Y, Cb, Cr and a
	// byte the shader ignores.
	ycbcr []byte
}

// An audioChunk is one demuxed piece of the audio elementary stream.
type audioChunk struct {
	data []byte
	pts  float64
	// stream identifies the elementary stream it came from, so that the
	// audio track can tell when the disc has switched to another one.
	stream streamID
}

// A subpicture is one complete subpicture unit with the time it is to be
// acted on.
type subpicture struct {
	data []byte
	pts  float64
}

// A streamID names an elementary stream inside the program stream.
type streamID struct {
	id        byte // the PES stream id
	substream byte // the substream id within private stream 1, or 0
}

// player turns the program stream the disc hands out into pictures and
// sound. The demuxer, the video decoder and the audio device each run on
// a goroutine of their own and meet at the queues below.
type player struct {
	d     *disc
	demux *mpg.Demux
	video *mpeg2.Decoder

	mu   sync.Mutex
	cond *sync.Cond

	videoQ []mpg.Packet
	audioQ []audioChunk
	spuQ   []subpicture

	// frames holds decoded pictures in display order; pool holds the
	// buffers the shown ones came back in.
	frames []*frame
	pool   []*frame

	// partial is the subpicture unit being reassembled and partialPTS
	// the time it was announced with.
	partial    []byte
	partialPTS float64

	// audioStream and spuStream are the streams sound and subpictures
	// are currently being taken from.
	audioStream streamID
	spuStream   byte

	// flush counts the jumps the stream has been through. Each consumer
	// keeps its own copy and throws away what it is holding when the
	// two differ, so that nothing from before a jump is played over
	// what the viewer asked for.
	flush atomic.Int64

	// resync makes the video decoder drop pictures until it has the
	// reference frames to decode them properly, which is the case at the
	// start of a stream and after the disc jumps.
	resync  bool
	gotI    bool
	anchors int

	// lastVideoPTS is the time stamp of the last video packet fed to the
	// decoder, used to notice that the disc has jumped, and videoGen the
	// decoder's copy of the jump count.
	lastVideoPTS float64
	videoGen     int64

	// nextTime is the predicted presentation time of the picture after
	// the one just decoded, for pictures that carry no time stamp.
	nextTime    float64
	framePeriod float64

	// stopped is set once the disc has been played out, and closing
	// releases the goroutines.
	stopped bool
	closing bool
	err     error
}

func newPlayer(d *disc) *player {
	p := &player{
		d:            d,
		demux:        mpg.NewDemux(d),
		video:        mpeg2.NewDecoder(),
		partialPTS:   mpg.NoPTS,
		lastVideoPTS: mpg.NoPTS,
		framePeriod:  1.0 / 30,
		resync:       true,
	}
	p.cond = sync.NewCond(&p.mu)
	return p
}

// start sets the demuxer and the video decoder going.
func (p *player) start() {
	go p.route()
	go p.decode()
}

// close releases the goroutines. The disc must be closed first, so that
// the demuxer is not left waiting on it.
func (p *player) close() {
	p.mu.Lock()
	p.closing = true
	p.cond.Broadcast()
	p.mu.Unlock()
}

// route is the demuxer's goroutine: it splits the program stream and
// hands each packet to the queue it belongs in.
func (p *player) route() {
	for {
		pkt, err := p.demux.Next()
		if errors.Is(err, errDiscontinuity) {
			// The disc has left for somewhere else. Everything held
			// belongs to where it was, including whatever the demuxer
			// has read but not yet made a packet of.
			p.discard()
			p.demux = mpg.NewDemux(p.d)
			continue
		}
		if err != nil {
			p.mu.Lock()
			p.err = err
			p.stopped = true
			p.cond.Broadcast()
			p.mu.Unlock()
			return
		}

		p.mu.Lock()
		switch {
		case pkt.IsVideo():
			for len(p.videoQ) >= maxVideoPackets && !p.closing {
				p.cond.Wait()
			}
			p.videoQ = append(p.videoQ, pkt)
		case pkt.IsSubpicture():
			p.routeSubpicture(pkt)
		case pkt.IsAC3() || pkt.IsMPEGAudio():
			p.routeAudio(pkt)
		}
		p.cond.Broadcast()
		closing := p.closing
		p.mu.Unlock()
		if closing {
			return
		}
	}
}

// routeAudio queues a packet of the stream the disc has selected and
// throws away the rest. p.mu must be held.
func (p *player) routeAudio(pkt mpg.Packet) {
	want, ok := p.d.audioStream()
	if ok {
		p.audioStream = want
	} else {
		// The disc has not said which stream to play, so take the first
		// one that turns up and stay with it.
		if p.audioStream == (streamID{}) {
			p.audioStream = streamID{pkt.StreamID, pkt.Substream}
		}
		want = p.audioStream
	}
	if pkt.StreamID != want.id || pkt.Substream != want.substream {
		return
	}
	for (len(p.audioQ) >= maxAudioChunks || p.audioLead() > maxAudioLead) && !p.closing {
		p.cond.Wait()
	}
	p.audioQ = append(p.audioQ, audioChunk{data: pkt.Data, pts: pkt.PTS, stream: want})
}

// audioLead reports how much stream time the queued sound covers, which
// is how far ahead of what is being heard the demuxer has run. It reports
// zero for a stream that carries no time stamps to measure with. p.mu
// must be held.
func (p *player) audioLead() float64 {
	first, last := mpg.NoPTS, mpg.NoPTS
	for _, c := range p.audioQ {
		if c.pts == mpg.NoPTS {
			continue
		}
		if first == mpg.NoPTS {
			first = c.pts
		}
		last = c.pts
	}
	if first == mpg.NoPTS || last <= first {
		return 0
	}
	return last - first
}

// routeSubpicture reassembles subpicture units, which are generally
// spread over several packets. p.mu must be held.
func (p *player) routeSubpicture(pkt mpg.Packet) {
	want, ok := p.d.subpictureStream()
	if ok {
		p.spuStream = want
	} else {
		// The disc has not said which stream to take, so take the first
		// one that turns up: mixing two would leave the units in
		// pieces.
		if p.spuStream == 0 {
			p.spuStream = pkt.Substream
		}
		want = p.spuStream
	}
	if pkt.Substream != want {
		return
	}

	if len(p.partial) == 0 {
		if len(pkt.Data) < 2 {
			return
		}
		p.partialPTS = pkt.PTS
	} else if pkt.PTS != mpg.NoPTS {
		// A new unit began before the last one was complete.
		p.partial, p.partialPTS = nil, pkt.PTS
		if len(pkt.Data) < 2 {
			return
		}
	}
	p.partial = append(p.partial, pkt.Data...)

	size := int(binary.BigEndian.Uint16(p.partial))
	if size == 0 || len(p.partial) < size {
		return
	}
	if len(p.spuQ) >= maxSubpictures {
		p.spuQ = p.spuQ[1:]
	}
	p.spuQ = append(p.spuQ, subpicture{data: p.partial[:size], pts: p.partialPTS})
	p.partial, p.partialPTS = nil, mpg.NoPTS
}

// discard throws away everything buffered, which is what a jump calls
// for: it all belongs to where the disc was.
func (p *player) discard() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.videoQ = p.videoQ[:0]
	p.audioQ = p.audioQ[:0]
	p.spuQ = p.spuQ[:0]
	p.pool = append(p.pool, p.frames...)
	p.frames = p.frames[:0]
	p.partial, p.partialPTS = nil, mpg.NoPTS
	p.flush.Add(1)
	p.cond.Broadcast()
}

// nextSubpicture hands back the oldest subpicture unit that has been
// reassembled, if any.
func (p *player) nextSubpicture() (subpicture, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.spuQ) == 0 {
		return subpicture{}, false
	}
	s := p.spuQ[0]
	p.spuQ = p.spuQ[1:]
	return s, true
}

// feedVideo hands the next video packet to the decoder, waiting for one
// to arrive. It reports whether the stream is still running.
func (p *player) feedVideo() bool {
	p.mu.Lock()
	for len(p.videoQ) == 0 {
		if p.closing || p.stopped {
			p.mu.Unlock()
			return false
		}
		p.cond.Wait()
	}
	pkt := p.videoQ[0]
	p.videoQ = p.videoQ[1:]
	p.cond.Broadcast()
	p.mu.Unlock()

	// A jump makes everything the decoder holds worthless: it describes
	// a picture from before the jump and predicts nothing that follows.
	// The queue was emptied along with it, so this packet is the first
	// of the new stream.
	//
	// The reset has to happen here rather than around the parsing,
	// because it clears the decoder's input — and doing it a moment
	// later would throw away this very packet, which is the one
	// carrying the sequence header that the new stream cannot be
	// decoded without. On a menu that is a still picture over a piece
	// of music, that one packet is very nearly all the video there is,
	// and losing it leaves the picture from the previous menu on screen
	// for as long as the viewer stays.
	if g := p.flush.Load(); g != p.videoGen {
		p.videoGen = g
		p.video.Reset(true)
		p.resync, p.gotI, p.anchors = true, false, 0
		p.lastVideoPTS, p.nextTime = mpg.NoPTS, 0
	}

	if pkt.PTS != mpg.NoPTS {
		if p.lastVideoPTS != mpg.NoPTS && math.Abs(pkt.PTS-p.lastVideoPTS) > 1 {
			// The disc jumped: what the decoder holds is no reference
			// for what comes next.
			p.resync, p.gotI, p.anchors = true, false, 0
		}
		p.lastVideoPTS = pkt.PTS
		// Tag the picture that starts in this packet with its time. The
		// tag travels with the picture through B-frame reordering, so
		// the presentation time comes back with the displayed picture.
		ticks := uint64(pkt.PTS*90000 + 0.5)
		p.video.TagPicture(uint32(ticks), uint32(ticks>>32))
	}
	p.video.Buffer = pkt.Data
	return true
}

// decode is the video decoder's goroutine: it turns the video stream
// into pictures and queues them in display order. Everything it works on
// between the queues is its own, so decoding never holds the lock the
// audio device and the screen need.
func (p *player) decode() {
	for {
		switch p.video.Parse() {
		case mpeg2.StateBuffer:
			if !p.feedVideo() {
				return
			}

		case mpeg2.StateSequence:
			// A new sequence: nothing decoded so far can serve as a
			// reference for what follows.
			p.resync, p.gotI, p.anchors = true, false, 0
			if seq := p.video.Info.Sequence; seq != nil && seq.FramePeriod != 0 {
				// FramePeriod counts 1/27000000 second units.
				p.framePeriod = float64(seq.FramePeriod) / 27000000
			}

		case mpeg2.StateSlice, mpeg2.StateEnd, mpeg2.StateInvalidEnd:
			if !p.emitFrame() {
				return
			}
		}
	}
}

// emitFrame queues the picture the decoder has just finished, if it is
// one that can be shown. It reports whether the stream is still running.
func (p *player) emitFrame() bool {
	info := &p.video.Info
	if p.resync {
		// Count the reference pictures decoded since the last I frame,
		// so that pictures predicted from frames we never saw — the
		// leading B frames of an open GOP — can be left out.
		if cur := info.CurrentPicture; cur != nil {
			switch cur.Flags & mpeg2.PicMaskCodingType {
			case mpeg2.PicFlagCodingTypeI:
				p.gotI = true
				p.anchors++
			case mpeg2.PicFlagCodingTypeP:
				if p.gotI {
					p.anchors++
				}
			}
		}
	}
	if info.DisplayFrameBuffer == nil || info.Sequence == nil {
		return true
	}

	pts := p.nextTime
	if pic := info.DisplayPicture; pic != nil && pic.Flags&mpeg2.PicFlagTags != 0 {
		pts = float64(uint64(pic.Tag2)<<32|uint64(pic.Tag)) / 90000
	}
	duration := p.framePeriod
	if pic := info.DisplayPicture; pic != nil && pic.NbFields > 0 {
		// Respect repeated fields, which is how 24 fps film is carried
		// in a 30 fps stream.
		duration = p.framePeriod * float64(pic.NbFields) / 2
	}
	p.nextTime = pts + duration

	if p.resync {
		isB := false
		if pic := info.DisplayPicture; pic != nil {
			isB = pic.Flags&mpeg2.PicMaskCodingType == mpeg2.PicFlagCodingTypeB
		}
		closedGop := info.Gop != nil && info.Gop.Flags&mpeg2.GopFlagClosedGop != 0
		if !p.gotI || (isB && p.anchors < 2 && !closedGop) {
			return true
		}
		p.resync = false
	}

	f := p.takeFrame(info.Sequence)
	if f == nil {
		return false
	}
	f.pts, f.gen = pts, p.videoGen
	// The decoder's buffers are its own to reuse, so the picture has to
	// be copied out before the next Parse call.
	convertYCbCr(f, info.DisplayFrameBuffer.Buf, info.Sequence)

	p.mu.Lock()
	if p.flush.Load() != p.videoGen {
		// The disc jumped while this picture was being decoded, so it
		// belongs to where the disc was. Queueing it now would put it
		// back on screen after everything from before the jump had just
		// been thrown away.
		p.pool = append(p.pool, f)
	} else {
		p.frames = append(p.frames, f)
	}
	p.cond.Broadcast()
	p.mu.Unlock()
	return true
}

// takeFrame waits for room in the display queue and returns a frame
// buffer sized for the sequence, reusing a shown one when it can. It
// returns nil once the player is closing.
func (p *player) takeFrame(seq *mpeg2.Sequence) *frame {
	p.mu.Lock()
	for len(p.frames) >= maxFrames {
		if p.closing {
			p.mu.Unlock()
			return nil
		}
		p.cond.Wait()
	}
	var f *frame
	if n := len(p.pool); n > 0 {
		f, p.pool = p.pool[n-1], p.pool[:n-1]
	} else {
		f = &frame{}
	}
	p.mu.Unlock()

	f.width, f.height = int(seq.Width), int(seq.Height)
	f.pictureW, f.pictureH = int(seq.PictureWidth), int(seq.PictureHeight)
	if f.pictureW == 0 || f.pictureH == 0 {
		f.pictureW, f.pictureH = f.width, f.height
	}
	f.displayW, f.displayH = f.pictureW, f.pictureH
	if seq.PixelWidth != 0 && seq.PixelHeight != 0 {
		f.displayW = int(math.Round(float64(f.pictureW) * float64(seq.PixelWidth) / float64(seq.PixelHeight)))
	}
	if n := 4 * f.width * f.height; cap(f.ycbcr) < n {
		f.ycbcr = make([]byte, n)
	} else {
		f.ycbcr = f.ycbcr[:n]
	}
	return f
}

// convertYCbCr packs the decoder's three planes into the interleaved
// form the shader samples, undoing the chroma subsampling on the way.
func convertYCbCr(f *frame, planes [3][]uint8, seq *mpeg2.Sequence) {
	w, h := f.width, f.height
	yStride, cStride := int(seq.Width), int(seq.ChromaWidth)
	// 4:2:0 halves both dimensions, 4:2:2 only the width, 4:4:4 neither.
	xShift, yShift := 0, 0
	if seq.ChromaWidth < seq.Width {
		xShift = 1
	}
	if seq.ChromaHeight < seq.Height {
		yShift = 1
	}
	for j := range h {
		yi, ci := j*yStride, (j>>yShift)*cStride
		// Slice up front to let the compiler drop the bounds checks.
		ys := planes[0][yi : yi+w]
		cbs := planes[1][ci : ci+(w>>xShift)]
		crs := planes[2][ci : ci+(w>>xShift)]
		for i := range w {
			idx := 4 * (j*w + i)
			buf := f.ycbcr[idx : idx+3]
			buf[0] = ys[i]
			buf[1] = cbs[i>>xShift]
			buf[2] = crs[i>>xShift]
		}
	}
}

// nextFrame hands back the oldest decoded picture when its time has
// come, along with the number still queued behind it.
func (p *player) nextFrame(now float64) (*frame, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.frames) == 0 {
		return nil, false
	}
	if p.frames[0].pts != mpg.NoPTS && p.frames[0].pts > now {
		return nil, false
	}
	f := p.frames[0]
	p.frames = p.frames[1:]
	p.cond.Broadcast()
	return f, true
}

// peekFrame reports the time of the picture at the head of the queue.
func (p *player) peekFrame() (float64, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.frames) == 0 {
		return 0, false
	}
	return p.frames[0].pts, true
}

// recycle returns a shown picture's buffer for reuse.
func (p *player) recycle(f *frame) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.pool) < maxFrames+2 {
		p.pool = append(p.pool, f)
	}
}

// done reports whether the disc has been played out and everything
// decoded from it shown, and why the stream ended.
func (p *player) done() (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stopped && len(p.frames) == 0, p.err
}
