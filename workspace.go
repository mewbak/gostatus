package main

import (
	"fmt"
	"go/build"
	"log"
	"strings"
	"sync"

	"github.com/bradfitz/iter"
	"github.com/shurcooL/vcsstate"
	"golang.org/x/tools/go/vcs"
)

// workspace is a Go workspace environment; each repo has local and remote components.
type workspace struct {
	ImportPaths       chan string // ImportPaths is the input for Go packages to be processed.
	unique            chan *Repo  // Unique repos.
	processedFiltered chan *Repo  // Processed repos, populated with local and remote state, filtered with shouldShow.
	Statuses          chan string // Statuses has results of running presenter on processed repos.
	Errors            chan error  // Errors contains errors that were encountered during processing of repos.

	shouldShow RepoFilter
	presenter  RepoPresenter

	reposMu sync.Mutex
	repos   map[string]*Repo // Map key is the import path corresponding to the root of the repository or Go package.
}

func NewWorkspace(shouldShow RepoFilter, presenter RepoPresenter) *workspace {
	w := &workspace{
		ImportPaths:       make(chan string, 64),
		unique:            make(chan *Repo, 64),
		processedFiltered: make(chan *Repo, 64),
		Statuses:          make(chan string, 64),
		Errors:            make(chan error, 64),

		shouldShow: shouldShow,
		presenter:  presenter,

		repos: make(map[string]*Repo),
	}

	{
		var wg sync.WaitGroup
		for range iter.N(parallelism) {
			wg.Add(1)
			go w.uniqueWorker(&wg)
		}
		go func() {
			wg.Wait()
			close(w.unique)
		}()
	}
	{
		var wg sync.WaitGroup
		for range iter.N(parallelism) {
			wg.Add(1)
			go w.processFilterWorker(&wg)
		}
		go func() {
			wg.Wait()
			close(w.processedFiltered)
		}()
	}
	{
		var wg sync.WaitGroup
		for range iter.N(parallelism) {
			wg.Add(1)
			go w.presenterWorker(&wg)
		}
		go func() {
			wg.Wait()
			close(w.Statuses)
			close(w.Errors)
		}()
	}

	return w
}

// uniqueWorker finds unique repos out of all input Go packages.
func (w *workspace) uniqueWorker(wg *sync.WaitGroup) {
	defer wg.Done()
	for importPath := range w.ImportPaths {
		// Determine repo root.
		// This is potentially somewhat slow.
		bpkg, err := build.Import(importPath, wd, build.FindOnly|build.IgnoreVendor)
		if err != nil {
			w.Errors <- err
			continue
		}
		if bpkg.Goroot {
			// gostatus has no support for printing status of packages in GOROOT, so skip those.
			continue
		}
		vcsCmd, root, err := vcs.FromDir(bpkg.Dir, bpkg.SrcRoot)
		if err != nil {
			// Go package not under VCS.
			var pkg *Repo
			w.reposMu.Lock()
			if _, ok := w.repos[bpkg.ImportPath]; !ok {
				pkg = &Repo{
					Path: bpkg.Dir,
					Root: bpkg.ImportPath,
				}
				w.repos[bpkg.ImportPath] = pkg
			}
			w.reposMu.Unlock()

			// If new package, send off to next stage.
			if pkg != nil {
				w.unique <- pkg
			}
			continue
		}
		vcs, err := vcsstate.NewVCS(vcsCmd)
		if err != nil {
			// Repository not supported by vcsstate.
			var pkg *Repo
			w.reposMu.Lock()
			if _, ok := w.repos[root]; !ok {
				pkg = &Repo{
					Path:     bpkg.Dir,
					Root:     root,
					vcsError: fmt.Errorf("%v not supported by vcsstate: %v", vcsCmd.Name, err),
				}
				w.repos[root] = pkg
			}
			w.reposMu.Unlock()

			// If new package, display an error and send off to next stage.
			if pkg != nil {
				w.unique <- pkg
			}
			continue
		}

		var repo *Repo
		w.reposMu.Lock()
		if _, ok := w.repos[root]; !ok {
			repo = &Repo{
				Path: bpkg.Dir,
				Root: root,
				vcs:  vcs,
			}
			w.repos[root] = repo
		}
		w.reposMu.Unlock()

		// If new repo, send off to next stage.
		if repo != nil {
			w.unique <- repo
		}
	}
}

// processFilterWorker computes repository local and remote state, and filters with shouldShow.
func (w *workspace) processFilterWorker(wg *sync.WaitGroup) {
	defer wg.Done()
	for repo := range w.unique {
		w.computeVCSState(repo)

		if !w.shouldShow(repo) {
			continue
		}

		w.processedFiltered <- repo
	}
}

func (*workspace) computeVCSState(r *Repo) {
	if r.vcs == nil {
		// Go package not under VCS.
		return
	}

	if s, err := r.vcs.Status(r.Path); err == nil {
		r.Local.Status = s
	}
	if b, err := r.vcs.Branch(r.Path); err == nil {
		r.Local.Branch = b
	}
	if s, err := r.vcs.Stash(r.Path); err == nil {
		r.Local.Stash = s
	}
	if remote, err := r.vcs.RemoteURL(r.Path); err == nil {
		r.Local.RemoteURL = remote
	}
	if b, rev, remoteError := r.vcs.RemoteBranchAndRevision(r.Path); remoteError == nil {
		r.Remote.Branch = b
		r.Remote.Revision = rev
	} else if remoteError == vcsstate.ErrNoRemote {
		r.Remote.Branch = r.vcs.NoRemoteDefaultBranch()
	} else if notFoundError, ok := remoteError.(vcsstate.NotFoundError); ok {
		r.Remote.NotFound = notFoundError
		r.Remote.Branch = r.vcs.NoRemoteDefaultBranch()
	} else if remoteError != nil {
		if b, err := r.vcs.CachedRemoteDefaultBranch(); err == nil {
			r.Remote.Branch = b
		} else {
			log.Printf("%v: %v\n", r.Root, remoteError)
			r.Remote.Branch = r.vcs.NoRemoteDefaultBranch() // It's a better fallback than empty string.
		}
	}
	if rev, err := r.vcs.LocalRevision(r.Path, r.Remote.Branch); err == nil {
		r.Local.Revision = rev
	}
	if r.Remote.Revision != "" {
		if c, err := r.vcs.Contains(r.Path, r.Remote.Revision, r.Remote.Branch); err == nil {
			r.Local.ContainsRemoteRevision = c
		}
	}
	if r.Local.Revision != "" {
		if c, err := r.vcs.RemoteContains(r.Path, r.Local.Revision, r.Remote.Branch); err == nil {
			r.Remote.ContainsLocalRevision = c
		} else if strings.Contains(err.Error(), "not implemented") && r.Local.Revision != r.Remote.Revision && r.Remote.Revision != "" {
			// Fall back to using r.Local.ContainsRemoteRevision to deduct information.
			// Assume that if local contains remote revision, then remote doesn't, and vice versa.
			r.Remote.ContainsLocalRevision = !r.Local.ContainsRemoteRevision
		}
	}
	if rr, err := vcs.RepoRootForImportPath(r.Root, false); err == nil {
		r.Remote.RepoURL = rr.Repo
	}
}

// presenterWorker runs presenter on processed and filtered repos.
func (w *workspace) presenterWorker(wg *sync.WaitGroup) {
	defer wg.Done()
	for repo := range w.processedFiltered {
		w.Statuses <- w.presenter(repo)
	}
}
