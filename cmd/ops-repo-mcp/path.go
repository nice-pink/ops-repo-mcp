package main

import (
	"errors"
	"path/filepath"
)

// resolveAncestor walks p upward via filepath.Dir until it finds an existing
// directory, runs filepath.EvalSymlinks on that ancestor, then re-attaches the
// (already char-allowlisted) unresolved tail. This handles the first-deploy
// case where the manifest file and its parent directory do not yet exist.
//
// Termination: if filepath.Dir(p) == p (filesystem root), we return an error
// rather than looping forever.
func resolveAncestor(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}

	// Walk upward until we find an existing directory.
	tail := ""
	cur := abs
	for {
		fi, statErr := filepath.EvalSymlinks(cur)
		if statErr == nil {
			// cur exists; reconstruct the full path.
			return filepath.Join(fi, tail), nil
		}

		parent := filepath.Dir(cur)
		if parent == cur {
			// Reached filesystem root without finding an existing ancestor.
			return "", errors.New("resolveAncestor: filesystem root reached without finding an existing directory")
		}

		// Prepend the non-existing segment to tail.
		base := filepath.Base(cur)
		if tail == "" {
			tail = base
		} else {
			tail = filepath.Join(base, tail)
		}
		cur = parent
	}
}

// pathEscape checks whether target is rooted under root (both already
// canonicalised). Returns true if target escapes root (unsafe), false if safe.
func pathEscape(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return true
	}
	// filepath.Rel returns a path starting with ".." if target is outside root.
	if len(rel) >= 2 && rel[:2] == ".." {
		return true
	}
	return false
}
