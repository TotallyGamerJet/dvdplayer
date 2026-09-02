// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2026 The Media Authors

//go:build darwin

package main

import (
	"fmt"
	"log/slog"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"
)

// This file puts the disc on macOS's Now Playing display — the panel in
// Control Center and on the lock screen — and takes the media keys back
// off it. It talks to MediaPlayer.framework through the Objective-C
// runtime rather than through cgo, the way the dvdcss package talks to
// IOKit.
//
// Two Objective-C rules shape what follows. Nothing here may be touched
// before the framework has loaded, so the whole thing is behind a
// sync.OnceValue and a failure to load leaves the player with the
// do-nothing implementation. And a Now Playing session is only published
// for a process that has taken at least one remote command, which is why
// the transport controls below are not optional decoration.
//
// What the system then does with all this cannot be seen from inside the
// process: the info centre reads back whatever it was handed whether or
// not anything is showing it, and reading another process's is behind a
// private framework. The tests here go as far as the framework and stop.
// If the panel stays empty, the thing to suspect first is that dvdplay
// is a bare executable rather than an application bundle, since macOS
// hangs a good deal of this off a bundle identifier.

// MPNowPlayingPlaybackState.
const (
	mpPlaybackStatePlaying = 1
	mpPlaybackStatePaused  = 2
	mpPlaybackStateStopped = 3
)

// MPNowPlayingInfoMediaType.
const mpMediaTypeVideo = 2

// MPRemoteCommandHandlerStatus.
const (
	mpCommandSuccess    = 0
	mpCommandNoSuchItem = 100
)

// mediaPlayer is everything loaded out of the system frameworks: the
// selectors and classes are looked up once, and the property keys are
// NSString constants exported by MediaPlayer.framework, which have to be
// read out of the framework rather than spelled out here — several of
// them are not the strings their names suggest.
type mediaPlayer struct {
	// The Now Playing info dictionary keys.
	title, artist, album           objc.ID
	duration, elapsed, rate, media objc.ID

	nsString, nsNumber, nsDictionary objc.Class
	infoCenter, commandCenter        objc.Class

	stringWithUTF8String, numberWithDouble, numberWithInt objc.SEL
	dictionary, setObjectForKey                           objc.SEL
	defaultCenter, setNowPlayingInfo, setPlaybackState    objc.SEL
	sharedCommandCenter, setEnabled, addTargetWithHandler objc.SEL

	poolPush func() unsafe.Pointer
	poolPop  func(unsafe.Pointer)
}

// loadMediaPlayer opens the frameworks and looks everything up. It is
// run at most once, whether or not it succeeded.
var loadMediaPlayer = sync.OnceValues(func() (*mediaPlayer, error) {
	mp, err := purego.Dlopen("/System/Library/Frameworks/MediaPlayer.framework/MediaPlayer",
		purego.RTLD_GLOBAL|purego.RTLD_NOW)
	if err != nil {
		return nil, fmt.Errorf("dvdplay: cannot load MediaPlayer.framework: %w", err)
	}
	objcLib, err := purego.Dlopen("/usr/lib/libobjc.A.dylib", purego.RTLD_GLOBAL|purego.RTLD_NOW)
	if err != nil {
		return nil, fmt.Errorf("dvdplay: cannot load libobjc: %w", err)
	}

	m := &mediaPlayer{
		nsString:     objc.GetClass("NSString"),
		nsNumber:     objc.GetClass("NSNumber"),
		nsDictionary: objc.GetClass("NSMutableDictionary"),

		infoCenter:    objc.GetClass("MPNowPlayingInfoCenter"),
		commandCenter: objc.GetClass("MPRemoteCommandCenter"),

		stringWithUTF8String: objc.RegisterName("stringWithUTF8String:"),
		numberWithDouble:     objc.RegisterName("numberWithDouble:"),
		numberWithInt:        objc.RegisterName("numberWithInt:"),
		dictionary:           objc.RegisterName("dictionary"),
		setObjectForKey:      objc.RegisterName("setObject:forKey:"),
		defaultCenter:        objc.RegisterName("defaultCenter"),
		setNowPlayingInfo:    objc.RegisterName("setNowPlayingInfo:"),
		setPlaybackState:     objc.RegisterName("setPlaybackState:"),
		sharedCommandCenter:  objc.RegisterName("sharedCommandCenter"),
		setEnabled:           objc.RegisterName("setEnabled:"),
		addTargetWithHandler: objc.RegisterName("addTargetWithHandler:"),
	}
	for _, c := range []struct {
		name string
		cls  objc.Class
	}{
		{"NSString", m.nsString},
		{"NSNumber", m.nsNumber},
		{"NSMutableDictionary", m.nsDictionary},
		{"MPNowPlayingInfoCenter", m.infoCenter},
		{"MPRemoteCommandCenter", m.commandCenter},
	} {
		if c.cls == 0 {
			return nil, fmt.Errorf("dvdplay: the Objective-C runtime has no %s class", c.name)
		}
	}

	for _, k := range []struct {
		name string
		into *objc.ID
	}{
		{"MPMediaItemPropertyTitle", &m.title},
		{"MPMediaItemPropertyArtist", &m.artist},
		{"MPMediaItemPropertyAlbumTitle", &m.album},
		{"MPMediaItemPropertyPlaybackDuration", &m.duration},
		{"MPNowPlayingInfoPropertyElapsedPlaybackTime", &m.elapsed},
		{"MPNowPlayingInfoPropertyPlaybackRate", &m.rate},
		{"MPNowPlayingInfoPropertyMediaType", &m.media},
	} {
		p, err := purego.Dlsym(mp, k.name)
		if err != nil || p == 0 {
			return nil, fmt.Errorf("dvdplay: MediaPlayer.framework exports no %s", k.name)
		}
		// The symbol is an NSString *const, so its address holds the
		// pointer rather than being it.
		*k.into = objc.ID(**(**uintptr)(unsafe.Pointer(&p)))
	}

	purego.RegisterLibFunc(&m.poolPush, objcLib, "objc_autoreleasePoolPush")
	purego.RegisterLibFunc(&m.poolPop, objcLib, "objc_autoreleasePoolPop")
	return m, nil
})

