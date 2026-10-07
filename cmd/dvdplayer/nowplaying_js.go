// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2026 The Media Authors

//go:build js

package main

import (
	"log/slog"
	"syscall/js"
)

// newNowPlaying has nothing to report to in a browser, but the browser is
// still the system here, and hiding the page — another app, another tab,
// the phone locked — is the one transport control it works: the film is
// paused, the way pressing pause would.
//
// Left running, the film does not carry on in the background regardless.
// Mobile Safari interrupts the page's audio when it goes out of view, and
// the clock the picture keeps is steered by the sound, so the picture
// comes back frozen on the frame it went away on; and every other browser
// stops drawing a hidden page, so a film left to play there is a film
// nobody sees. Paused, it comes back to the frame it left, and pressing
// play again — a tap, which the page takes to resume the audio the
// browser interrupted — carries on from there.
func newNowPlaying(cmds chan<- nowPlayingCommand, _ *slog.Logger) nowPlaying {
	doc := js.Global().Get("document")
	onChange := js.FuncOf(func(js.Value, []js.Value) any {
		if !doc.Get("hidden").Bool() {
			return nil
		}
		// A callback must not block, and a pause already on its way
		// does just as well as this one.
		select {
		case cmds <- npPause:
		default:
		}
		return nil
	})
	doc.Call("addEventListener", "visibilitychange", onChange)
	return &pageVisibility{doc: doc, onChange: onChange}
}

type pageVisibility struct {
	doc      js.Value
	onChange js.Func
}

func (*pageVisibility) update(nowPlayingState) {}
func (*pageVisibility) artwork([]byte)         {}

func (v *pageVisibility) close() {
	v.doc.Call("removeEventListener", "visibilitychange", v.onChange)
	v.onChange.Release()
}
