// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2026 The Media Authors

// Command dvdplay plays a DVD: a device, an ISO image or a directory
// holding a VIDEO_TS tree. It runs the disc's own navigation program
// through this module's dvdnav package, descrambles a CSS protected disc
// with the dvdcss package, decodes the MPEG-2 video and the AC3, MPEG or
// linear PCM sound, and draws the result with Ebitengine, so the disc's
// menus work the way they do on a set-top player.
//
//	dvdplay
//	dvdplay /dev/rdisk4
//	dvdplay -title 1 movie.iso
//	dvdplay /Volumes/MOVIE/VIDEO_TS
//
// With no path it plays the system's default DVD drive.
//
// The controls are:
//
//	arrows        move between a menu's buttons, or seek ten seconds
//	enter, space  press the highlighted button, or pause the film
//	escape        leave the menu and resume
//	m, t          call up the root and the title menu
//	backspace     go up one menu
//	n, p          the next and the previous chapter
//	a, s          the next soundtrack and the next subtitle track
//	u             show or hide the subtitles
//	i             show or hide the status line
//	f             fill the screen
//	q             quit
//
// The mouse works in menus too: move it to pick out a button and click
// to press it.
package main

import (
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"
	"time"

	"github.com/hajimehoshi/ebiten/v2"

	"codeberg.org/totallygamerjet/media/dvdnav"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("dvdplay: ")

	title := flag.Int("title", 0, "start at this title (0 runs the disc's first play program)")
	part := flag.Int("part", 1, "start at this chapter of -title")
	css := flag.Bool("css", true, "descramble a CSS protected disc with the dvdcss package")
	readahead := flag.Bool("readahead", true, "read ahead of the decoder, which an optical drive needs")
	verbose := flag.Bool("v", false, "report what the navigator is doing")
	unlock := flag.Bool("unlock", false,
		"authenticate the drive against the disc in it and exit, so that a\n"+
			"disc the system withholds can be read through the mount")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: dvdplay [flags] [path]\n\n"+
			"path is a DVD device such as /dev/rdisk4, an ISO image, or a\n"+
			"directory holding a VIDEO_TS tree. With no path, the system's\n"+
			"default DVD drive is played.\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if flag.NArg() > 1 {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(flag.Arg(0), *title, *part, *css, *readahead, *verbose, *unlock); err != nil {
		// Where there is a page rather than a terminal, say so there
		// too: the chrome is out of the way by the time playback fails,
		// and a viewer left looking at black learns nothing.
		reportFatal(err)
		log.Fatalln(err)
	}
}

func run(path string, title, part int, useCSS, readahead, verbose, unlock bool) error {
	level := slog.LevelError
	if verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if unlock {
		return unlockDrive(path, logger)
	}

	d, err := openDisc(path, logger, useCSS, readahead)
	if err != nil {
		return err
	}
	defer d.Close()

	printDisc(d.nav, path)

	if title > 0 {
		if err := d.nav.PartPlay(int32(title), int32(part)); err != nil {
			return fmt.Errorf("cannot play title %d chapter %d: %s", title, part, d.nav.ErrToString())
		}
	}

	p := newPlayer(d)
	d.start()
	p.start()
	defer p.close()

	g, err := newGame(d, p)
	if err != nil {
		return err
	}

	ebiten.SetWindowTitle(windowTitle(d.nav))
	ebiten.SetWindowSize(720, 540)
	ebiten.SetWindowResizingMode(ebiten.WindowResizingModeEnabled)
	if err := ebiten.RunGame(g); err != nil {
		return err
	}
	return nil
}

func windowTitle(nav *dvdnav.DVDNav) string {
	if s := nav.GetTitleString(); s != "" {
		return s + " — dvdplay"
	}
	return "dvdplay"
}

func printDisc(nav *dvdnav.DVDNav, path string) {
	fmt.Printf("Disc:    %s\n", path)
	fmt.Printf("Title:   %s\n", nav.GetTitleString())
	fmt.Printf("Serial:  %s\n", nav.GetSerialString())
	if volid, err := nav.GetVolIDString(); err == nil {
		fmt.Printf("Volume:  %s\n", volid)
	}
	if titles, err := nav.NumberOfTitles(); err == nil {
		fmt.Printf("Titles:  %d\n", titles)
		for t := int32(1); t <= titles && t <= 10; t++ {
			parts, _ := nav.NumberOfParts(t)
			angles, _ := nav.NumberOfAngles(t)
			_, duration, err := nav.DescribeTitleChapters(t)
			if err != nil {
				fmt.Printf("  %2d: %d chapter(s), %d angle(s)\n", t, parts, angles)
				continue
			}
			fmt.Printf("  %2d: %d chapter(s), %d angle(s), %s\n", t, parts, angles, ticks(duration))
		}
		if titles > 10 {
			fmt.Printf("  ... and %d more\n", titles-10)
		}
	}
	if mask, err := nav.GetDiskRegionMask(); err == nil {
		fmt.Printf("Regions: %#02x\n", mask)
	}
	fmt.Println()
}

// ticks turns a count of 90 kHz PTS ticks into a duration.
func ticks(t uint64) time.Duration {
	return (time.Duration(t) * time.Second / 90000).Round(time.Second)
}
