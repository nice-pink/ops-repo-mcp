package main

import (
	"regexp"
	"testing"
)

func TestCountNonTagLineChanges_TagOnly(t *testing.T) {
	parent := "image: registry/app:v1\nreplicas: 3\n"
	last := "image: registry/app:v2\nreplicas: 3\n"
	got := countNonTagLineChanges(parent, last, "v1", "v2")
	if got != 0 {
		t.Errorf("expected 0 non-tag line changes, got %d", got)
	}
}

func TestCountNonTagLineChanges_LabelAlsoSubstituted(t *testing.T) {
	// Manifest where the tag also appears in a label — SetTag's plain-string
	// pass updates both. Both lines change but each is explained by the tag
	// substitution, so the count should be zero.
	parent := "labels:\n  version: v1\nimage: registry/app:v1\n"
	last := "labels:\n  version: v2\nimage: registry/app:v2\n"
	got := countNonTagLineChanges(parent, last, "v1", "v2")
	if got != 0 {
		t.Errorf("expected 0 non-tag line changes (label substitution), got %d", got)
	}
}

func TestCountNonTagLineChanges_NonTagLineChanged(t *testing.T) {
	parent := "image: registry/app:v1\nreplicas: 3\n"
	last := "image: registry/app:v2\nreplicas: 5\n"
	got := countNonTagLineChanges(parent, last, "v1", "v2")
	if got != 1 {
		t.Errorf("expected 1 non-tag line change (replicas), got %d", got)
	}
}

func TestCountNonTagLineChanges_LineAdded(t *testing.T) {
	parent := "image: registry/app:v1\nreplicas: 3\n"
	last := "image: registry/app:v2\nreplicas: 3\nresources:\n  requests:\n    cpu: 100m\n"
	got := countNonTagLineChanges(parent, last, "v1", "v2")
	if got < 3 {
		t.Errorf("expected at least 3 non-tag line changes (extra lines), got %d", got)
	}
}

func TestCountNonTagLineChanges_EmptyTagFallback(t *testing.T) {
	// When tag info is missing, the function falls back to a raw line-diff
	// count, which is conservative — anything differing is non-tag.
	parent := "image: registry/app:v1\n"
	last := "image: registry/app:v1\nrequests:\n  cpu: 100m\n"
	got := countNonTagLineChanges(parent, last, "", "")
	if got < 2 {
		t.Errorf("expected at least 2 non-tag line changes with empty tag pair, got %d", got)
	}
}

func TestExtractTag(t *testing.T) {
	pattern := `(.*[/| ]app:)([a-zA-Z0-9_-].*)`
	re := regexp.MustCompile(pattern)

	cases := []struct {
		name string
		body string
		want string
	}{
		{"basic", "image: registry/app:v0.1.2\n", "v0.1.2"},
		{"commit_hash", "image: ghcr.io/org/app:4b6b455\n", "4b6b455"},
		{"no_match", "image: registry/other:v1\n", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extractTag(c.body, re)
			if got != c.want {
				t.Errorf("want %q, got %q", c.want, got)
			}
		})
	}
}

func TestFirstLine(t *testing.T) {
	cases := []struct{ in, want string }{
		{"single", "single"},
		{"first\nsecond\nthird", "first"},
		{"", ""},
	}
	for _, c := range cases {
		if got := firstLine(c.in); got != c.want {
			t.Errorf("firstLine(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
