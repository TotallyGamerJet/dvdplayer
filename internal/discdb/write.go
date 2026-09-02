// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2026 The Media Authors

package discdb

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// writer holds where the tree is going and what it was built from.
type writer struct {
	opts   Options
	commit string
}

// path locates a file in the generated tree.
func (w *writer) path(elem ...string) string {
	return filepath.Join(append([]string{w.opts.Out}, elem...)...)
}

// upstream is a permanent URL for a file in the data repository, pinned
// to the commit this tree was built from. Artwork is linked rather than
// copied: it is two gigabytes and it never changes.
func (w *writer) upstream(p string) string {
	owner, repo, ok := githubRepo(w.opts.Repo)
	if !ok {
		return ""
	}
	segs := strings.Split(path.Join("data", p), "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return "https://raw.githubusercontent.com/" + owner + "/" + repo + "/" +
		w.commit + "/" + strings.Join(segs, "/")
}

// githubRepo pulls the owner and repository out of a GitHub clone URL.
func githubRepo(repoURL string) (owner, repo string, ok bool) {
	u, err := url.Parse(repoURL)
	if err != nil || !strings.HasSuffix(u.Hostname(), "github.com") {
		return "", "", false
	}
	parts := strings.Split(strings.Trim(strings.TrimSuffix(u.Path, ".git"), "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// titles writes the documents shared by every disc of a film or series.
// They live under titles/<slug>/ rather than beside each disc because a
// tmdb.json runs to fifty kilobytes and a popular title has a dozen
// discs; info.json links to them.
func (w *writer) titles(repo *git.Repository, ds *dataset) error {
	for _, t := range ds.titles {
		dir := w.path("titles", t.meta.Slug)
		if err := w.doc(filepath.Join(dir, "metadata.json"), t.raw); err != nil {
			return err
		}
		for name, blob := range map[string]plumbing.Hash{"tmdb.json": t.tmdb, "imdb.json": t.imdb} {
			if blob.IsZero() {
				continue
			}
			data, err := readBlob(repo, blob)
			if err != nil {
				return fmt.Errorf("discdb: %s/%s: %w", t.dir, name, err)
			}
			if err := w.doc(filepath.Join(dir, name), compact(data)); err != nil {
				return err
			}
		}
	}
	return nil
}

// hashes writes the flat list of every hash in the tree, so a reader can
// hold the whole keyspace and know a lookup will miss without asking.
func (w *writer) hashes(index map[string]*group) error {
	all := make([]string, 0, len(index))
	for h := range index {
		all = append(all, h)
	}
	sort.Strings(all)
	var buf bytes.Buffer
	for _, h := range all {
		buf.WriteString(h)
		buf.WriteByte('\n')
	}
	return writeFile(w.path("hashes.txt"), buf.Bytes())
}

// infos writes {HASH}/info.json for every hash, and reports where each
// disc document has to be copied to.
//
// A match's number is its place in info.json's matches, and the disc
// document sits at {HASH}/<n>/disc.json. The releases under one hash
// nearly always describe the same disc, though: an anthology can list
// one disc under forty films. Rather than write forty copies of it, the
// first match to want a disc document keeps it and the rest link to
// that one, which is why a reader follows links.disc instead of
// building the path itself.
func (w *writer) infos(index map[string]*group) (map[plumbing.Hash][]string, []discRow, error) {
	dests := map[plumbing.Hash][]string{}
	rows := make([]discRow, 0, len(index))
	for hash, g := range index {
		views := make([]matchView, 0, len(g.entries))
		doc := info{
			Schema:  schemaVersion,
			Hash:    hash,
			Types:   g.kinds(),
			Count:   len(g.entries),
			Matches: make([]match, 0, len(g.entries)),
		}
		held := map[plumbing.Hash]string{}
		for i, e := range g.entries {
			discPath, ok := held[e.disc.blob]
			if !ok {
				discPath = path.Join(discsDir, hash, strconv.Itoa(i), "disc.json")
				held[e.disc.blob] = discPath
				dests[e.disc.blob] = append(dests[e.disc.blob], w.path(discPath))
			}

			t := e.rel.title
			summary := summarize(e.disc)
			lnks := links{
				Disc:   discPath,
				Title:  path.Join("titles", t.meta.Slug, "metadata.json"),
				Tmdb:   w.titleLink(t, t.tmdb, "tmdb.json"),
				Imdb:   w.titleLink(t, t.imdb, "imdb.json"),
				Cover:  w.artwork(t.cover, t.dir, "cover.jpg"),
				Front:  w.artwork(e.rel.front, e.rel.dir, "front.jpg"),
				Back:   w.artwork(e.rel.back, e.rel.dir, "back.jpg"),
				Source: w.upstream(path.Join(e.disc.release.dir, e.disc.name+".json")),
			}
			doc.Matches = append(doc.Matches, match{
				Index:      i,
				Collection: e.rel.dir,
				Kind:       t.kind,
				Ref:        e.ref,
				Disc:       summary,
				Title:      t.raw,
				Release:    e.rel.raw,
				Links:      lnks,
			})
			views = append(views, matchView{
				Index:      i,
				Collection: e.rel.dir,
				Kind:       t.kind,
				Ref:        e.ref,
				Title:      t.meta,
				Release:    e.rel.meta,
				Disc:       summary,
				Links:      lnks,
			})
		}
		if err := w.json(w.path(discsDir, hash, "info.json"), doc); err != nil {
			return nil, nil, err
		}

		kinds := g.kinds()
		if err := w.render(path.Join(discsDir, hash, "index.html"), "disc", discView{
			page:    page{Title: hash, Base: "../../"},
			Hash:    hash,
			Kinds:   kindNames(kinds, "content hash", "global disc id"),
			Matches: views,
		}); err != nil {
			return nil, nil, err
		}
		rows = append(rows, discRow{
			Hash:   hash,
			Kinds:  kindNames(kinds, "content", "global"),
			Name:   views[0].Disc.Name,
			Format: views[0].Disc.Format,
			Title:  views[0].Title.FullTitle,
			More:   len(views) - 1,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Hash < rows[j].Hash })
	return dests, rows, nil
}

// kindNames spells the hash kinds the way a person reads them, at the
// length the page has room for.
func kindNames(kinds []string, content, global string) string {
	out := make([]string, 0, len(kinds))
	for _, k := range kinds {
		switch k {
		case kindContent:
			out = append(out, content)
		case kindGlobal:
			out = append(out, global)
		}
	}
	return strings.Join(out, " · ")
}

// render writes one of the browsable pages.
func (w *writer) render(p, name string, data any) error {
	var buf bytes.Buffer
	if err := pages.ExecuteTemplate(&buf, name, data); err != nil {
		return fmt.Errorf("discdb: rendering %s: %w", p, err)
	}
	return writeFile(w.path(p), buf.Bytes())
}

// browse writes the pages that make the tree walkable in a browser: the
// stylesheet they share, the front page, the list of hashes, and a page
// for every film and series.
func (w *writer) browse(ds *dataset, m manifest, rows []discRow) error {
	if err := writeFile(w.path("style.css"), []byte(styleCSS)); err != nil {
		return err
	}
	if err := w.render("index.html", "root", rootView{
		page:     page{Title: "discdb"},
		Manifest: m,
		Repo:     w.opts.Repo,
	}); err != nil {
		return err
	}
	if err := w.render(path.Join(discsDir, "index.html"), "discs", discsView{
		page: page{Title: "Discs", Base: "../"},
		Rows: rows,
	}); err != nil {
		return err
	}
	return w.titlePages(ds)
}

// titlePages writes titles/index.html and a page for each title, listing
// the releases it has and the discs in them.
func (w *writer) titlePages(ds *dataset) error {
	byTitle := map[*title][]*entry{}
	for _, e := range ds.entries() {
		byTitle[e.rel.title] = append(byTitle[e.rel.title], e)
	}

	rows := make([]titleRow, 0, len(ds.titles))
	for _, t := range ds.titles {
		entries := byTitle[t]
		rows = append(rows, titleRow{
			Slug:  t.meta.Slug,
			Title: t.meta.FullTitle,
			Year:  t.meta.Year,
			Kind:  t.kind,
			Discs: len(entries),
		})
		if err := w.titlePage(t, entries); err != nil {
			return err
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := strings.ToLower(rows[i].Title), strings.ToLower(rows[j].Title)
		if a != b {
			return a < b
		}
		return rows[i].Slug < rows[j].Slug
	})
	return w.render(path.Join("titles", "index.html"), "titles", titlesView{
		page: page{Title: "Films and series", Base: "../"},
		Rows: rows,
	})
}

// titlePage writes one film or series, its releases and their discs.
func (w *writer) titlePage(t *title, entries []*entry) error {
	sort.Slice(entries, func(i, j int) bool {
		if a, b := entries[i].rel.dir, entries[j].rel.dir; a != b {
			return a < b
		}
		return entries[i].disc.name < entries[j].disc.name
	})

	var groups []releaseGroup
	for _, e := range entries {
		// A disc is linked by the hash it is filed under, preferring the
		// one a player can work out for itself.
		hash := e.disc.contentHash()
		if hash == "" {
			hash = e.disc.globalDiscID()
		}
		if hash == "" {
			continue // a disc with no hash cannot be looked up
		}
		if len(groups) == 0 || groups[len(groups)-1].Path != e.rel.dir {
			groups = append(groups, releaseGroup{Release: e.rel.meta, Path: e.rel.dir})
		}
		g := &groups[len(groups)-1]
		g.Discs = append(g.Discs, releaseDisc{
			Hash:   hash,
			Name:   e.disc.meta.Name,
			Format: e.disc.meta.Format,
			Index:  e.disc.meta.Index,
			Ref:    e.ref,
		})
	}

	return w.render(path.Join("titles", t.meta.Slug, "index.html"), "title", titleView{
		page:   page{Title: t.meta.FullTitle, Base: "../../"},
		Meta:   t.meta,
		Kind:   t.kind,
		Tmdb:   !t.tmdb.IsZero(),
		Imdb:   !t.imdb.IsZero(),
		Cover:  w.artwork(t.cover, t.dir, "cover.jpg"),
		Groups: groups,
	})
}

func (w *writer) titleLink(t *title, blob plumbing.Hash, name string) string {
	if blob.IsZero() {
		return ""
	}
	return path.Join("titles", t.meta.Slug, name)
}

func (w *writer) artwork(present bool, dir, name string) string {
	if !present {
		return ""
	}
	return w.upstream(path.Join(dir, name))
}

// discs copies each disc document to every place the index put it. The
// blobs are read one at a time, because go-git's object store is not
// safe to read from several goroutines, and squeezed and written on all
// the cores there are.
func (w *writer) discs(repo *git.Repository, dests map[plumbing.Hash][]string) error {
	type job struct {
		data  []byte
		paths []string
	}
	jobs := make(chan job, 2*runtime.NumCPU())

	var (
		mu       sync.Mutex
		writeErr error
		wg       sync.WaitGroup
	)
	for i := 0; i < runtime.NumCPU(); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				data := []byte(compact(j.data))
				if w.opts.Pretty {
					data = j.data
				}
				for _, p := range j.paths {
					if err := writeFile(p, data); err != nil {
						mu.Lock()
						if writeErr == nil {
							writeErr = err
						}
						mu.Unlock()
						break
					}
				}
			}
		}()
	}

	var readErr error
	written, last := 0, time.Now()
	for blob, paths := range dests {
		data, err := readBlob(repo, blob)
		if err != nil {
			readErr = fmt.Errorf("discdb: reading disc %s: %w", blob, err)
			break
		}
		jobs <- job{data, paths}
		if written += len(paths); time.Since(last) > 2*time.Second {
			fmt.Printf("discdb: %d disc documents written\n", written)
			last = time.Now()
		}
	}
	close(jobs)
	wg.Wait()

	if readErr != nil {
		return readErr
	}
	return writeErr
}

// newManifest builds index.json, which says what the tree was built from
// and where a reader finds everything in it.
func newManifest(w *writer, commit *object.Commit, ds *dataset, index map[string]*group) manifest {
	m := manifest{
		Schema:    schemaVersion,
		Generated: timestamp(time.Now()),
		Source: manifestSource{
			Repo:   w.opts.Repo,
			Branch: w.opts.Branch,
			Commit: w.commit,
			Date:   timestamp(commit.Committer.When),
		},
		Layout: manifestLayout{
			Info:   "discs/{hash}/info.json",
			Disc:   "discs/{hash}/{match}/disc.json",
			Title:  "titles/{slug}/metadata.json",
			Hashes: "hashes.txt",
			Note: "A hash is upper case hex: the 32 digit MD5 content hash, " +
				"or the 40 digit MakeMKV global disc id. Every path in this " +
				"tree is relative to the directory index.json sits in.",
		},
	}
	for _, g := range index {
		m.Counts.Matches += len(g.entries)
		if g.content {
			m.Counts.ContentHashes++
		}
		if g.global {
			m.Counts.GlobalDiscIds++
		}
	}
	m.Counts.Hashes = len(index)
	m.Counts.Discs = len(ds.discs)
	m.Counts.Releases = len(ds.releases)
	m.Counts.Titles = len(ds.titles)
	return m
}

// readme leaves a short account of the layout in the tree itself, so
// that someone who lands on the published site is not left guessing.
// Backticks are written as @ and swapped in at the end, which is what it
// costs to keep the text below readable as a raw string.
func (w *writer) readme(m manifest) error {
	const text = `# discdb

A disc lookup built from [TheDiscDb](%[1]s) at commit
[%[2]s](%[1]s/tree/%[3]s), dated %[4]s. Generated by
@go tool mage discdb:build@; do not edit it by hand.

Every path below is relative to this directory.

	index.json                    what the tree was built from, and how much of each thing it holds
	hashes.txt                    every hash in the tree, one to a line, sorted
	discs/<HASH>/info.json        the disc that hashes to HASH
	discs/<HASH>/<n>/disc.json    that disc as TheDiscDb describes it
	titles/<slug>/                metadata.json, tmdb.json and imdb.json for one film or series
	index.html                    in each of those directories, the same thing to read in a browser

A hash is upper case hex. Each disc is filed under both of the hashes
TheDiscDb knows it by:

  - its content hash, the MD5 over the sizes of the files in VIDEO_TS or
    BDMV/STREAM, which a player can work out for itself from the disc in
    its drive;
  - its global disc id, as MakeMKV reports it.

Both are 32 hex digits on a DVD, so the length of a hash does not say
which kind it is. The @types@ field of info.json does.

Every directory also holds an index.html, so the published site can be
browsed: the front page links to a list of all the hashes and a list of
all the films and series, both of which filter as you type.

## Looking up a disc

Fetch @discs/<HASH>/info.json@. A 404 means TheDiscDb does not have the
disc.

The file lists every release that holds the disc, ordered by collection,
and carries the film or series metadata and the release metadata inline,
so that one fetch is usually the whole answer. The bulky documents are
linked instead: @links.disc@ for the full title, track and chapter
listing, @links.tmdb@ and @links.imdb@ for the external metadata, and
@links.cover@, @links.front@ and @links.back@ for the artwork, which
stays upstream.

Follow the paths under @links@ rather than building them. A disc that
forty films list is stored once, and all forty matches link to that copy.

## Finding the title that is playing

A player knows the DVD title number its navigator is on, 1 to 99. On a
DVD, @matches[].disc.titleList@ maps that number onto a name:

	for _, t := range match.Disc.TitleList {
		if t.TitleNumber == playing {
			// t.Title, t.Type, t.Duration
		}
	}

@titleList@ is written for DVDs only, and each entry lines up with the
entry at the same @index@ in disc.json. @disc.feature@ carries the same
fields for the main title, plus @titleNumber@.

On a Blu-ray @titleList@ is absent: those discs run to hundreds of
titles, and carrying them would cost thirty megabytes across the tree
and quintuple the size of a busy info.json, for a mapping that is not a
title number anyway. Fetch @links.disc@ and read @Titles@ there.

### What SourceFile holds

@SourceFile@ is spelled differently by format, and the two never mix:

	DVD        the title number in decimal, sometimes zero padded: "1", "01", "27", "99"
	Blu-ray    a file on the disc: "00020.mpls", "00131.m2ts", occasionally "00002.mpls(1)"

Across the whole data set every DVD title is a decimal in 1 to 99 and no
Blu-ray title is, so a case folded @Format == "DVD"@ tells you which you
have. Fold the case: the data spells it @Blu-Ray@, @Blu-ray@, @blu-ray@,
@UHD@ and @DVD@. Testing for a @.mpls@ suffix is not enough, because
@.m2ts@ is the commoner of the two.

The @titleNumber@ field exists so that none of this has to be inferred.
It is written only where the value genuinely is a DVD title number.

### Chapters do not line up

**@Item.Chapters[].Index@ is a position in the list, not a DVD chapter
number. Do not index it against the chapter your navigator reports.**

MakeMKV takes a title as a run of segments, which need not be the whole
of the DVD's program chain, and numbers the chapters of what it took
from 1. Where the two diverge it is visible in the data: a chapter it
could not name is labelled @Chapter N@ with N the number on the source
disc, and in 154 of the 202 such labels N is not the entry's Index.

The disc that shows it plainest is A Christmas Carol (2009),
@E3F840DD2D5B8F7535750EA81CB48D49@: the feature has 17 chapters, of
which the last is named @Chapter 18@, and a navigator reports 18
chapters for its DVD title. Its @SegmentMap@ is @7-23,24@, the only
title on that disc not starting at segment 1, and the only one whose
running time disagrees with the disc, by nine seconds.

So @feature.chapters@ and @titleList[].chapters@ are honest counts of
what disc.json holds, not of what the disc has. Chapter names are also
scarce: 2,355 of 251,685 titles have any at all, 5%% of DVD titles.

## Artwork

@links.cover@, @links.front@ and @links.back@ are absolute URLs into
TheDiscDb's repository on raw.githubusercontent.com, pinned to the
commit above, so they are immutable and safe to cache forever whatever
the five minute max-age on them says. They are served as @image/jpeg@
with @access-control-allow-origin: *@, so a browser or a wasm player can
fetch them directly.

	cover   the poster for the film or series. Present on every match.
	        A 2:3 portrait, 500x750 in nearly every case, occasionally 800x1200.
	front   the front of the packaging. Present on 99%% of matches, and of
	        no fixed size or aspect: seen from 750x1025 to 2000x3000.
	back    the back of the packaging. Present on 41%% of matches, same caveat.

Hand @cover@ to anything that wants a poster. No dimensions are
published, so read them from the image; nothing but the aspect of
@cover@ can be assumed. raw.githubusercontent.com is not a CDN and does
rate limit, so a player should fetch each image once and keep it rather
than lean on the cache header.

## What is copied and what is derived

@disc.json@ is TheDiscDb's own document for that disc with the
whitespace taken out and nothing else changed: same fields, same key
order, same escaping. It is equal to the upstream file when both are
parsed, but it is **not** byte for byte the same, being about half the
size. Compare parsed JSON, not bytes.

Everything else in info.json is derived. @title@ and @release@ are
upstream's metadata.json and release.json inlined the same way.
@disc.feature@ and @disc.titleList@ are this generator's work: @title@
there is TheDiscDb's name for the title, or, when it has none, the name
of the file MakeMKV wrote with its extension removed, and empty when
that was only a serial number like @title.mkv@ or @B1_t06-09.mkv@. More
than four titles in five are unnamed upstream, so expect to show a
duration and a type and no name.

## Rebuilding

	go tool mage discdb:build

DISCDB_REPO, DISCDB_BRANCH, DISCDB_CACHE, DISCDB_OUT and DISCDB_PRETTY
override the defaults. The tree is rebuilt from nothing every time.
`
	out := fmt.Sprintf(text, m.Source.Repo, m.Source.Commit[:8], m.Source.Commit, m.Source.Date[:10])
	return writeFile(w.path("README.md"), []byte(strings.ReplaceAll(out, "@", "`")))
}

// json writes a document this generator built.
func (w *writer) json(p string, v any) error {
	var (
		data []byte
		err  error
	)
	if w.opts.Pretty {
		data, err = json.MarshalIndent(v, "", "  ")
	} else {
		data, err = json.Marshal(v)
	}
	if err != nil {
		return err
	}
	return writeFile(p, data)
}

// doc writes a document copied from upstream, squeezed unless the build
// asked for the tree to stay readable.
func (w *writer) doc(p string, raw json.RawMessage) error {
	if !w.opts.Pretty {
		return writeFile(p, raw)
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return writeFile(p, raw)
	}
	return writeFile(p, buf.Bytes())
}

func writeFile(p string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o644)
}

func readBlob(repo *git.Repository, hash plumbing.Hash) ([]byte, error) {
	blob, err := repo.BlobObject(hash)
	if err != nil {
		return nil, err
	}
	r, err := blob.Reader()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}
