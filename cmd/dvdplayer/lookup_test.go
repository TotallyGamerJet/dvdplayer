// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2026 The Media Authors

package main

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"codeberg.org/totallygamerjet/media/discdb"
)

const testHash = "E3F840DD2D5B8F7535750EA81CB48D49"

// apiAnswer is what TheDiscDb's API says about a disc, cut down to what
// the lookup reads.
const apiAnswer = `{"data":{"mediaItems":{"nodes":[{
  "title":"A Christmas Carol","year":2009,"type":"Movie",
  "releases":[{"title":"2010-DVD","discs":[{
    "name":"DVD","format":"DVD","contentHash":"E3F840DD2D5B8F7535750EA81CB48D49",
    "titles":[{"index":1,"sourceFile":"27","duration":"1:35:39",
               "item":{"title":"A Christmas Carol","type":"MainMovie","chapters":[]}}]
  }]}]
}]}}}`

// apiServer answers every lookup with the given body, or with the status
// where it is not 200, and counts what it was asked.
func apiServer(t *testing.T, body string, status int) (*discdb.Client, *int) {
	t.Helper()
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n++
		if status != http.StatusOK {
			http.Error(w, "no", status)
			return
		}
		io.WriteString(w, body) //nolint:errcheck // a test server
	}))
	t.Cleanup(srv.Close)
	return &discdb.Client{Endpoint: srv.URL, HTTPClient: srv.Client()}, &n
}

func ask(t *testing.T, api *discdb.Client, hash string) discDbAnswer {
	t.Helper()
	return askDiscDb(t.Context(), hash, api, slog.New(slog.DiscardHandler))
}

// TestAskDiscDbUsesAPI checks that one request both names the disc and
// brings back what is on it.
func TestAskDiscDbUsesAPI(t *testing.T) {
	api, n := apiServer(t, apiAnswer, http.StatusOK)
	a := ask(t, api, testHash)
	if a.err != nil {
		t.Fatal(a.err)
	}
	m := a.match()
	if m == nil || m.Title.Title != "A Christmas Carol" {
		t.Fatalf("match = %+v, want A Christmas Carol", m)
	}
	if len(m.Disc.Titles) != 1 {
		t.Errorf("listing = %+v, want the one title the API sent", m.Disc.Titles)
	}
	if *n != 1 {
		t.Errorf("made %d requests, want 1", *n)
	}
}

// TestAskDiscDbNotFound checks that a disc the database has not got is
// carried back as the ordinary answer it is.
func TestAskDiscDbNotFound(t *testing.T) {
	api, _ := apiServer(t, `{"data":{"mediaItems":{"nodes":[]}}}`, http.StatusOK)
	a := ask(t, api, "00000000000000000000000000000000")
	if !errors.Is(a.err, discdb.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", a.err)
	}
	if a.match() != nil {
		t.Error("a disc that was not found should have no match")
	}
}

// TestAskDiscDbUnreachable checks that a database that will not answer
// is reported as a failure rather than as a disc it does not have: the
// player says so, and does not name the disc wrongly.
func TestAskDiscDbUnreachable(t *testing.T) {
	api, n := apiServer(t, "", http.StatusBadGateway)
	a := ask(t, api, testHash)
	if a.err == nil {
		t.Fatal("a database that will not answer should be reported")
	}
	if errors.Is(a.err, discdb.ErrNotFound) {
		t.Errorf("err = %v, want a failure rather than ErrNotFound", a.err)
	}
	if a.match() != nil {
		t.Error("nothing should be named when the lookup failed")
	}
	if *n != 1 {
		t.Errorf("made %d requests, want 1", *n)
	}
}

// TestCoverURL checks what is asked for: the path TheDiscDb gave, under
// its image host, at the size the display wants — and nothing at all for
// a release that has no artwork.
func TestCoverURL(t *testing.T) {
	got := coverURL("Movie/a-christmas-carol-2009/2010-dvd.jpg")
	want := discdb.ImageBaseURL + "Movie/a-christmas-carol-2009/2010-dvd.jpg?width=512"
	if got != want {
		t.Errorf("coverURL = %q, want %q", got, want)
	}
	if got := coverURL(""); got != "" {
		t.Errorf("coverURL(\"\") = %q, want nothing to fetch", got)
	}
}

// TestFetchCover checks that the picture comes back, and that a cover
// which will not come is reported as no cover rather than as a failure:
// the disc is still named without it.
func TestFetchCover(t *testing.T) {
	const body = "\xff\xd8\xff\xe0 pretend JPEG"
	tests := []struct {
		name    string
		status  int
		want    string
		wantURL bool
	}{
		{"a cover", http.StatusOK, body, true},
		{"no such cover", http.StatusNotFound, "", true},
		{"the server is unwell", http.StatusInternalServerError, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var asked string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				asked = r.URL.String()
				if tt.status != http.StatusOK {
					http.Error(w, "no", tt.status)
					return
				}
				io.WriteString(w, body) //nolint:errcheck // a test server
			}))
			defer srv.Close()

			got := fetchCover(t.Context(), srv.URL+"/Movie/x/cover.jpg?width=512",
				slog.New(slog.DiscardHandler))
			if string(got) != tt.want {
				t.Errorf("fetchCover gave %q, want %q", got, tt.want)
			}
			if tt.wantURL && asked != "/Movie/x/cover.jpg?width=512" {
				t.Errorf("asked for %q, want the path and width given", asked)
			}
		})
	}
}

// TestFetchCoverUnreachable checks that a host that is not there costs
// the cover and nothing else.
func TestFetchCoverUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL + "/cover.jpg"
	srv.Close() // nothing is listening now

	if got := fetchCover(t.Context(), url, slog.New(slog.DiscardHandler)); got != nil {
		t.Errorf("fetchCover gave %q, want nil", got)
	}
}
