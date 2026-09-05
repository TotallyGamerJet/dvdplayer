// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2026 The Media Authors

package main

import "testing"

// TestSubpictureStream checks what the player takes the navigator's
// subpicture stream number to mean, bit 7 above all: it says the
// subpictures are to be held back, not that there is no stream. Reading
// it as no stream left the player taking whichever subpicture turned up
// first, which on a disc that lists French before English is French.
func TestSubpictureStream(t *testing.T) {
	tests := []struct {
		name   string
		spu    int
		want   byte
		wantOK bool
	}{
		{"the first stream", 0, 0x20, true},
		{"English, third on the disc", 2, 0x22, true},
		{"the last stream", 31, 0x3f, true},
		{"held back, still decoded", 0x80 | 2, 0x22, true},
		{"the last stream held back", 0x80 | 31, 0x3f, true},
		{"no stream at all", -1, 0, false},
		{"past the last stream", 32, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &disc{spu: [3]int{tt.spu, tt.spu, tt.spu}}
			got, ok := d.subpictureStream()
			if ok != tt.wantOK {
				t.Fatalf("stream %d: ok = %v, want %v", tt.spu, ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Errorf("stream %d decodes substream %#02x, want %#02x", tt.spu, got, tt.want)
			}
		})
	}
}

func TestLangCode(t *testing.T) {
	tests := []struct {
		in   string
		want uint16
	}{
		{"en", 'e'<<8 | 'n'},
		{"fr", 'f'<<8 | 'r'},
		{"es", 'e'<<8 | 's'},
		{"", 0},
		{"e", 0},
		{"eng", 0},
		{"EN", 0}, // the caller lower cases; this is what a DVD stores
		{"e1", 0},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got := langCode(tt.in)
			if got != tt.want {
				t.Errorf("langCode(%q) = %#04x, want %#04x", tt.in, got, tt.want)
			}
			if tt.want != 0 && langName(got) != tt.in {
				t.Errorf("langName(langCode(%q)) = %q", tt.in, langName(got))
			}
		})
	}
	if got := langName(0); got != "" {
		t.Errorf("langName(0) = %q, want the empty string", got)
	}
}
