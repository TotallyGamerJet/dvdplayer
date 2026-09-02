// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2026 The Media Authors

//go:build darwin

package main

import (
	"log/slog"
	"testing"
	"time"
	"unsafe"

	"github.com/ebitengine/purego/objc"
)

// goString reads an NSString back, so that a test can check what the
// system was actually told rather than what it was meant to be told.
func goString(id objc.ID) string {
	if id == 0 {
		return ""
	}
	p := objc.Send[*byte](id, objc.RegisterName("UTF8String"))
	if p == nil {
		return ""
	}
	var b []byte
	for q := p; *q != 0; q = (*byte)(unsafe.Add(unsafe.Pointer(q), 1)) {
		b = append(b, *q)
	}
	return string(b)
}

// TestNowPlayingRoundTrip pushes a state through MediaPlayer.framework
// and reads the info centre back. It checks the bridge — the framework
// loading, the property keys, the string and number boxing — not what
// Control Center chooses to draw, which is not visible from here.
func TestNowPlayingRoundTrip(t *testing.T) {
	m, err := loadMediaPlayer()
	if err != nil {
		t.Skip(err)
	}

	cmds := make(chan nowPlayingCommand, 1)
	np := newNowPlaying(cmds, slog.New(slog.DiscardHandler))
	mac, ok := np.(*macNowPlaying)
	if !ok {
		t.Fatalf("newNowPlaying gave %T, want the macOS one", np)
	}
	t.Cleanup(mac.close)

	want := nowPlayingState{
		title:    "Hearse",
		artist:   "A Christmas Carol",
		album:    "2010-DVD",
		elapsed:  42 * time.Second,
		duration: 74 * time.Second,
		playing:  true,
	}
	mac.update(want)

	info := mac.center.Send(objc.RegisterName("nowPlayingInfo"))
	if info == 0 {
		t.Fatal("the info centre took the state and reports none")
	}
	objectForKey := objc.RegisterName("objectForKey:")
	doubleValue := objc.RegisterName("doubleValue")

	for _, s := range []struct {
		name string
		key  objc.ID
		want string
	}{
		{"title", m.title, want.title},
		{"artist", m.artist, want.artist},
		{"album", m.album, want.album},
	} {
		if got := goString(info.Send(objectForKey, s.key)); got != s.want {
			t.Errorf("%s = %q, want %q", s.name, got, s.want)
		}
	}
	for _, n := range []struct {
		name string
		key  objc.ID
		want float64
	}{
		{"duration", m.duration, want.duration.Seconds()},
		{"elapsed", m.elapsed, want.elapsed.Seconds()},
		{"rate", m.rate, 1},
		{"media type", m.media, mpMediaTypeVideo},
	} {
		got := objc.Send[float64](info.Send(objectForKey, n.key), doubleValue)
		if got != n.want {
			t.Errorf("%s = %v, want %v", n.name, got, n.want)
		}
	}
	if state := objc.Send[uint](mac.center, objc.RegisterName("playbackState")); state != mpPlaybackStatePlaying {
		t.Errorf("playback state = %d, want %d", state, mpPlaybackStatePlaying)
	}

	// A paused disc must report a rate of zero, or the panel counts on
	// without it.
	want.playing = false
	mac.update(want)
	info = mac.center.Send(objc.RegisterName("nowPlayingInfo"))
	if got := objc.Send[float64](info.Send(objectForKey, m.rate), doubleValue); got != 0 {
		t.Errorf("rate while paused = %v, want 0", got)
	}
	if state := objc.Send[uint](mac.center, objc.RegisterName("playbackState")); state != mpPlaybackStatePaused {
		t.Errorf("playback state while paused = %d, want %d", state, mpPlaybackStatePaused)
	}
}

// TestNowPlayingCommands checks that the transport handlers the system
// holds reach the player, by invoking one the way the system would.
func TestNowPlayingCommands(t *testing.T) {
	if _, err := loadMediaPlayer(); err != nil {
		t.Skip(err)
	}
	cmds := make(chan nowPlayingCommand, 8)
	np := newNowPlaying(cmds, slog.New(slog.DiscardHandler))
	mac, ok := np.(*macNowPlaying)
	if !ok {
		t.Fatalf("newNowPlaying gave %T, want the macOS one", np)
	}
	t.Cleanup(mac.close)

	if len(mac.blocks) == 0 {
		t.Fatal("no transport controls were taken; without one macOS publishes nothing")
	}
	for i, block := range mac.blocks {
		status, err := objc.InvokeBlock[int](block, objc.ID(0))
		if err != nil {
			t.Fatalf("handler %d: %v", i, err)
		}
		if status != mpCommandSuccess {
			t.Errorf("handler %d returned %d, want %d", i, status, mpCommandSuccess)
		}
	}
	if len(cmds) != len(mac.blocks) {
		t.Errorf("%d commands reached the player, want %d", len(cmds), len(mac.blocks))
	}
}

// TestNowPlayingCommandsDoNotBlock checks that a handler whose send
// cannot be taken returns rather than holding up the system's queue.
func TestNowPlayingCommandsDoNotBlock(t *testing.T) {
	if _, err := loadMediaPlayer(); err != nil {
		t.Skip(err)
	}
	cmds := make(chan nowPlayingCommand) // nothing reading
	np := newNowPlaying(cmds, slog.New(slog.DiscardHandler))
	mac, ok := np.(*macNowPlaying)
	if !ok {
		t.Fatalf("newNowPlaying gave %T, want the macOS one", np)
	}
	t.Cleanup(mac.close)

	done := make(chan int, 1)
	go func() {
		status, err := objc.InvokeBlock[int](mac.blocks[0], objc.ID(0))
		if err != nil {
			t.Error(err)
		}
		done <- status
	}()
	select {
	case status := <-done:
		if status != mpCommandNoSuchItem {
			t.Errorf("handler returned %d, want %d for a command it could not take", status, mpCommandNoSuchItem)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the handler blocked on a command nothing was reading")
	}
}
