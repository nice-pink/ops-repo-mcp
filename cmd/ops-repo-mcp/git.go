package main

import (
	"errors"
	"sort"
	"strings"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/storer"
	"github.com/nice-pink/repo-services/pkg/util"
)

// errNoUpstreamRef reports that HEAD has no origin/<branch> tracking ref, so
// there is nothing to compare against and nothing `git push` could target.
var errNoUpstreamRef = errors.New("no upstream tracking ref")

// currentBranch returns the short name of the branch HEAD points at.
// A detached HEAD has no branch name and is reported as an error: the server
// must never write a deploy into a commit nobody can push.
func currentBranch(rh *util.RepoHandle) (string, error) {
	headRef, err := rh.Repo().Head()
	if err != nil {
		return "", err
	}
	if !headRef.Name().IsBranch() {
		return "", errors.New("HEAD is detached (not on a branch)")
	}
	return headRef.Name().Short(), nil
}

// defaultBranchFromOriginHEAD resolves refs/remotes/origin/HEAD to the branch
// name it points at. A normal `git clone` sets this; a repo built with
// `git init` + `git remote add` does not, so an empty return is expected and
// means "cannot infer a default branch", not an error.
func defaultBranchFromOriginHEAD(rh *util.RepoHandle) string {
	ref, err := rh.Repo().Reference(plumbing.NewRemoteHEADReferenceName("origin"), false)
	if err != nil {
		return ""
	}
	target := ref.Target()
	if target == "" || !target.IsRemote() {
		return ""
	}
	// refs/remotes/origin/main -> main
	short := target.Short()
	return strings.TrimPrefix(short, "origin/")
}

// isWorkingTreeDirty returns (dirty, dirtyPaths, error).
// It filters out untracked-only entries (e.g. .DS_Store) because an untracked
// file cannot be overwritten by a fast-forward pull and should not block a deploy.
// Only tracked modifications (staged or unstaged) trigger DIRTY_REPO.
func isWorkingTreeDirty(rh *util.RepoHandle) (bool, []string, error) {
	wt, err := rh.Repo().Worktree()
	if err != nil {
		return false, nil, err
	}
	st, err := wt.Status()
	if err != nil {
		return false, nil, err
	}

	var trackedDirty []string
	for path, s := range st {
		// Skip entries where BOTH staging and worktree are Untracked — these are
		// new files unknown to git that a fast-forward pull cannot clobber.
		if s.Staging == git.Untracked && s.Worktree == git.Untracked {
			continue
		}
		trackedDirty = append(trackedDirty, path)
	}

	if len(trackedDirty) == 0 {
		return false, nil, nil
	}
	sort.Strings(trackedDirty)
	return true, trackedDirty, nil
}

// isAheadOfUpstream returns (ahead, aheadBy, error).
// If the local branch has no upstream tracking ref (e.g. a local-only branch),
// it returns (false, 0, nil) — not an error. The subsequent pull may surface a
// clearer message if remote access fails.
func isAheadOfUpstream(rh *util.RepoHandle) (bool, int, error) {
	repo := rh.Repo()

	headRef, err := repo.Head()
	if err != nil {
		return false, 0, err
	}

	upstreamRefName := plumbing.NewRemoteReferenceName("origin", headRef.Name().Short())
	upstreamRef, err := repo.Reference(upstreamRefName, true)
	if err != nil {
		// No upstream tracking ref. This used to be treated as "not ahead", which
		// silently disabled the ahead-check: commits piled up locally, the bare
		// `git push` in every commitDirective failed, and each call still reported
		// success. A branch the server writes to must be publishable.
		return false, 0, errNoUpstreamRef
	}

	upstreamHash := upstreamRef.Hash()
	count := 0

	// Walk commits from HEAD; stop when we reach the upstream commit.
	iter, err := repo.Log(&git.LogOptions{From: headRef.Hash()})
	if err != nil {
		return false, 0, err
	}
	defer iter.Close()

	iterErr := iter.ForEach(func(c *object.Commit) error {
		if c.Hash == upstreamHash {
			return storer.ErrStop
		}
		count++
		return nil
	})
	if iterErr != nil && iterErr != storer.ErrStop {
		return false, 0, iterErr
	}

	return count > 0, count, nil
}