// str turns a Go string into an autoreleased NSString.
func (m *mediaPlayer) str(s string) objc.ID {
	b := append([]byte(s), 0)
	return objc.ID(m.nsString).Send(m.stringWithUTF8String, unsafe.Pointer(&b[0]))
}

// num turns a Go float into an autoreleased NSNumber.
func (m *mediaPlayer) num(v float64) objc.ID {
	return objc.ID(m.nsNumber).Send(m.numberWithDouble, v)
}

// macNowPlaying is the disc as macOS's Now Playing display sees it.
type macNowPlaying struct {
	mp     *mediaPlayer
	center objc.ID
	log    *slog.Logger

	// blocks are the transport control handlers. The command centre
	// keeps them for as long as the process runs, so they are held here
	// too: releasing one the system still has would leave it calling
	// into freed memory.
	blocks []objc.Block
}

// newNowPlaying puts the player on macOS's Now Playing display and takes
// the media keys, sending what they ask for to cmds.
//
// It returns the do-nothing implementation where the frameworks will not
// load, since a player that cannot reach Control Center should still
// play the disc.
func newNowPlaying(cmds chan<- nowPlayingCommand, log *slog.Logger) nowPlaying {
	m, err := loadMediaPlayer()
	if err != nil {
		log.Debug("no Now Playing display", "err", err)
		return nopNowPlaying{}
	}
	center := objc.ID(m.infoCenter).Send(m.defaultCenter)
	if center == 0 {
		log.Debug("no Now Playing display", "err", "MPNowPlayingInfoCenter has no default centre")
		return nopNowPlaying{}
	}
	n := &macNowPlaying{mp: m, center: center, log: log}
	n.takeCommands(cmds)
	return n
}

// takeCommands enables the transport controls the disc can answer and
// points them at cmds.
//
// A command handler runs on a thread of the system's own, so it does no
// more than offer the command to the player: the send does not block,
// because a viewer leaning on a media key must not be able to stall the
// system's main queue, and a command dropped because the player has not
// caught up is one the viewer can simply give again.
func (n *macNowPlaying) takeCommands(cmds chan<- nowPlayingCommand) {
	center := objc.ID(n.mp.commandCenter).Send(n.mp.sharedCommandCenter)
	if center == 0 {
		return
	}
	for _, c := range []struct {
		name string
		cmd  nowPlayingCommand
	}{
		{"playCommand", npPlay},
		{"pauseCommand", npPause},
		{"togglePlayPauseCommand", npToggle},
		{"nextTrackCommand", npNext},
		{"previousTrackCommand", npPrev},
	} {
		command := center.Send(objc.RegisterName(c.name))
		if command == 0 {
			continue
		}
		want := c.cmd
		block := objc.NewBlock(func(objc.Block, objc.ID) int {
			select {
			case cmds <- want:
				return mpCommandSuccess
			default:
				return mpCommandNoSuchItem
			}
		})
		command.Send(n.mp.setEnabled, true)
		command.Send(n.mp.addTargetWithHandler, block)
		n.blocks = append(n.blocks, block)
	}
}

func (n *macNowPlaying) update(s nowPlayingState) {
	m := n.mp
	// Everything built below is autoreleased, and the player's thread is
	// not one the system drains a pool on, so it brings its own.
	pool := m.poolPush()
	defer m.poolPop(pool)

	info := objc.ID(m.nsDictionary).Send(m.dictionary)
	set := func(key, value objc.ID) {
		if key != 0 && value != 0 {
			info.Send(m.setObjectForKey, value, key)
		}
	}
	set(m.title, m.str(s.title))
	set(m.artist, m.str(s.artist))
	set(m.album, m.str(s.album))
	set(m.media, m.num(mpMediaTypeVideo))
	if s.duration > 0 {
		set(m.duration, m.num(s.duration.Seconds()))
	}
	set(m.elapsed, m.num(s.elapsed.Seconds()))

	// The rate is what the system counts the elapsed time on with, so a
	// paused disc must report zero or the panel keeps ticking.
	rate, state := 1.0, mpPlaybackStatePlaying
	if !s.playing {
		rate, state = 0, mpPlaybackStatePaused
	}
	set(m.rate, m.num(rate))

	n.center.Send(m.setNowPlayingInfo, info)
	n.center.Send(m.setPlaybackState, uint(state))
}

// close clears the display, so that a player that has quit is not left
// sitting in Control Center as though it were still going.
func (n *macNowPlaying) close() {
	m := n.mp
	pool := m.poolPush()
	defer m.poolPop(pool)

	n.center.Send(m.setPlaybackState, uint(mpPlaybackStateStopped))
	n.center.Send(m.setNowPlayingInfo, objc.ID(0))
}
