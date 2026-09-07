package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v6"
)

// resolveHelperEnv puts the re-executed test binary into helper mode, where it
// runs resolveOpsRepoPath and reports the result instead of running tests.
// resolveOpsRepoPath exits the process on every failure path, so the failures
// are only observable from outside.
const resolveHelperEnv = "OPS_TEST_RESOLVE_HELPER"

func TestMain(m *testing.M) {
	if os.Getenv(resolveHelperEnv) == "1" {
		path, source := resolveOpsRepoPath()
		fmt.Printf("%s\t%s\n", path, source)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type resolveResult struct {
	path, source string
	exitCode     int
	stderr       string
}

// runResolve re-executes the test binary in helper mode with cwd set to dir and
// env applied on top of a cleared MCP_OPS_REPO_PATH / MCP_ALLOW_ANY_CWD_REPO.
func runResolve(t *testing.T, dir string, env ...string) resolveResult {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	cmd := exec.Command(self)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		resolveHelperEnv+"=1",
		opsRepoPathEnv+"=",
		allowAnyCwdRepoEnv+"=",
	)
	cmd.Env = append(cmd.Env, env...)

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	res := resolveResult{stderr: stderr.String()}
	if ee, ok := runErr.(*exec.ExitError); ok {
		res.exitCode = ee.ExitCode()
		return res
	}
	if runErr != nil {
		t.Fatalf("helper: %v (stderr: %s)", runErr, res.stderr)
	}
	fields := strings.SplitN(strings.TrimRight(stdout.String(), "\n"), "\t", 2)
	if len(fields) != 2 {
		t.Fatalf("helper stdout %q is not path\\tsource (stderr: %s)", stdout.String(), res.stderr)
	}
	res.path, res.source = fields[0], fields[1]
	return res
}

func (r resolveResult) wantOK(t *testing.T, path, source string) {
	t.Helper()
	if r.exitCode != 0 {
		t.Fatalf("helper exited %d, want success (stderr: %s)", r.exitCode, r.stderr)
	}
	if r.path != path {
		t.Errorf("path = %q, want %q", r.path, path)
	}
	if r.source != source {
		t.Errorf("source = %q, want %q", r.source, source)
	}
}

func (r resolveResult) wantFatal(t *testing.T, code string, msgContains ...string) {
	t.Helper()
	if r.exitCode != 2 {
		t.Fatalf("helper exited %d, want 2 (stderr: %s)", r.exitCode, r.stderr)
	}
	if !strings.Contains(r.stderr, "FATAL: "+code+":") {
		t.Errorf("stderr does not report %s:\n%s", code, r.stderr)
	}
	for _, want := range msgContains {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("stderr does not mention %q:\n%s", want, r.stderr)
		}
	}
}

// tempRepo creates a git work tree with one commit and returns its canonical
// root. markOps controls whether it carries the ops-repo marker file.
func tempRepo(t *testing.T, markOps bool) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	initRepoAt(t, root, markOps)
	return root
}

