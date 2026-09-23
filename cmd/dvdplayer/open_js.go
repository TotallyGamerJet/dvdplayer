// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2026 The Media Authors

//go:build js

package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"strings"
	"syscall/js"

	"codeberg.org/totallygamerjet/media/dvdcss"
	"codeberg.org/totallygamerjet/media/dvdnav"
	"codeberg.org/totallygamerjet/media/dvdread"
	"codeberg.org/totallygamerjet/media/udf"
)

// descrambler hands each VOB to libdvdcss as the reader it is. There is no
// drive in a browser to authenticate against, so the title keys are cracked
// out of the scrambled stream — which is the one thing about CSS that needs
// nothing but the bytes. fsys is a directory the viewer picked or a single
// image parsed by the udf package; either way it hands back a VOB seekable
// on its own.
func descrambler(fsys fs.FS, logger *slog.Logger) dvdread.CSSOpener {
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

// pickElementID is the directory picker button. It only exists where the
// File System Access API does — a Chromium browser, as of writing — so it
// is one of two ways the viewer may hand over a disc; see pickFileElementID
// for the other, which every browser offers. The picker may only be opened
// from a gesture the viewer made, so there has to be something to press.
const pickElementID = "pick"

// pickFileElementID is the plain <input type="file"> the page offers for
// choosing a single ISO or cdr image, which needs nothing beyond the File
// API — every browser this module is likely to run in has that, Safari
// included, unlike the directory picker.
const pickFileElementID = "pickFile"

// openNav asks the viewer where the disc is — a VIDEO_TS folder or a single
// image file, whichever the page offers and the viewer picks — and plays it
// from there. A browser has no filesystem to name a path in, so the path is
// not one, but the CSS flag still means what it says: there is no drive
// here to authenticate a scrambled disc against, and the keys are cracked
// out of the VOBs, or the image, themselves instead.
func openNav(_ string, logger *slog.Logger, useCSS bool) (*dvdnav.DVDNav, error) {
	for {
		src, err := pickSource(logger)
		if err != nil {
			// No picker at all: pressing again will not help.
			tellPage("dvdplayFailed", err.Error())
			return nil, err
		}

		var nav *dvdnav.DVDNav
		var name string
		switch {
		case src.dir.Truthy():
			name = src.dir.Get("name").String()
			nav, err = openDirectory(src.dir, logger, useCSS)
		case src.file.Truthy():
			name = src.file.Get("name").String()
			nav, err = openImageFile(src.file, logger, useCSS)
		}
		if err == nil {
			tellPage("dvdplayStarted", name)
			return nav, nil
		}
		// The wrong folder or file is an easy mistake and an easy fix:
		// say so and leave the pickers there to be tried again.
		logger.Error("that did not hold a disc", "error", err)
		tellPage("dvdplayFailed", err.Error())
	}
}

// openDirectory opens a disc from a VIDEO_TS folder the viewer picked.
func openDirectory(root js.Value, logger *slog.Logger, useCSS bool) (*dvdnav.DVDNav, error) {
	fsys := browserFS{root: root}
	var css dvdread.CSSOpener
	if useCSS {
		css = descrambler(fsys, logger)
	}
	nav, err := dvdnav.OpenFSCSS(fsys, logger, css)
	if err != nil {
		return nil, err
	}
	// Opening reads the IFOs, which a withheld disc hands over quite
	// happily; it is the video it keeps back. Find that out now, while
	// the viewer still has a screen to be told on, rather than three
	// seconds into a black one.
	if err := readableVideo(fsys); err != nil {
		nav.Close()
		return nil, err
	}
	return nav, nil
}

// openImageFile opens a disc from a single ISO or cdr image the viewer
// picked through the plain file input. The image has no directory of its
// own to read through — it is one file the disc's own UDF filesystem is
// packed into — so the udf package reads that filesystem out of it, the
// same way dvdread reads one out of a path or a device: dvdread's own
// [dvdread.OpenFS] doc says a filesystem may be "whatever else a caller
// can put behind io/fs", and a [*udf.FS] is exactly that.
//
// Unlike a mounted VIDEO_TS tree, an ordinary file is never withheld
// pending a drive handshake — there is no drive here for the system to be
// guarding it against — so there is nothing to check for before handing
// the navigator back, the way [openDirectory] checks with readableVideo.
func openImageFile(file js.Value, logger *slog.Logger, useCSS bool) (*dvdnav.DVDNav, error) {
	name := file.Get("name").String()
	size := int64(file.Get("size").Float())
	reader := &browserFile{name: name, file: file, size: size}

	fsys, err := udf.New(reader, size, udf.WithCaseFolding(), udf.WithLogger(logger))
	if err != nil {
		return nil, fmt.Errorf("dvdplay: %s does not hold a DVD image: %w", name, err)
	}

	var css dvdread.CSSOpener
	if useCSS {
		css = descrambler(fsys, logger)
	}
	return dvdnav.OpenFSCSS(fsys, logger, css)
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

// pickedSource is what the viewer handed over: exactly one of dir (a
// FileSystemDirectoryHandle from the directory picker) and file (a File
// from the plain file input) is truthy.
type pickedSource struct {
	dir  js.Value
	file js.Value
}

// pickSource waits for the viewer to hand over a disc through whichever of
// the page's pickers they use first: the directory picker, where the
// browser has one, or the plain file input, which every browser does.
//
// It waits for the pickers to be used rather than opening one straight
// away: the directory picker in particular may only be opened from a
// gesture the viewer made, so that call has to be made from inside the
// press itself. A viewer who opens the directory picker and dismisses it is
// not an error — the pickers are still there, and this goes on waiting.
func pickSource(logger *slog.Logger) (pickedSource, error) {
	fileInput := js.Global().Get("document").Call("getElementById", pickFileElementID)
	if !fileInput.Truthy() {
		return pickedSource{}, errors.New("dvdplay: the page has no element with id " + pickFileElementID)
	}
	haveDirPicker := !js.Global().Get("showDirectoryPicker").IsUndefined()
	var dirButton js.Value
	if haveDirPicker {
		dirButton = js.Global().Get("document").Call("getElementById", pickElementID)
		if !dirButton.Truthy() {
			return pickedSource{}, errors.New("dvdplay: the page has no element with id " + pickElementID)
		}
	}

	type picked struct {
		src pickedSource
		err error
	}
	ch := make(chan picked, 1)

	if haveDirPicker {
		onPress := js.FuncOf(func(_ js.Value, _ []js.Value) any {
			// Called from within the press, which is what lets the
			// picker open. Await it on a goroutine so this returns at
			// once and the browser is not kept waiting on the handler.
			promise := js.Global().Call("showDirectoryPicker")
			go func() {
				root, err := await(promise)
				ch <- picked{src: pickedSource{dir: root}, err: err}
			}()
			return nil
		})
		defer onPress.Release()
		dirButton.Call("addEventListener", "click", onPress)
		defer dirButton.Call("removeEventListener", "click", onPress)
	}

	onChange := js.FuncOf(func(_ js.Value, _ []js.Value) any {
		files := fileInput.Get("files")
		if files.Get("length").Int() == 0 {
			// Some browsers fire change on a dismissed dialog too; the
			// input is still there to be tried again.
			return nil
		}
		ch <- picked{src: pickedSource{file: files.Index(0)}}
		return nil
	})
	defer onChange.Release()
	fileInput.Call("addEventListener", "change", onChange)
	defer fileInput.Call("removeEventListener", "change", onChange)

	// Only now is there anything listening, so only now are the pickers
	// worth using. The page enables them here rather than when the module
	// loads, which would leave a press between the two going nowhere.
	tellPage("dvdplayReady", "")

	for {
		p := <-ch
		if p.err == nil {
			return p.src, nil
		}
		// AbortError is the viewer dismissing the picker. Anything else
		// is worth saying, but neither is worth giving up over: they can
		// try again.
		logger.Info("no disc chosen, waiting for another try", "reason", p.err)
	}
}
