// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2026 The Media Authors

//go:build mage

// Command magefiles holds the build targets for the dvd player. The
// discdb targets mirror TheDiscDb's data repository into docs/discdb, a
// static tree keyed by disc hash that the player looks discs up in; the
// work itself is in the internal/discdb package.
package main

import (
	"fmt"
	"os"

	"github.com/magefile/mage/mg"

	"dvdplayer.app/dvdplayer/internal/discdb"
)

// Discdb builds the disc lookup tree under docs/discdb.
type Discdb mg.Namespace

// Fetch clones TheDiscDb's data repository into the cache directory, or
// brings an existing clone up to date. The clone is shallow and left
// unchecked out: the generator reads the files straight out of the git
// object store, so the 2 GB of artwork in the tree never lands on disk.
func (Discdb) Fetch() error {
	return discdb.Fetch(options())
}

// Build regenerates docs/discdb, fetching the data repository first.
func (Discdb) Build() error {
	mg.Deps(Discdb.Fetch)
	return discdb.Generate(options())
}

// Clean removes the generated tree. The git cache is left alone; use
// DISCDB_CACHE or delete it by hand to drop that too.
func (Discdb) Clean() error {
	out := options().Out
	fmt.Printf("discdb: removing %s\n", out)
	return os.RemoveAll(out)
}

// options reads the settings out of the environment, which is how the
// workflow points a build somewhere other than the defaults.
func options() discdb.Options {
	return discdb.Options{
		Repo:   env("DISCDB_REPO", discdb.DefaultRepo),
		Branch: env("DISCDB_BRANCH", discdb.DefaultBranch),
		Cache:  env("DISCDB_CACHE", discdb.DefaultCache),
		Out:    env("DISCDB_OUT", discdb.DefaultOut),
		Pretty: os.Getenv("DISCDB_PRETTY") != "",
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