func initRepoAt(t *testing.T, root string, markOps bool) {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	repo, err := gogit.PlainInit(root, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	if markOps {
		writeMarker(t, root)
	}
	// A commit, so HEAD resolves — gitWorkTreeRoot requires it.
	if err := os.WriteFile(filepath.Join(root, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatalf("AddGlob: %v", err)
	}
	if _, err := wt.Commit("init", &gogit.CommitOptions{AllowEmptyCommits: true}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

func writeMarker(t *testing.T, root string) {
	t.Helper()
	body := "version: 1\nlayout:\n  base: base/apps\n"
	if err := os.WriteFile(filepath.Join(root, repoConfigFileName), []byte(body), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
}

func mkdirIn(t *testing.T, root string, parts ...string) string {
	t.Helper()
	dir := filepath.Join(append([]string{root}, parts...)...)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	return dir
}

// --- gitWorkTreeRoot ---

func TestGitWorkTreeRootFromSubdir(t *testing.T) {
	root := tempRepo(t, true)
	sub := mkdirIn(t, root, "base", "apps", "poma")

	got, err := gitWorkTreeRoot(sub)
	if err != nil {
		t.Fatalf("gitWorkTreeRoot(%q): %v", sub, err)
	}
	if got != root {
		t.Errorf("got %q, want repo root %q", got, root)
	}
}

func TestGitWorkTreeRootAtRoot(t *testing.T) {
	root := tempRepo(t, true)
	got, err := gitWorkTreeRoot(root)
	if err != nil {
		t.Fatalf("gitWorkTreeRoot(%q): %v", root, err)
	}
	if got != root {
		t.Errorf("got %q, want %q", got, root)
	}
}

// The symlink is the case filepath.EvalSymlinks exists for: on macOS a temp dir
// reached via /var resolves to /private/var. Without canonicalising the work
// tree root, the returned path would not match the paths every other check is
// rooted at.
func TestGitWorkTreeRootCanonicalisesSymlinkedPath(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	initRepoAt(t, real, true)
	canonicalReal, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}

	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	got, err := gitWorkTreeRoot(link)
	if err != nil {
		t.Fatalf("gitWorkTreeRoot(%q): %v", link, err)
	}
	if got != canonicalReal {
		t.Errorf("got %q, want canonical %q", got, canonicalReal)
	}
}

func TestGitWorkTreeRootRejectsNonRepo(t *testing.T) {
	dir := t.TempDir()
	_, err := gitWorkTreeRoot(dir)
	if err == nil {
		t.Fatalf("succeeded on a directory outside any git work tree")
	}
	if !strings.Contains(err.Error(), "not inside a git work tree") {
		t.Errorf("error does not say why: %v", err)
	}
}

func TestGitWorkTreeRootRejectsBareRepo(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if _, err := gogit.PlainInit(dir, true); err != nil {
		t.Fatalf("PlainInit bare: %v", err)
	}
	if _, err := gitWorkTreeRoot(dir); err == nil {
		t.Fatalf("succeeded on a bare repository, which has no work tree to deploy into")
	}
}

// A linked worktree opens cleanly and reports a work tree, but keeps its refs
// in the main repo's commondir. The runner opens the repo with git.PlainOpen,
// which does not read that, so HEAD is unresolvable at call time and every tool
// reports a detached HEAD and a fully dirty tree. Startup must refuse it.
func TestGitWorkTreeRootRejectsLinkedWorktree(t *testing.T) {
	main := tempRepo(t, true)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	linked := filepath.Join(filepath.Dir(main), "linked")
	out, err := exec.Command("git", "-C", main, "worktree", "add", "-q", "-b", "feature", linked).CombinedOutput()
	if err != nil {
		t.Skipf("git worktree add: %v (%s)", err, out)
	}

	_, err = gitWorkTreeRoot(linked)
	if err == nil {
		t.Fatalf("accepted a linked worktree the runner cannot read refs from")
	}
	if !strings.Contains(err.Error(), "HEAD") {
		t.Errorf("error does not name the HEAD problem: %v", err)
	}
}

// --- resolveOpsRepoPath ---

func TestResolveEnvWinsAndSkipsGitCheck(t *testing.T) {
	repo := tempRepo(t, true)
	// Not a git repo, and no marker: a designated path is still accepted.
	plain, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}

	res := runResolve(t, repo, opsRepoPathEnv+"="+plain)
	res.wantOK(t, plain, opsRepoPathEnv)
}

// A designated path pointing inside a work tree is normalised to the repo root.
// The runner opens the repo with git.PlainOpen, so the un-normalised form fails
// every call with PULL_FAILED while .ops-repo-mcp.yaml goes silently unread.
func TestResolveEnvNormalisesSubdirToRepoRoot(t *testing.T) {
	repo := tempRepo(t, true)
	sub := mkdirIn(t, repo, "base", "apps")

	res := runResolve(t, t.TempDir(), opsRepoPathEnv+"="+sub)
	res.wantOK(t, repo, opsRepoPathEnv)
}

func TestResolveEnvMissingPathIsRepoNotFound(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	res := runResolve(t, t.TempDir(), opsRepoPathEnv+"="+missing)
	res.wantFatal(t, codeRepoNotFound, missing)
}

func TestResolveFallsBackToCwdRepoRoot(t *testing.T) {
	repo := tempRepo(t, true)
	sub := mkdirIn(t, repo, "base", "apps", "poma")

	res := runResolve(t, sub)
	res.wantOK(t, repo, opsRepoPathCwd)
}

// The marker is what separates "a git repo" from "a repo someone intends to
// deploy from". Without it any ancestor .git — a dotfiles repo at $HOME, say —
// would become a deploy target, and would supply the branch guard, the layout,
// and the remote the pre-flight fetch authenticates against.
func TestResolveCwdWithoutMarkerIsRefused(t *testing.T) {
	repo := tempRepo(t, false)
	sub := mkdirIn(t, repo, "nested")

	res := runResolve(t, sub)
	res.wantFatal(t, codeConfigError, repoConfigFileName, allowAnyCwdRepoEnv)
}

func TestResolveCwdWithoutMarkerAllowedByOptOut(t *testing.T) {
	repo := tempRepo(t, false)
	sub := mkdirIn(t, repo, "nested")

	res := runResolve(t, sub, allowAnyCwdRepoEnv+"=1")
	res.wantOK(t, repo, opsRepoPathCwd)
}

func TestResolveCwdOutsideGitRepoIsConfigError(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	res := runResolve(t, dir)
	res.wantFatal(t, codeConfigError, "not inside a git work tree", opsRepoPathEnv)
}

// The opt-out waives the ops-repo marker, not the git requirement: without a
// repo there is no branch guard, no dirty check, and no rollback history.
func TestResolveCwdOptOutStillRequiresGitRepo(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	res := runResolve(t, dir, allowAnyCwdRepoEnv+"=1")
	res.wantFatal(t, codeConfigError, "not inside a git work tree")
}

// opsRepoPathCwd is compared in main.go and published to operators as the
// opsRepoSource log field and in the docs. A rename would silently disable the
// ops_repo_from_cwd warning with every other test still green.
func TestOpsRepoSourceConstants(t *testing.T) {
	if opsRepoPathEnv != "MCP_OPS_REPO_PATH" {
		t.Errorf("opsRepoPathEnv = %q", opsRepoPathEnv)
	}
	if opsRepoPathCwd != "cwd" {
		t.Errorf("opsRepoPathCwd = %q", opsRepoPathCwd)
	}
	if allowAnyCwdRepoEnv != "MCP_ALLOW_ANY_CWD_REPO" {
		t.Errorf("allowAnyCwdRepoEnv = %q", allowAnyCwdRepoEnv)
	}
}
