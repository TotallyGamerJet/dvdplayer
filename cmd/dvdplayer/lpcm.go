// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2026 The Media Authors

package main

import (
	"encoding/binary"
	"math"

	"codeberg.org/totallygamerjet/media/lpcm"
)

// lpcmState holds what the track carries from one packet of a linear PCM
// soundtrack to the next. The audio device plays 48 kHz stereo, so the track
// mixes every stream down to that: the decoder returns the stream's own
// channels and sample rate, and the decimator brings 96 kHz audio to 48 kHz.
type lpcmState struct {
	dec    lpcm.Decoder
	down   lpcm.Decimator
	format lpcm.Format // the format of the previous packet, if known is true
	known  bool
}

// Reset discards the partial block, the filter history and the recorded
// format. The track calls it when the disc jumps or the soundtrack changes,
// since none of that state belongs to the sound that follows.
func (s *lpcmState) Reset() {
	s.dec.Reset()
	s.down.Reset()
	s.known = false
}

// decodeLPCM turns the packet in t.in into sound for the device.
func (t *audioTrack) decodeLPCM() {
	s := &t.lpcm
	samples, f, err := s.dec.Decode(nil, t.in)
	t.in = t.in[:0]
	if err != nil {
		// The packet is in a format this player does not play. Drop it
		// and let the silence that Read produces stand in for it.
		s.Reset()
		return
	}
	if !s.known || f != s.format {
		// The filter history belongs to the previous format.
		s.down.Reset()
		s.format, s.known = f, true
	}
	pairs := f.Stereo(nil, samples)
	if f.Rate != sampleRate {
		pairs = s.down.Feed(nil, pairs)
	}
	b := make([]byte, 4*len(pairs))
	for i, v := range pairs {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(v))
	}
	t.emit(b)
}
