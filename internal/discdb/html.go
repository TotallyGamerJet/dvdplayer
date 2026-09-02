// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2026 The Media Authors

package discdb

import (
	"fmt"
	"html/template"
	"strings"
)

// The tree is meant to be read by a program, but a static site nobody
// can browse is hard to trust and hard to debug, so every directory in
// it also gets an index.html. They are plain documents over one small
// stylesheet: the only script is the filter box on the two long lists.

// page is what every template needs: what to call the document, and the
// way back to the root of the tree from wherever the document sits.
type page struct {
	Title string
	Base  string // "", "../" or "../../"
}

// rootView is index.html at the root of the tree.
type rootView struct {
	page
	Manifest manifest
	Repo     string
}

// discsView is discs/index.html, every hash in the tree.
type discsView struct {
	page
	Rows []discRow
}

// discRow is one hash in that list. It carries enough of the disc to be
// worth scanning and to filter on.
type discRow struct {
	Hash   string
	Kinds  string
	Name   string
	Format string
	Title  string
	More   int // releases beyond the first
}

// discView is discs/<HASH>/index.html.
type discView struct {
	page
	Hash    string
	Kinds   string
	Matches []matchView
}

// matchView is one release holding the disc.
type matchView struct {
	Index      int
	Collection string
	Kind       string
	Ref        bool
	Title      srcMeta
	Release    srcRelease
	Disc       discSummary
	Links      links
}

// titlesView is titles/index.html, every film and series.
type titlesView struct {
	page
	Rows []titleRow
}

type titleRow struct {
	Slug  string
	Title string
	Year  int
	Kind  string
	Discs int
}

// titleView is titles/<slug>/index.html.
type titleView struct {
	page
	Meta   srcMeta
	Kind   string
	Tmdb   bool
	Imdb   bool
	Cover  string
	Groups []releaseGroup
}

// releaseGroup is one release of a title and the discs in it.
type releaseGroup struct {
	Release srcRelease
	Path    string
	Discs   []releaseDisc
}

type releaseDisc struct {
	Hash   string
	Name   string
	Format string
	Index  int
	Ref    bool
}

