// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2026 The Media Authors

package discdb

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// dataset is the part of the data repository this generator cares about,
// held in memory. The disc documents themselves are not: only what a
// summary needs, and the blob to copy the rest from.
type dataset struct {
	titles   map[string]*title   // keyed by path under data
	releases map[string]*release // keyed by path under data
	discs    map[string]*disc    // keyed by "<release dir>/<disc name>"
	refs     map[string]srcRef   // same key, pointing at another disc
}

// collect walks one commit and reads every document the tree is built
// from. Artwork is noted but never read: the links point upstream at it.
func collect(repo *git.Repository, commit *object.Commit) (*dataset, error) {
	tree, err := commit.Tree()
	if err != nil {
		return nil, err
	}

	ds := &dataset{
		titles:   map[string]*title{},
		releases: map[string]*release{},
		discs:    map[string]*disc{},
		refs:     map[string]srcRef{},
	}
	// Files arrive in whatever order the tree holds them, so the walk
	// only fills these tables in; the links between them are made after.
	var (
		metas = map[string][]byte{}        // title dir -> metadata.json
		rels  = map[string][]byte{}        // release dir -> release.json
		flags = map[string]bool{}          // artwork path under data -> it exists
		blobs = map[string]plumbing.Hash{} // tmdb and imdb path under data -> its blob
	)
	var mu sync.Mutex // guards the discs the workers below parse

	// Parsing the disc documents is the bulk of the work, so it happens
	// on every core. Reading them stays on this goroutine: go-git's
	// object store is not safe to read from several at once.
	type job struct {
		key  string
		rel  string
		name string
		blob plumbing.Hash
		data []byte
	}
	jobs := make(chan job, 2*runtime.NumCPU())
	var wg sync.WaitGroup
	var parseErr error
	for i := 0; i < runtime.NumCPU(); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				d := &disc{name: j.name, blob: j.blob}
				if err := json.Unmarshal(j.data, &d.meta); err != nil {
					mu.Lock()
					if parseErr == nil {
						parseErr = fmt.Errorf("discdb: %s/%s.json: %w", j.rel, j.name, err)
					}
					mu.Unlock()
					continue
				}
				mu.Lock()
				ds.discs[j.key] = d
				mu.Unlock()
			}
		}()
	}

	files := tree.Files()
	err = files.ForEach(func(f *object.File) error {
		rest, ok := strings.CutPrefix(f.Name, "data/")
		if !ok {
			return nil
		}
		parts := strings.Split(rest, "/")
		if kind := parts[0]; kind != "movie" && kind != "series" {
			return nil // box sets carry no discs of their own
		}
		read := func() ([]byte, error) {
			r, err := f.Blob.Reader()
			if err != nil {
				return nil, err
			}
			defer r.Close()
			return io.ReadAll(r)
		}

		switch len(parts) {
		case 3: // data/<kind>/<title>/<file>
			dir := path.Join(parts[0], parts[1])
			switch parts[2] {
			case "metadata.json":
				data, err := read()
				if err != nil {
					return err
				}
				metas[dir] = data
			case "tmdb.json", "imdb.json":
				blobs[path.Join(dir, parts[2])] = f.Blob.Hash
			case "cover.jpg":
				flags[path.Join(dir, parts[2])] = true
			}
		case 4: // data/<kind>/<title>/<release>/<file>
			dir := path.Join(parts[0], parts[1], parts[2])
			name := parts[3]
			switch {
			case name == "release.json":
				data, err := read()
				if err != nil {
					return err
				}
				rels[dir] = data
			case name == "front.jpg", name == "back.jpg":
				flags[path.Join(dir, name)] = true
			case strings.HasPrefix(name, "disc") && strings.HasSuffix(name, ".json"):
				data, err := read()
				if err != nil {
					return err
				}
				base := strings.TrimSuffix(name, ".json")
				jobs <- job{path.Join(dir, base), dir, base, f.Blob.Hash, data}
			case strings.HasPrefix(name, "disc") && strings.HasSuffix(name, ".ref"):
				data, err := read()
				if err != nil {
					return err
				}
				var ref srcRef
				if err := json.Unmarshal(data, &ref); err != nil {
					return fmt.Errorf("discdb: %s/%s: %w", dir, name, err)
				}
				base := strings.TrimSuffix(name, ".ref")
				ds.refs[path.Join(dir, base)] = ref
			}
		}
		return nil
	})
	close(jobs)
	wg.Wait()
	if err != nil {
		return nil, err
	}
	if parseErr != nil {
		return nil, parseErr
	}

	// Now that everything has been seen, tie the discs to their releases
	// and the releases to their titles.
	slugs := map[string]string{}
	for dir, data := range metas {
		t := &title{
			dir:   dir,
			kind:  path.Dir(dir),
			raw:   compact(data),
			tmdb:  blobs[path.Join(dir, "tmdb.json")],
			imdb:  blobs[path.Join(dir, "imdb.json")],
			cover: flags[path.Join(dir, "cover.jpg")],
		}
		if err := json.Unmarshal(data, &t.meta); err != nil {
			return nil, fmt.Errorf("discdb: %s/metadata.json: %w", dir, err)
		}
		if t.meta.Slug == "" {
			return nil, fmt.Errorf("discdb: %s/metadata.json: no slug", dir)
		}
		// The slug names a directory of its own under titles, so two
		// titles claiming one would quietly overwrite each other.
		if other, ok := slugs[t.meta.Slug]; ok {
			return nil, fmt.Errorf("discdb: %s and %s share the slug %q", other, dir, t.meta.Slug)
		}
		slugs[t.meta.Slug] = dir
		ds.titles[dir] = t
	}
	for dir, data := range rels {
		t := ds.titles[path.Dir(dir)]
		if t == nil {
			continue // a release whose title has no metadata is unusable
		}
		r := &release{
			dir:   dir,
			title: t,
			raw:   compact(data),
			front: flags[path.Join(dir, "front.jpg")],
			back:  flags[path.Join(dir, "back.jpg")],
		}
		if err := json.Unmarshal(data, &r.meta); err != nil {
			return nil, fmt.Errorf("discdb: %s/release.json: %w", dir, err)
		}
		ds.releases[dir] = r
	}
	for key, d := range ds.discs {
		if r := ds.releases[path.Dir(key)]; r != nil {
			d.release = r
			continue
		}
		delete(ds.discs, key)
	}
	return ds, nil
}

