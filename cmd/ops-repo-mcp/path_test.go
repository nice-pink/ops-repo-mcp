package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveAncestor_Exists(t *testing.T) {
	// A path that definitely exists: the temp directory.
	tmp := t.TempDir()
	got, err := resolveAncestor(tmp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// EvalSymlinks may change case or resolve symlinks; the result should still
	// be a clean absolute path that exists.
	if _, statErr := os.Stat(got); statErr != nil {
		t.Errorf("resolved path %q does not exist: %v", got, statErr)
	}
}

func TestResolveAncestor_NonExistent(t *testing.T) {
	// A path whose parent exists but the leaf does not.
	tmp := t.TempDir()
	leaf := filepath.Join(tmp, "nonexistent", "subdir", "file.yaml")

	got, err := resolveAncestor(leaf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The resolved ancestor (tmp) should exist; the tail is re-attached.
	if got == "" {
		t.Fatal("expected non-empty result")
	}
	// The result should start with the canonical tmp path.
	canonTmp, _ := filepath.EvalSymlinks(tmp)
	rel, relErr := filepath.Rel(canonTmp, got)
	if relErr != nil || len(rel) >= 2 && rel[:2] == ".." {
		t.Errorf("resolved path %q is not under tmp %q", got, canonTmp)
	}
}

func TestResolveAncestor_Symlink(t *testing.T) {
	tmp := t.TempDir()

	// Create a real dir and a symlink pointing to it.
	realDir := filepath.Join(tmp, "real")
	if err := os.MkdirAll(realDir, 0755); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(tmp, "link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Skip("symlinks not supported on this platform")
	}

	leaf := filepath.Join(linkDir, "some-file.yaml")
	got, err := resolveAncestor(leaf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The result should be under real (symlink resolved).
	canonReal, _ := filepath.EvalSymlinks(realDir)
	rel, err := filepath.Rel(canonReal, got)
	if err != nil {
		t.Fatalf("filepath.Rel: %v", err)
	}
	if len(rel) >= 2 && rel[:2] == ".." {
		t.Errorf("resolved path %q escapes real dir %q", got, canonReal)
	}
}

func TestResolveAncestor_RootSentinel(t *testing.T) {
	// A path that starts from root and goes into territory that cannot exist
	// while still eventually hitting the real filesystem root.
	// On a real filesystem, "/" always exists so we can't simulate "root not found".
	// Instead we test that a deeply-nested nonexistent path under tmp resolves
	// without looping forever.
	tmp := t.TempDir()
	deep := filepath.Join(tmp, "a", "b", "c", "d", "e", "f", "file.yaml")
	got, err := resolveAncestor(deep)
	if err != nil {
		t.Fatalf("unexpected error for deep nonexistent path: %v", err)
	}
	if got == "" {
		t.Fatal("expected non-empty result")
	}
}

func TestPathEscape_Inside(t *testing.T) {
	root := "/ops-repo"
	target := "/ops-repo/base/resources/default/myapp/dev/deployment.yaml"
	if pathEscape(root, target) {
		t.Error("expected pathEscape=false for path inside root")
	}
}

func TestPathEscape_Outside(t *testing.T) {
	root := "/ops-repo"
	target := "/etc/passwd"
	if !pathEscape(root, target) {
		t.Error("expected pathEscape=true for path outside root")
	}
}

func TestPathEscape_DotDot(t *testing.T) {
	root := "/ops-repo"
	target := "/ops-repo/../etc/passwd"
	// filepath.Rel should still detect the escape.
	if !pathEscape(root, target) {
		t.Error("expected pathEscape=true for dotdot path")
	}
}