// commas puts a thousands separator into a count, which the totals on
// these pages badly need.
func commas(n int) string {
	s := fmt.Sprint(n)
	if n < 0 {
		return s
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// short cuts a commit hash down to the length a person reads.
func short(hash string) string {
	if len(hash) > 8 {
		return hash[:8]
	}
	return hash
}

var pages = template.Must(template.New("pages").Funcs(template.FuncMap{
	"commas": commas,
	"short":  short,
}).Parse(`
{{define "top"}}<!doctype html>
<html lang="en">
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Title}}</title>
<link rel="stylesheet" href="{{.Base}}style.css">
<nav><a href="{{.Base}}index.html">discdb</a> ·
<a href="{{.Base}}discs/index.html">discs</a> ·
<a href="{{.Base}}titles/index.html">titles</a></nav>
{{end}}

{{define "filter"}}
<input id="q" type="search" placeholder="Filter&hellip;" autocomplete="off" autofocus>
<p id="n" class="muted"></p>
<script>
document.addEventListener("DOMContentLoaded", () => {
  const q = document.getElementById("q"), n = document.getElementById("n");
  const rows = [...document.querySelectorAll("tbody tr")];
  for (const r of rows) r.dataset.k = r.textContent.toLowerCase();
  q.addEventListener("input", () => {
    const v = q.value.trim().toLowerCase();
    let shown = 0;
    for (const r of rows) {
      const hit = !v || r.dataset.k.includes(v);
      r.hidden = !hit;
      if (hit) shown++;
    }
    n.textContent = v ? shown.toLocaleString() + " of " + rows.length.toLocaleString() : "";
  });
});
</script>
{{end}}

{{define "root"}}{{template "top" .}}
<h1>discdb</h1>
<p>A disc lookup built from <a href="{{.Repo}}">TheDiscDb</a> at commit
<a href="{{.Repo}}/tree/{{.Manifest.Source.Commit}}"><code>{{short .Manifest.Source.Commit}}</code></a>,
dated {{.Manifest.Source.Date}}. Generated {{.Manifest.Generated}}.</p>

<p>Give it the hash of a disc and it tells you what the disc is. A disc
is filed under both of the hashes TheDiscDb knows it by: its
<b>content hash</b>, the MD5 over the sizes of the files in
<code>VIDEO_TS</code> or <code>BDMV/STREAM</code>, which a player can
work out for itself from the disc in its drive, and its
<b>global disc id</b>, as MakeMKV reports it. Both are 32 hex digits on
a DVD, so the length of a hash does not say which kind it is.</p>

<h2>Browse</h2>
<ul>
<li><a href="discs/index.html">{{commas .Manifest.Counts.Hashes}} hashes</a></li>
<li><a href="titles/index.html">{{commas .Manifest.Counts.Titles}} films and series</a></li>
</ul>

<h2>For programs</h2>
<p>Fetch <code>discs/&lt;HASH&gt;/info.json</code>, upper case hex. A 404
means TheDiscDb does not have the disc. It carries the film or series
and the release metadata inline, so one fetch is usually the whole
answer; follow the paths under <code>links</code> for the rest rather
than building them yourself.</p>
<ul>
<li><a href="index.json">index.json</a> — what this was built from, and how much of each thing it holds</li>
<li><a href="hashes.txt">hashes.txt</a> — every hash, one to a line, sorted</li>
<li><a href="README.md">README.md</a> — the layout in full</li>
</ul>

<h2>Counts</h2>
<table>
<tr><th>hashes<td class="num">{{commas .Manifest.Counts.Hashes}}
<tr><th>content hashes<td class="num">{{commas .Manifest.Counts.ContentHashes}}
<tr><th>global disc ids<td class="num">{{commas .Manifest.Counts.GlobalDiscIds}}
<tr><th>discs<td class="num">{{commas .Manifest.Counts.Discs}}
<tr><th>disc/release pairs<td class="num">{{commas .Manifest.Counts.Matches}}
<tr><th>releases<td class="num">{{commas .Manifest.Counts.Releases}}
<tr><th>films and series<td class="num">{{commas .Manifest.Counts.Titles}}
</table>
{{end}}

{{define "discs"}}{{template "top" .}}
<h1>Discs</h1>
<p class="muted">{{commas (len .Rows)}} hashes, sorted.</p>
{{template "filter" .}}
<div class="scroll">
<table class="rows">
<thead><tr><th>Hash<th>Kind<th>Disc<th>Format<th>Film or series</thead>
<tbody>
{{range .Rows}}<tr><td class="mono nowrap"><a href="{{.Hash}}/index.html">{{.Hash}}</a><td class="muted nowrap">{{.Kinds}}<td>{{.Name}}<td class="muted nowrap">{{.Format}}<td>{{.Title}}{{if .More}} <span class="muted">+{{.More}} more</span>{{end}}
{{end}}</tbody>
</table>
</div>
{{end}}

{{define "disc"}}{{template "top" .}}
<h1 class="mono break">{{.Hash}}</h1>
<p class="muted">{{.Kinds}} · {{len .Matches}} release{{if ne (len .Matches) 1}}s{{end}} ·
<a href="info.json">info.json</a></p>

{{range .Matches}}
<h2>{{.Title.FullTitle}}{{if .Ref}} <span class="muted">(by reference)</span>{{end}}</h2>
<p class="muted">{{.Collection}}</p>
<table>
<tr><th>release<td>{{.Release.Title}}{{if .Release.Year}} ({{.Release.Year}}){{end}}
<tr><th>disc<td>{{.Disc.Name}} — {{.Disc.Format}}, disc {{.Disc.Index}} of the set
{{if .Disc.Feature}}<tr><th>feature<td>{{.Disc.Feature.Title}}{{if .Disc.Feature.Duration}} — {{.Disc.Feature.Duration}}{{end}}{{if .Disc.Feature.Chapters}}, {{.Disc.Feature.Chapters}} chapters{{end}}{{end}}
<tr><th>titles on the disc<td>{{.Disc.Titles}}
{{if .Disc.ContentHash}}<tr><th>content hash<td class="mono break">{{.Disc.ContentHash}}{{end}}
{{if .Disc.GlobalDiscId}}<tr><th>global disc id<td class="mono break">{{.Disc.GlobalDiscId}}{{end}}
</table>
<p><a href="{{$.Base}}{{.Links.Disc}}">disc.json</a> ·
<a href="{{$.Base}}titles/{{.Title.Slug}}/index.html">{{.Title.Title}}</a> ·
<a href="{{$.Base}}{{.Links.Title}}">metadata.json</a>
{{if .Links.Tmdb}} · <a href="{{$.Base}}{{.Links.Tmdb}}">tmdb.json</a>{{end}}
{{if .Links.Imdb}} · <a href="{{$.Base}}{{.Links.Imdb}}">imdb.json</a>{{end}}
{{if .Links.Cover}} · <a href="{{.Links.Cover}}">cover</a>{{end}}
{{if .Links.Front}} · <a href="{{.Links.Front}}">front</a>{{end}}
{{if .Links.Back}} · <a href="{{.Links.Back}}">back</a>{{end}}
 · <a href="{{.Links.Source}}">source</a></p>
{{end}}
{{end}}

{{define "titles"}}{{template "top" .}}
<h1>Films and series</h1>
<p class="muted">{{commas (len .Rows)}} titles, sorted.</p>
{{template "filter" .}}
<div class="scroll">
<table class="rows">
<thead><tr><th>Title<th>Year<th>Kind<th class="num">Discs</thead>
<tbody>
{{range .Rows}}<tr><td><a href="{{.Slug}}/index.html">{{.Title}}</a><td class="muted num">{{if .Year}}{{.Year}}{{end}}<td class="muted">{{.Kind}}<td class="num">{{.Discs}}
{{end}}</tbody>
</table>
</div>
{{end}}

{{define "title"}}{{template "top" .}}
<h1>{{.Meta.FullTitle}}</h1>
{{if .Meta.Tagline}}<p class="muted">{{.Meta.Tagline}}</p>{{end}}
{{if .Meta.Plot}}<p>{{.Meta.Plot}}</p>{{end}}
<table>
<tr><th>kind<td>{{.Kind}}
{{if .Meta.Genres}}<tr><th>genres<td>{{.Meta.Genres}}{{end}}
{{if .Meta.Runtime}}<tr><th>runtime<td>{{.Meta.Runtime}}{{end}}
{{if .Meta.ContentRating}}<tr><th>rated<td>{{.Meta.ContentRating}}{{end}}
{{if .Meta.Directors}}<tr><th>directed by<td>{{.Meta.Directors}}{{end}}
{{if .Meta.Stars}}<tr><th>starring<td>{{.Meta.Stars}}{{end}}
<tr><th>documents<td><a href="metadata.json">metadata.json</a>{{if .Tmdb}} · <a href="tmdb.json">tmdb.json</a>{{end}}{{if .Imdb}} · <a href="imdb.json">imdb.json</a>{{end}}{{if .Cover}} · <a href="{{.Cover}}">cover</a>{{end}}
{{if .Meta.ExternalIds.Tmdb}}<tr><th>tmdb<td><a href="https://www.themoviedb.org/{{if eq .Kind "series"}}tv{{else}}movie{{end}}/{{.Meta.ExternalIds.Tmdb}}">{{.Meta.ExternalIds.Tmdb}}</a>{{end}}
{{if .Meta.ExternalIds.Imdb}}<tr><th>imdb<td><a href="https://www.imdb.com/title/{{.Meta.ExternalIds.Imdb}}/">{{.Meta.ExternalIds.Imdb}}</a>{{end}}
</table>

<h2>Releases</h2>
{{range .Groups}}
<h3>{{.Release.Title}}{{if .Release.Year}} ({{.Release.Year}}){{end}}</h3>
<p class="muted">{{.Path}}</p>
<div class="scroll">
<table>
<thead><tr><th>Disc<th>Name<th>Format<th>Hash</thead>
<tbody>
{{range .Discs}}<tr><td class="num">{{.Index}}<td>{{.Name}}{{if .Ref}} <span class="muted">(by reference)</span>{{end}}<td class="muted nowrap">{{.Format}}<td class="mono nowrap"><a href="{{$.Base}}discs/{{.Hash}}/index.html">{{.Hash}}</a>
{{end}}</tbody>
</table>
</div>
{{end}}
{{end}}
`))

// styleCSS is the whole of the tree's presentation. It leans on the
// system colours so that it follows the reader's light or dark setting
// without a media query.
const styleCSS = `:root { color-scheme: light dark }
body { font: 15px/1.6 system-ui, -apple-system, Segoe UI, sans-serif;
  max-width: 68rem; margin: 2rem auto; padding: 0 1.25rem }
h1 { font-size: 1.5rem; margin: 0 0 .25rem }
h2 { font-size: 1.15rem; margin: 2rem 0 .25rem }
h3 { font-size: 1rem; margin: 1.5rem 0 .25rem }
p, ul { max-width: 46rem }
p { margin: .35rem 0 }
nav { margin-bottom: 1.75rem; font-size: .875rem }
nav a { text-decoration: none }
a { color: LinkText }
ul { margin: .35rem 0; padding-left: 1.25rem }
li { margin: .15rem 0 }
code, .mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: .85em }
.break { overflow-wrap: anywhere }
.muted { color: GrayText }
.num { text-align: right; font-variant-numeric: tabular-nums }
.nowrap { white-space: nowrap }
.scroll { overflow-x: auto; margin: .5rem -1.25rem; padding: 0 1.25rem }
.scroll table { min-width: 54rem }
.rows td { white-space: nowrap; max-width: 26rem; overflow: hidden; text-overflow: ellipsis }
table { border-collapse: collapse; width: 100%; margin: .5rem 0; font-size: .9rem }
th, td { text-align: left; vertical-align: top; padding: .3rem .6rem;
  border-bottom: 1px solid Canvas; box-shadow: inset 0 -1px 0 GrayText }
th { font-weight: 600; white-space: nowrap; color: GrayText }
thead th { position: sticky; top: 0; background: Canvas }
tbody tr:hover { background: color-mix(in srgb, GrayText 12%, transparent) }
input[type=search] { width: 100%; box-sizing: border-box; padding: .5rem .7rem;
  font: inherit; border: 1px solid GrayText; border-radius: .35rem;
  background: Canvas; color: CanvasText }
`