// index files every disc under each hash it is known by, and orders the
// releases holding it by collection so that a disc's place in the list
// is stable from one build to the next.
//
// A disc is reachable through both of the hashes it carries: the content
// hash, which a player can work out from the disc in its drive, and the
// global disc id MakeMKV reports. No value in the data set is used as
// both, so one flat set of directories holds the two keyspaces, and a
// hash that ever were both would say so in its group.
func (ds *dataset) index() map[string]*group {
	index := map[string]*group{}
	add := func(h, kind string, e *entry) {
		if h == "" {
			return
		}
		g := index[h]
		if g == nil {
			g = &group{}
			index[h] = g
		}
		switch kind {
		case kindContent:
			g.content = true
		case kindGlobal:
			g.global = true
		}
		g.entries = append(g.entries, e)
	}
	for _, e := range ds.entries() {
		add(e.disc.contentHash(), kindContent, e)
		add(e.disc.globalDiscID(), kindGlobal, e)
	}
	for _, g := range index {
		sort.Slice(g.entries, func(i, j int) bool { return less(g.entries[i], g.entries[j]) })
	}
	return index
}

// entries pairs every disc with each release that holds it: its own, and
// any that reach it through a .ref.
func (ds *dataset) entries() []*entry {
	out := make([]*entry, 0, len(ds.discs)+len(ds.refs))
	for _, d := range ds.discs {
		out = append(out, &entry{disc: d, rel: d.release})
	}
	// A .ref is a release saying it holds a disc described elsewhere.
	for key, ref := range ds.refs {
		rel := ds.releases[path.Dir(key)]
		d := ds.discs[path.Join(ref.ReleasePath, ref.Disc)]
		if rel == nil || d == nil {
			continue // a reference upstream has not filled in yet
		}
		out = append(out, &entry{disc: d, rel: rel, ref: true})
	}
	return out
}

// group is everything filed under one hash: the releases that hold the
// disc, and which of the two hashes led here. A disc carries both, and
// on a DVD both are thirty two hex digits, so the shape of a hash says
// nothing about which kind it is.
type group struct {
	entries []*entry
	content bool
	global  bool
}

// kinds names the hashes this group answers to, in a fixed order.
func (g *group) kinds() []string {
	var out []string
	if g.content {
		out = append(out, kindContent)
	}
	if g.global {
		out = append(out, kindGlobal)
	}
	return out
}

// less orders two releases of the same disc by collection: the path the
// release sits at, compared without regard to case so that the order
// does not depend on the platform's idea of alphabetical.
func less(a, b *entry) bool {
	x, y := a.rel.dir, b.rel.dir
	if lx, ly := strings.ToLower(x), strings.ToLower(y); lx != ly {
		return lx < ly
	}
	if x != y {
		return x < y
	}
	return a.disc.name < b.disc.name
}

// compact strips the whitespace from a JSON document, leaving the rest
// of it, key order included, exactly as upstream wrote it.
func compact(data []byte) json.RawMessage {
	var buf bytes.Buffer
	if err := json.Compact(&buf, data); err != nil {
		return json.RawMessage(data)
	}
	return json.RawMessage(buf.Bytes())
}

// summarize describes a disc closely enough to name it and its feature
// without fetching disc.json: the title the disc marks as the film, or
// failing that the longest. Naming any other title on the disc means
// reading disc.json, which is where they all are.
func summarize(d *disc) discSummary {
	s := discSummary{
		File:         d.name,
		Index:        d.meta.Index,
		Slug:         d.meta.Slug,
		Name:         d.meta.Name,
		Format:       d.meta.Format,
		ContentHash:  d.contentHash(),
		GlobalDiscId: d.globalDiscID(),
		Titles:       len(d.meta.Titles),
	}

	var best *srcTitle
	for i := range d.meta.Titles {
		if t := &d.meta.Titles[i]; best == nil || betterFeature(t, best) {
			best = t
		}
	}

	if best != nil {
		s.Feature = &feature{
			Index:      best.Index,
			Title:      itemTitle(best),
			Type:       best.Item.Type,
			SourceFile: best.SourceFile,
			SegmentMap: best.SegmentMap,
			Duration:   best.Duration,
			Seconds:    durationSeconds(best.Duration),
			Size:       best.Size,
			Chapters:   len(best.Item.Chapters),
		}
		if n, ok := dvdTitleNumber(d.meta.Format, best.SourceFile); ok {
			s.Feature.TitleNumber = n
		}
	}
	return s
}

// betterFeature reports whether t makes a better feature than best: the
// title the disc marks as the film wins, and between two of a kind the
// longer one does.
func betterFeature(t, best *srcTitle) bool {
	if main := t.Item.Type == "MainMovie"; main != (best.Item.Type == "MainMovie") {
		return main
	}
	return t.Size > best.Size
}
