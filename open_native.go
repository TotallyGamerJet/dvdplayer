// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2026 The Media Authors

//go:build !js

package main

import (
	"fmt"
	"log/slog"

	"codeberg.org/totallygamerjet/media/dvdcss"
	"codeberg.org/totallygamerjet/media/dvdnav"
	"codeberg.org/totallygamerjet/media/dvdread"
)

// reportFatal has somewhere to report to only in a browser; a terminal has
// had the error printed to it already.
func reportFatal(error) {}

// unlockDrive authenticates the drive against the disc in it and stops there.
//
// Some discs are withheld by the operating system until the drive has been
// through the CSS handshake: the filesystem serves the IFOs and refuses the
// scrambled VOBs, so nothing that reads through the mount — a browser, most
// of all — can get at them. Authenticating once lifts that for as long as
// the disc is in, and the VOBs then read as scrambled data, which is what
// libdvdcss wants.
//
// It opens and reads the way playing does, since that is the sequence known
// to lift it, and then closes rather than going on to play.
func unlockDrive(path string, logger *slog.Logger) error {
	nav, err := openNav(path, logger, true)
	if err != nil {
		return err
	}
	defer nav.Close()

	var buf [dvdread.DVDVideoLBLen]byte
	for range 64 {
		if _, err := nav.GetNextBlock(buf[:]); err != nil {
			return fmt.Errorf("dvdplay: reading the disc after authenticating: %w", err)
		}
	}
	fmt.Println("drive authenticated: the disc can now be read by anything reading through the mount")
	return nil
}

// openNav opens the disc at path — a device, an ISO image or a directory
// holding a VIDEO_TS tree — descrambling it where the viewer asked for
// that and the path names a drive. An empty path means the drive the
// system would pick.
func openNav(path string, logger *slog.Logger, useCSS bool) (*dvdnav.DVDNav, error) {
	if path == "" {
		var err error
		if path, err = dvdcss.DefaultDevice(logger); err != nil {
			return nil, fmt.Errorf("%w; name a disc to play instead", err)
		}
	}
	var opener dvdread.CSSOpener
	if useCSS {
		opener = func(path string, logger *slog.Logger) (dvdread.CSS, error) {
			// libdvdread hands its own logger down, and may hand
			// down none at all.
			if logger == nil {
				logger = slog.New(slog.DiscardHandler)
			}
			return dvdcss.Open(path, logger)
		}
	}
	return dvdnav.OpenCSS(path, logger, opener)
}
