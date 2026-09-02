// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2026 The Media Authors

package discdb

import (
	"encoding/json"
	"time"
)

// schemaVersion rises whenever the layout of the generated tree changes
// in a way a reader must know about. It is the schema field of every
// document below.
const schemaVersion = 2

// discsDir is the directory the hashes live under, one level down from
// the root so that index.json, hashes.txt and titles are not lost among
// eight thousand hash directories.
const discsDir = "discs"

// The types below are the documents the generator writes. Every path in
// them is relative to the root of the generated tree, so a reader joins
// them onto whatever base URL it fetched from and needs to know nothing
// else about the layout.

// manifest is index.json at the root of the tree.
type manifest struct {
	Schema    int            `json:"schema"`
	Generated string         `json:"generated"`
	Source    manifestSource `json:"source"`
	Layout    manifestLayout `json:"layout"`
	Counts    manifestCounts `json:"counts"`
}

type manifestSource struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
	Commit string `json:"commit"`
	Date   string `json:"date"`
}

// manifestLayout spells out where a reader finds each document, so that
// a change of layout is something it can notice rather than guess at.
type manifestLayout struct {
	Info   string `json:"info"`
	Disc   string `json:"disc"`
	Title  string `json:"title"`
	Hashes string `json:"hashes"`
	Note   string `json:"note"`
}

type manifestCounts struct {
	Hashes        int `json:"hashes"`
	ContentHashes int `json:"contentHashes"`
	GlobalDiscIds int `json:"globalDiscIds"`
	Discs         int `json:"discs"`
	Matches       int `json:"matches"`
	Releases      int `json:"releases"`
	Titles        int `json:"titles"`
}

// info is {HASH}/info.json, everything known about the disc that hashes
// to HASH. One fetch of it answers what the disc is; the links point at
// the larger documents for a reader that wants them.
type info struct {
	Schema  int      `json:"schema"`
	Hash    string   `json:"hash"`
	Types   []string `json:"types"` // "contentHash", "globalDiscId", or both
	Count   int      `json:"count"`
	Matches []match  `json:"matches"`
}

// match is one release that holds this disc. The matches are ordered by
// collection, and a match's position is the subdirectory it was written
// to: matches[2] is at {HASH}/2/.
type match struct {
	Index      int             `json:"index"`
	Collection string          `json:"collection"`
	Kind       string          `json:"kind"` // "movie" or "series"
	Ref        bool            `json:"ref,omitempty"`
	Disc       discSummary     `json:"disc"`
	Title      json.RawMessage `json:"title"`
	Release    json.RawMessage `json:"release"`
	Links      links           `json:"links"`
}

// discSummary is enough of the disc to put a name on it without
// fetching disc.json.
type discSummary struct {
	File         string   `json:"file"`
	Index        int      `json:"index"`
	Slug         string   `json:"slug,omitempty"`
	Name         string   `json:"name"`
	Format       string   `json:"format"`
	ContentHash  string   `json:"contentHash,omitempty"`
	GlobalDiscId string   `json:"globalDiscId,omitempty"`
	Titles       int      `json:"titles"`
	Feature      *feature `json:"feature,omitempty"`
	// TitleList indexes every title on the disc, and is written for
	// DVDs only: it is what lets a player name the segment it is
	// playing without fetching disc.json. On a Blu-ray it is left out,
	// because those discs run to hundreds of titles and the saving does
	// not pay for the size. Absent means fetch disc.json.
	TitleList []titleEntry `json:"titleList,omitempty"`
}

// titleEntry is one title of a disc, enough of it to put a name and a
// running time on screen. It is the same title disc.json describes at
// the same Index.
type titleEntry struct {
	Index int `json:"index"`
	// TitleNumber is the DVD title number, 1 to 99, that a player asks
	// its navigator for. It is written whenever the disc is a DVD and
	// SourceFile names a title number, which upstream it always does.
	TitleNumber int    `json:"titleNumber,omitempty"`
	SourceFile  string `json:"sourceFile,omitempty"`
	Title       string `json:"title,omitempty"`
	Type        string `json:"type,omitempty"`
	Duration    string `json:"duration,omitempty"`
	Seconds     int    `json:"seconds,omitempty"`
	Chapters    int    `json:"chapters"`
}

// feature is the disc's main title: the one marked MainMovie, or else
// the longest, which is what a player wants to start with.
type feature struct {
	Index int `json:"index"`
	// TitleNumber is the DVD title number, as in titleEntry. It is
	// absent on a Blu-ray, where SourceFile names a file instead.
	TitleNumber int    `json:"titleNumber,omitempty"`
	Title       string `json:"title,omitempty"`
	Type        string `json:"type,omitempty"`
	SourceFile  string `json:"sourceFile,omitempty"`
	SegmentMap  string `json:"segmentMap,omitempty"`
	Duration    string `json:"duration,omitempty"`
	Seconds     int    `json:"seconds,omitempty"`
	Size        int64  `json:"size,omitempty"`
	Chapters    int    `json:"chapters"`
}

// links locates the documents a match does not inline. The first four
// are paths within this tree; the rest are upstream URLs pinned to the
// commit the tree was built from.
type links struct {
	Disc   string `json:"disc"`
	Title  string `json:"title"`
	Tmdb   string `json:"tmdb,omitempty"`
	Imdb   string `json:"imdb,omitempty"`
	Cover  string `json:"cover,omitempty"`
	Front  string `json:"front,omitempty"`
	Back   string `json:"back,omitempty"`
	Source string `json:"source"`
}

func timestamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }
