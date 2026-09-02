// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2026 The Media Authors

package discdb

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
)

// The types below name the parts of TheDiscDb's schema this generator
// reads. They are deliberately partial: whatever they leave out still
// reaches a reader through the verbatim copy of the source document.

// srcDisc is a discNN.json, the description of one physical disc.
type srcDisc struct {
	Index        int
	Slug         string
	Name         string
	Format       string
	ContentHash  string
	GlobalDiscId string
	// DiscHash is how one record in the data set spells ContentHash.
	DiscHash string
	Titles   []srcTitle
}

// srcTitle is one playable title on a disc. Chapters and tracks are
// parsed into empty structs: only their number reaches the summary, and
// the full text is a fetch away in the copied disc document.
type srcTitle struct {
	Index      int
	Comment    string
	SourceFile string
	SegmentMap string
	Duration   string
	Size       int64
	Item       struct {
		Title    string
		Type     string
		Chapters []struct{}
	}
	Tracks []struct{}
}

// srcMeta is a title's metadata.json, the film or series the discs hold.
type srcMeta struct {
	Title       string
	FullTitle   string
	SortTitle   string
	Slug        string
	Type        string
	Year        int
	ExternalIds struct {
		Tmdb string
		Imdb string
	}
}

// srcRelease is a release.json, one packaging of a title.
type srcRelease struct {
	Slug   string
	Title  string
	Year   int
	Locale string
}

// srcRef is a discNN.ref, a disc described under another release.
type srcRef struct {
	ReleasePath string `json:"releasePath"`
	Disc        string `json:"disc"`
}

// The two hashes a disc is filed under.
const (
	// kindContent is the MD5 over the sizes of the files in VIDEO_TS or
	// BDMV/STREAM, which a player can work out from the disc in its
	// drive.
	kindContent = "contentHash"
	// kindGlobal is the disc id MakeMKV reports.
	kindGlobal = "globalDiscId"
)

// title is a film or series in the data set.
type title struct {
	dir   string // path under data, "movie/Heat (1995)"
	kind  string // "movie" or "series"
	meta  srcMeta
	raw   json.RawMessage
	tmdb  plumbing.Hash // zero when the title has no tmdb.json
	imdb  plumbing.Hash
	cover bool
}

// release is one packaging of a title, the directory the discs sit in.
type release struct {
	dir   string // path under data, "movie/Heat (1995)/2022-4k"
	title *title
	meta  srcRelease
	raw   json.RawMessage
	front bool
	back  bool
}

// disc is one discNN.json, held once however many releases list it.
type disc struct {
	release *release // the release that describes it
	name    string   // "disc01"
	blob    plumbing.Hash
	meta    srcDisc
}

// entry is a disc as filed under one release: the disc's own release, or
// another that reaches it through a .ref.
type entry struct {
	disc *disc
	rel  *release
	ref  bool
}

// contentHash reports the disc's MakeMKV content hash, the MD5 over the
// sizes of the files in VIDEO_TS or BDMV/STREAM that a player can work
// out for itself from a disc in the drive.
func (d *disc) contentHash() string {
	if d.meta.ContentHash != "" {
		return normalizeHash(d.meta.ContentHash)
	}
	return normalizeHash(d.meta.DiscHash)
}

func (d *disc) globalDiscID() string { return normalizeHash(d.meta.GlobalDiscId) }

// normalizeHash puts a hash in the one spelling the tree is keyed by:
// upper case hex, no separators. It returns "" for anything else, so a
// blank or malformed field never becomes a directory.
func normalizeHash(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	if s == "" {
		return ""
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'A' || c > 'F') {
			return ""
		}
	}
	return s
}

// durationSeconds turns a "2:11:34" or "11:34" duration into seconds. It
// reports 0 for anything it cannot read.
func durationSeconds(s string) int {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0
	}
	total := 0
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return 0
		}
		total = total*60 + n
	}
	return total
}
