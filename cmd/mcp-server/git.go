package main

import (
	"sort"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/storer"
	"github.com/nice-pink/repo-services/pkg/util"
)

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
		// No upstream tracking ref — not an error for our purposes.
		return false, 0, nil
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
