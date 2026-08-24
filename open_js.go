// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2026 The Media Authors

//go:build js

package main

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"syscall/js"

	"codeberg.org/totallygamerjet/media/dvdcss"
	"codeberg.org/totallygamerjet/media/dvdnav"
	"codeberg.org/totallygamerjet/media/dvdread"
)

// descrambler hands each VOB to libdvdcss as the reader it is. There is no
// drive in a browser to authenticate against, so the title keys are cracked
// out of the scrambled stream — which is the one thing about CSS that needs
// nothing but the bytes.
func descrambler(fsys browserFS, logger *slog.Logger) dvdread.CSSOpener {
	return func(name string, l *slog.Logger) (dvdread.CSS, error) {
		if l == nil {
			l = logger
		}
		f, err := fsys.Open(name)
		if err != nil {
			return nil, err
		}
		rs, ok := f.(io.ReadSeeker)
		if !ok {
			f.Close()
			return nil, fmt.Errorf("dvdplay: %s cannot be seeked", name)
		}
		return dvdcss.OpenReader(rs, l)
	}
}

// pickElementID is the element the page puts on screen for the viewer to
// press. The picker may only be opened from a gesture the viewer made, so
// there has to be something to press.
const pickElementID = "pick"

// openNav asks the viewer for the directory holding the disc and plays it
// from there. A browser has no filesystem to name a path in, so the path is
// not one, but the CSS flag still means what it says: there is no drive
// here to authenticate a scrambled disc against, and the keys are cracked
// out of the VOBs themselves instead.
func openNav(_ string, logger *slog.Logger, useCSS bool) (*dvdnav.DVDNav, error) {
	for {
		root, err := pickDirectory(logger)
		if err != nil {
			// No picker at all: pressing again will not help.
			tellPage("dvdplayFailed", err.Error())
			return nil, err
		}
		fsys := browserFS{root: root}
		var css dvdread.CSSOpener
		if useCSS {
			css = descrambler(fsys, logger)
		}
		nav, err := dvdnav.OpenFSCSS(fsys, logger, css)
		if err == nil {
			// Opening reads the IFOs, which a withheld disc hands over
			// quite happily; it is the video it keeps back. Find that
			// out now, while the viewer still has a screen to be told
			// on, rather than three seconds into a black one.
			if err = readableVideo(fsys); err == nil {
				tellPage("dvdplayStarted", root.Get("name").String())
				return nav, nil
			}
			nav.Close()
		}
		// The wrong folder is an easy mistake and an easy fix: say so
		// and leave the button there to be pressed again.
		logger.Error("that folder holds no disc", "error", err)
		tellPage("dvdplayFailed", err.Error())
	}
}

// unlockDrive has no drive to authenticate: a browser reads a folder the
// viewer picked, and the drive it may have come from is out of reach.
func unlockDrive(string, *slog.Logger) error {
	return errors.New("dvdplay: no drive to authenticate from a browser; " +
		"run dvdplay -unlock on the machine with the disc in it")
}

// reportFatal hands the page the error that ended playback, so that a disc
// which stops has something to show for it rather than a black screen.
func reportFatal(err error) {
	tellPage("dvdplayFatal", err.Error())
}

// readableVideo reads the first byte of the first title it can find, which
// is the question opening the disc does not ask: a disc the system is
// withholding gives up its IFOs and refuses its VOBs, so it opens perfectly
// and then stops at the first frame.
func readableVideo(fsys browserFS) error {
	entries, err := fsys.ReadDir("VIDEO_TS")
	if err != nil {
		// No VIDEO_TS to look in. Opening has already had its say about
		// that, so there is nothing to add here.
		return nil
	}
	for _, e := range entries {
		if !strings.HasSuffix(strings.ToUpper(e.Name()), "_1.VOB") {
			continue
		}
		f, err := fsys.Open("VIDEO_TS/" + e.Name())
		if err != nil {
			return err
		}
		defer f.Close()
		var b [1]byte
		if _, err := f.Read(b[:]); err != nil {
			return err
		}
		return nil
	}
	return nil // no titles to speak of; let playing find that out
}

// tellPage calls one of the hooks the page may have put up to follow along
// — dvdplayStarted once a disc is playing, dvdplayFailed when one could not
// be opened. A page that defines neither is none the wiser.
//
// Ebitengine puts its canvas up as soon as the program runs, long before
// there is a disc to draw on it, so the page cannot tell from the canvas
// alone when to clear itself out of the way. This is the program saying so.
func tellPage(hook, message string) {
	if fn := js.Global().Get(hook); fn.Type() == js.TypeFunction {
		fn.Invoke(message)
	}
}

// pickDirectory opens the browser's directory picker and hands back the
// directory the viewer chose.
//
// It waits for a press rather than asking straight away: the picker is only
// allowed to open while a gesture the viewer made is still being handled,
// so the call has to be made from inside the press itself. A viewer who
// changes their mind and dismisses it is not an error — the button is still
// there, and this goes on waiting for the next press.
func pickDirectory(logger *slog.Logger) (js.Value, error) {
	if js.Global().Get("showDirectoryPicker").IsUndefined() {
		return js.Undefined(), errors.New("dvdplay: this browser has no directory picker: " +
			"the File System Access API is needed to read a disc")
	}
	button := js.Global().Get("document").Call("getElementById", pickElementID)
	if !button.Truthy() {
		return js.Undefined(), errors.New("dvdplay: the page has no element with id " + pickElementID)
	}

	type picked struct {
		root js.Value
		err  error
	}
	ch := make(chan picked, 1)

	onPress := js.FuncOf(func(_ js.Value, _ []js.Value) any {
		// Called from within the press, which is what lets the picker
		// open. Await it on a goroutine so this returns at once and the
		// browser is not kept waiting on the handler.
		promise := js.Global().Call("showDirectoryPicker")
		go func() {
			root, err := await(promise)
			ch <- picked{root: root, err: err}
		}()
		return nil
	})
	defer onPress.Release()

	button.Call("addEventListener", "click", onPress)
	defer button.Call("removeEventListener", "click", onPress)

	// Only now is there anything listening, so only now is the button
	// worth pressing. The page enables it here rather than when the module
	// loads, which would leave a press between the two going nowhere.
	tellPage("dvdplayReady", "")

	for {
		p := <-ch
		if p.err == nil {
			return p.root, nil
		}
		// AbortError is the viewer dismissing the picker. Anything else
		// is worth saying, but neither is worth giving up over: they can
		// press it again.
		logger.Info("no directory chosen, waiting for another press", "reason", p.err)
	}
}
