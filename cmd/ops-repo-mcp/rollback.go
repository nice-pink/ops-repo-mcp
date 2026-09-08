package main

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/storer"
)

// rollbackAnalysis is the result of inspecting the last commit that touched
// the manifest file. previousTag is what the file would revert to; nonTagLines
// is the count of changed lines that aren't explained by a tag substitution.
type rollbackAnalysis struct {
	currentTag       string
	previousTag      string
	lastCommitHash   string
	parentCommitHash string
	lastCommitMsg    string
	nonTagLines      int
}

// analyseRollback walks the file's git history, finds the most recent commit
// that touched it, and compares it against its parent to derive the previous
// tag plus a count of non-tag-related line changes. relFilePath must be
// relative to the repo root with forward-slash separators. imagePatternRaw is
// the ManifestHandler.ImagePattern() string — group(2) must capture the tag.
func analyseRollback(repo *git.Repository, relFilePath, imagePatternRaw string) (*rollbackAnalysis, error) {
	headRef, err := repo.Head()
	if err != nil {
		return nil, fmt.Errorf("repo head: %w", err)
	}

	relCopy := relFilePath
	iter, err := repo.Log(&git.LogOptions{
		From:     headRef.Hash(),
		FileName: &relCopy,
	})
	if err != nil {
		return nil, fmt.Errorf("git log: %w", err)
	}
	defer iter.Close()

	var lastC *object.Commit
	iterErr := iter.ForEach(func(c *object.Commit) error {
		lastC = c
		return storer.ErrStop
	})
	if iterErr != nil && iterErr != storer.ErrStop {
		return nil, fmt.Errorf("git log walk: %w", iterErr)
	}
	if lastC == nil {
		return nil, errors.New("file has no commit history")
	}
	if lastC.NumParents() == 0 {
		return nil, errors.New("manifest was created in the initial commit; no previous version to roll back to")
	}

	parent, err := lastC.Parent(0)
	if err != nil {
		return nil, fmt.Errorf("parent commit: %w", err)
	}

	lastBlob, err := readBlob(lastC, relFilePath)
	if err != nil {
		return nil, fmt.Errorf("read blob at %s: %w", lastC.Hash.String()[:7], err)
	}
	parentBlob, err := readBlob(parent, relFilePath)
	if err != nil {
		return nil, fmt.Errorf("read blob at %s (parent): %w", parent.Hash.String()[:7], err)
	}

	re, err := regexp.Compile(imagePatternRaw)
	if err != nil {
		return nil, fmt.Errorf("compile image pattern: %w", err)
	}
	currentTag := extractTag(lastBlob, re)
	previousTag := extractTag(parentBlob, re)
	if previousTag == "" {
		return nil, errors.New("could not extract previous tag from parent commit's manifest")
	}

	nonTag := countNonTagLineChanges(parentBlob, lastBlob, previousTag, currentTag)

	return &rollbackAnalysis{
		currentTag:       currentTag,
		previousTag:      previousTag,
		lastCommitHash:   lastC.Hash.String(),
		parentCommitHash: parent.Hash.String(),
		lastCommitMsg:    firstLine(lastC.Message),
		nonTagLines:      nonTag,
	}, nil
}

// readBlob fetches the file contents at the given commit. Returns an error if
// the file does not exist in that tree.
func readBlob(c *object.Commit, relFilePath string) (string, error) {
	tree, err := c.Tree()
	if err != nil {
		return "", err
	}
	entry, err := tree.File(relFilePath)
	if err != nil {
		return "", err
	}
	return entry.Contents()
}

// extractTag returns the first capture-group-2 match in content for the given
// pre-compiled image-pattern regex. Empty string if no match.
func extractTag(content string, re *regexp.Regexp) string {
	m := re.FindStringSubmatch(content)
	if len(m) >= 3 {
		return m[2]
	}
	return ""
}

// countNonTagLineChanges returns the number of changed lines between parent
// and last that cannot be explained by a single substitution of prevTag with
// currTag. A differing line counts as "tag-related" if applying that
// substitution to its parent form produces the last form. Length mismatches
// add the absolute line-count delta to the non-tag count.
func countNonTagLineChanges(parentBlob, lastBlob, prevTag, currTag string) int {
	if prevTag == "" || currTag == "" || prevTag == currTag {
		if parentBlob == lastBlob {
			return 0
		}
		return countAllDifferingLines(parentBlob, lastBlob)
	}

	parentLines := strings.Split(parentBlob, "\n")
	lastLines := strings.Split(lastBlob, "\n")

	minLen := min(len(parentLines), len(lastLines))

	nonTag := 0
	for i := range minLen {
		if parentLines[i] == lastLines[i] {
			continue
		}
		if strings.ReplaceAll(parentLines[i], prevTag, currTag) == lastLines[i] {
			continue
		}
		nonTag++
	}

	if d := len(parentLines) - len(lastLines); d != 0 {
		if d < 0 {
			d = -d
		}
		nonTag += d
	}
	return nonTag
}

func countAllDifferingLines(a, b string) int {
	la := strings.Split(a, "\n")
	lb := strings.Split(b, "\n")
	minLen := min(len(la), len(lb))
	n := 0
	for i := range minLen {
		if la[i] != lb[i] {
			n++
		}
	}
	if d := len(la) - len(lb); d != 0 {
		if d < 0 {
			d = -d
		}
		n += d
	}
	return n
}

func firstLine(s string) string {
	first, _, _ := strings.Cut(s, "\n")
	return first
}
