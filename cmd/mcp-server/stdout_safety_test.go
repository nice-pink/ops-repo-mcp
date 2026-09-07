package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nice-pink/repo-services/pkg/runner"
	"github.com/nice-pink/repo-services/pkg/util"
)

// TestRunnerSilentOnStdout is the load-bearing regression guard for stdout purity.
// It redirects os.Stdout to a pipe, calls runner.Deploy and runner.Promote against
// a copy of examples/repo with Push=false and Url="", and asserts the pipe is empty.
// Any output on stdout would corrupt the MCP JSON-RPC channel.
func TestRunnerSilentOnStdout(t *testing.T) {
	// Silence slog during the test so we only capture actual stdout writes.
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})))

	// Find examples/repo relative to the module root.
	repoRoot := findModuleRoot(t)
	examplesRepo := filepath.Join(repoRoot, "examples", "repo")
	if _, err := os.Stat(examplesRepo); err != nil {
		t.Skipf("examples/repo not found at %s: %v", examplesRepo, err)
	}

	// Copy the examples/repo to a temp dir so we don't mutate the source.
	tmpRepo := t.TempDir()
	if err := copyDir(examplesRepo, tmpRepo); err != nil {
		t.Fatalf("failed to copy examples/repo: %v", err)
	}

	// Build flags: Push=false, Url="" — the MCP's invariant.
	flags, gitFlags := util.NewFlagsFromValues(util.FlagValues{
		App:             "test-app",
		Namespace:       "test",
		Env:             "dev",
		Base:            "base/resources",
		Cluster:         "base/resources",
		Image:           "test-app",
		ImageFile:       "deployment.yaml",
		ImageHistoryFile: "",
		PathScheme:      "{base}/{namespace}/{app}/{env}",
		SrcPath:         tmpRepo,
		Push:            false,
		Url:             "",
	})

	t.Run("Deploy", func(t *testing.T) {
		capturedOutput := captureStdout(t, func() {
			// Ignore the error — the manifest may not have the right image pattern for
			// "newimage:test", but what matters is stdout silence.
			_ = runner.Deploy("newimage:test", "", flags, gitFlags)
		})
		if capturedOutput != "" {
			t.Errorf("runner.Deploy wrote to stdout (MCP channel corruption!):\n%s", capturedOutput)
		}
	})

	t.Run("Promote", func(t *testing.T) {
		// Promote from dev -> prod; src tag may not exist but stdout must be silent.
		promoteFlags, promoteGitFlags := util.NewFlagsFromValues(util.FlagValues{
			App:             "test-app",
			Namespace:       "test",
			Env:             "prod",
			Base:            "base/resources",
			Cluster:         "base/resources",
			Image:           "test-app",
			ImageFile:       "deployment.yaml",
			ImageHistoryFile: "",
			PathScheme:      "{base}/{namespace}/{app}/{env}",
			SrcPath:         tmpRepo,
			Push:            false,
			Url:             "",
		})

		capturedOutput := captureStdout(t, func() {
			_ = runner.Promote("dev", "", promoteFlags, promoteGitFlags)
		})
		if capturedOutput != "" {
			t.Errorf("runner.Promote wrote to stdout (MCP channel corruption!):\n%s", capturedOutput)
		}
	})
}

// captureStdout redirects os.Stdout to a pipe, calls fn, then restores Stdout
// and returns anything that was written.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	origStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w

	fn()

	w.Close()
	os.Stdout = origStdout

	var sb strings.Builder
	io.Copy(&sb, r)
	r.Close()

	return sb.String()
}

// findModuleRoot walks upward from the test binary location to find go.mod.
func findModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find module root (go.mod)")
		}
		dir = parent
	}
}

// copyDir recursively copies src directory into dst (existing dst is OK).
func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		return copyFile(path, target)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}
