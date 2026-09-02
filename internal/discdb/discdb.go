// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2026 The Media Authors

// Package discdb mirrors TheDiscDb's data repository into a static tree
// keyed by disc hash, so that a player with a disc in its drive can ask
// a plain web server what the disc is.
//
// The repository is cloned with go-git and read out of the git object
// store rather than a working tree, and [Generate] writes the tree the
// published site serves. See the README it leaves behind for the layout.
package discdb

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// The defaults [Options] fills its blanks in with.
const (
	DefaultRepo   = "https://github.com/TheDiscDb/data"
	DefaultBranch = "main"
	DefaultCache  = ".cache/thediscdb"
	DefaultOut    = "docs/discdb"
)

// Options says which data repository to read and where to put the tree
// built from it. The zero value asks for the defaults above.
type Options struct {
	// Repo is the clone URL of TheDiscDb's data repository.
	Repo string
	// Branch is the branch to read.
	Branch string
	// Cache is the directory the clone is kept in between builds.
	Cache string
	// Out is the directory the tree is written to. It is emptied first.
	Out string
	// Pretty leaves the generated JSON indented, which is worth twice
	// the bytes only when a person is going to read it.
	Pretty bool
}

// withDefaults fills in whatever the caller left blank.
func (o Options) withDefaults() Options {
	if o.Repo == "" {
		o.Repo = DefaultRepo
	}
	if o.Branch == "" {
		o.Branch = DefaultBranch
	}
	if o.Cache == "" {
		o.Cache = DefaultCache
	}
	if o.Out == "" {
		o.Out = DefaultOut
	}
	return o
}

// Fetch brings the cached clone up to date, cloning it if it is not
// there yet. Generate does not do this for itself, so that a build can
// choose when to go to the network.
func Fetch(o Options) error {
	o = o.withDefaults()
	_, _, err := openCache(o, true)
	return err
}

// Generate writes the whole tree under o.Out from the cached clone. It
// clones the repository if the cache is missing, but otherwise takes it
// as it stands: call Fetch first to build against the latest data.
func Generate(o Options) error {
	o = o.withDefaults()
	repo, commit, err := openCache(o, false)
	if err != nil {
		return err
	}
	return generate(o, repo, commit)
}

// openCache returns the cached clone and the commit the generator should
// read. With update set it pulls the branch first. It only ever reclones
// when the cache cannot be read at all, so a fetch that fails leaves a
// working cache alone.
func openCache(o Options, update bool) (*git.Repository, *object.Commit, error) {
	remoteRef := plumbing.NewRemoteReferenceName(git.DefaultRemoteName, o.Branch)

	repo, err := git.PlainOpen(o.Cache)
	switch {
	case errors.Is(err, git.ErrRepositoryNotExists):
		if repo, err = clone(o); err != nil {
			return nil, nil, err
		}
	case err != nil:
		return nil, nil, fmt.Errorf("discdb: opening %s: %w", o.Cache, err)
	case update:
		// A fetch that fails is not worth throwing the cache away for:
		// the build carries on with the commit already in hand, which is
		// what an offline build wants anyway.
		if err := fetch(repo, o.Branch); err != nil {
			fmt.Printf("discdb: fetch failed, building from the cache: %v\n", err)
		}
	}

	commit, err := head(repo, remoteRef)
	if err != nil {
		// The clone is there but unusable, which a shallow clone that
		// could not be advanced ends up as. Replacing it is cheaper than
		// repairing it.
		fmt.Printf("discdb: recloning: %v\n", err)
		if err := os.RemoveAll(o.Cache); err != nil {
			return nil, nil, err
		}
		if repo, err = clone(o); err != nil {
			return nil, nil, err
		}
		if commit, err = head(repo, remoteRef); err != nil {
			return nil, nil, err
		}
	}
	return repo, commit, nil
}

// head resolves the commit at the tip of the tracked branch.
func head(repo *git.Repository, remoteRef plumbing.ReferenceName) (*object.Commit, error) {
	ref, err := repo.Reference(remoteRef, true)
	if err != nil {
		return nil, fmt.Errorf("discdb: resolving %s: %w", remoteRef, err)
	}
	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return nil, fmt.Errorf("discdb: reading commit %s: %w", ref.Hash(), err)
	}
	return commit, nil
}

func clone(o Options) (*git.Repository, error) {
	fmt.Printf("discdb: cloning %s (%s) into %s\n", o.Repo, o.Branch, o.Cache)
	repo, err := git.PlainClone(o.Cache, false, &git.CloneOptions{
		URL:           o.Repo,
		ReferenceName: plumbing.NewBranchReferenceName(o.Branch),
		SingleBranch:  true,
		Depth:         1,
		NoCheckout:    true,
		Tags:          git.NoTags,
		Progress:      os.Stderr,
	})
	if err != nil {
		os.RemoveAll(o.Cache) // a half written clone is worse than none
		return nil, fmt.Errorf("discdb: cloning %s: %w", o.Repo, err)
	}
	return repo, nil
}

func fetch(repo *git.Repository, branch string) error {
	fmt.Printf("discdb: fetching %s\n", branch)
	spec := config.RefSpec(fmt.Sprintf("+refs/heads/%s:refs/remotes/%s/%s",
		branch, git.DefaultRemoteName, branch))
	err := repo.Fetch(&git.FetchOptions{
		RemoteName: git.DefaultRemoteName,
		RefSpecs:   []config.RefSpec{spec},
		Depth:      1,
		Force:      true,
		Tags:       git.NoTags,
		Progress:   os.Stderr,
	})
	if errors.Is(err, git.NoErrAlreadyUpToDate) {
		return nil
	}
	return err
}

// generate writes the whole tree from one commit of the data
// repository. It is rebuilt from nothing every time, so a disc that
// upstream withdrew leaves with it.
func generate(o Options, repo *git.Repository, commit *object.Commit) error {
	start := time.Now()
	ds, err := collect(repo, commit)
	if err != nil {
		return err
	}
	fmt.Printf("discdb: read %d titles, %d releases, %d discs in %s\n",
		len(ds.titles), len(ds.releases), len(ds.discs), time.Since(start).Round(time.Millisecond))

	index := ds.index()
	fmt.Printf("discdb: %d hashes\n", len(index))

	fmt.Printf("discdb: writing %s\n", o.Out)
	if err := os.RemoveAll(o.Out); err != nil {
		return err
	}
	w := &writer{opts: o, commit: commit.Hash.String()}
	if err := w.titles(repo, ds); err != nil {
		return err
	}
	if err := w.hashes(index); err != nil {
		return err
	}
	dests, err := w.infos(index)
	if err != nil {
		return err
	}
	if err := w.discs(repo, dests); err != nil {
		return err
	}
	m := newManifest(w, commit, ds, index)
	if err := w.json(w.path("index.json"), m); err != nil {
		return err
	}
	if err := w.readme(m); err != nil {
		return err
	}
	fmt.Printf("discdb: done in %s\n", time.Since(start).Round(time.Millisecond))
	return nil
}
