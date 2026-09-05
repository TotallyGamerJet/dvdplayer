// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2026 The Media Authors

//go:build !darwin

package main

import "log/slog"

// newNowPlaying reports nothing anywhere but macOS, which is the only
// system this player knows how to tell.
func newNowPlaying(chan<- nowPlayingCommand, *slog.Logger) nowPlaying {
	return nopNowPlaying{}
}
