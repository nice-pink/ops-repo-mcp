package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// resolveHelperEnv puts the re-executed test binary into helper mode, where it
// runs resolveOpsRepoPath and reports the result instead of running tests.
// resolveOpsRepoPath exits the process on every failure path, so the failures
// are only observable from outside.
const resolveHelperEnv = "OPS_TEST_RESOLVE_HELPER"

// configHelperEnv runs the whole of loadConfig instead, so the decisions it
// makes downstream of the resolution — chiefly whether an ambient GITHUB_TOKEN
// is adopted — are covered by a test rather than by reading the code.
const configHelperEnv = "OPS_TEST_CONFIG_HELPER"

func TestMain(m *testing.M) {
	if os.Getenv(resolveHelperEnv) == "1" {
		r := resolveOpsRepoPath()
		fmt.Printf("%s\t%s\t%s\n", r.Path, r.Source, r.NormalisedFrom)
		os.Exit(0)
	}
	if os.Getenv(configHelperEnv) == "1" {
		cfg := loadConfig()
		fmt.Printf("source=%s gitToken=%q suppressed=%t markerWaived=%t notWalkable=%t\n",
			cfg.OpsRepoPathSource, cfg.GitToken, cfg.GitHubTokenSuppressed,
			cfg.AnyCwdRepoAllowed, cfg.OpsRepoPathNotWalkable != "")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type resolveResult struct {
	path, source, normalisedFrom string
	exitCode                     int
	stderr                       string
}

// runResolve re-executes the test binary in helper mode with cwd set to dir and
// env applied on top of a cleared MCP_OPS_REPO_PATH / MCP_ALLOW_ANY_CWD_REPO.
func runResolve(t *testing.T, dir string, env ...string) resolveResult {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	// -test.run guards against recursion: if TestMain ever stops intercepting the
	// helper env var, the child runs no tests instead of re-forking the suite.
	cmd := exec.Command(self, "-test.run=^$")
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
	fields := strings.SplitN(strings.TrimRight(stdout.String(), "\n"), "\t", 3)
	if len(fields) != 3 {
		t.Fatalf("helper stdout %q is not path\\tsource\\tnormalisedFrom (stderr: %s)", stdout.String(), res.stderr)
	}
	res.path, res.source, res.normalisedFrom = fields[0], fields[1], fields[2]
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
	// An explicit author: go-git otherwise falls back to the machine's git
	// identity and returns ErrMissingAuthor where there is none, which is every
	// CI runner.
	sig := &object.Signature{Name: "ops-repo-mcp test", Email: "test@example.invalid", When: time.Now()}
	if _, err := wt.Commit("init", &gogit.CommitOptions{Author: sig, Committer: sig}); err != nil {
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
	linked := filepath.Join(t.TempDir(), "linked")
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

// A designated path pointing at a subdirectory is reported as normalised, so
// main can say which path was given and which one is being used. A path used
// verbatim reports nothing.
func TestResolveReportsNormalisation(t *testing.T) {
	repo := tempRepo(t, true)
	sub := mkdirIn(t, repo, "base", "apps")

	res := runResolve(t, t.TempDir(), opsRepoPathEnv+"="+sub)
	res.wantOK(t, repo, opsRepoPathEnv)
	if res.normalisedFrom != sub {
		t.Errorf("normalisedFrom = %q, want %q", res.normalisedFrom, sub)
	}

	res = runResolve(t, t.TempDir(), opsRepoPathEnv+"="+repo)
	res.wantOK(t, repo, opsRepoPathEnv)
	if res.normalisedFrom != "" {
		t.Errorf("normalisedFrom = %q, want empty for a path used as given", res.normalisedFrom)
	}
}

// A freshly `git init`ed repo has an unborn HEAD. It reaches the same check as a
// linked worktree, so the message has to account for both — a new ops repo that
// has not been committed to yet is a far more likely arrival than a worktree.
func TestGitWorkTreeRootRejectsUnbornHead(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if _, err := gogit.PlainInit(root, false); err != nil {
		t.Fatalf("PlainInit: %v", err)
	}

	_, err = gitWorkTreeRoot(root)
	if err == nil {
		t.Fatalf("accepted a repo with no commits")
	}
	for _, want := range []string{"no commits yet", "worktree"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

// runLoadConfig re-executes the test binary in loadConfig-helper mode.
func runLoadConfig(t *testing.T, dir string, env ...string) (out string, exitCode int, stderr string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	cmd := exec.Command(self, "-test.run=^$")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		configHelperEnv+"=1",
		opsRepoPathEnv+"=",
		allowAnyCwdRepoEnv+"=",
		"MCP_GIT_TOKEN=",
		"GITHUB_TOKEN=",
		"DS_SRC_PATH=",
		"MCP_ENV_ALLOWLIST=dev,prod",
	)
	cmd.Env = append(cmd.Env, env...)

	var so, se strings.Builder
	cmd.Stdout = &so
	cmd.Stderr = &se
	runErr := cmd.Run()
	if ee, ok := runErr.(*exec.ExitError); ok {
		return "", ee.ExitCode(), se.String()
	}
	if runErr != nil {
		t.Fatalf("helper: %v (stderr: %s)", runErr, se.String())
	}
	return strings.TrimRight(so.String(), "\n"), 0, se.String()
}

// The security control this change turns on. An ambient GITHUB_TOKEN is exported
// on most developer machines for unrelated reasons; the pre-flight fetch sends
// the token to whatever origin the resolved repo has, unscoped by host. So it is
// adopted only for a repo the operator designated.
func TestGitHubTokenOnlyAdoptedForDesignatedRepo(t *testing.T) {
	repo := tempRepo(t, true)

	t.Run("inferred repo does not adopt it", func(t *testing.T) {
		out, code, stderr := runLoadConfig(t, repo, "GITHUB_TOKEN=ambient-secret")
		if code != 0 {
			t.Fatalf("exit %d (stderr: %s)", code, stderr)
		}
		if !strings.Contains(out, `gitToken=""`) {
			t.Errorf("GITHUB_TOKEN leaked to an inferred repo: %s", out)
		}
		if !strings.Contains(out, "suppressed=true") {
			t.Errorf("suppression not recorded, so nothing warns: %s", out)
		}
	})

	t.Run("designated repo does adopt it", func(t *testing.T) {
		out, code, stderr := runLoadConfig(t, t.TempDir(),
			opsRepoPathEnv+"="+repo, "GITHUB_TOKEN=ambient-secret")
		if code != 0 {
			t.Fatalf("exit %d (stderr: %s)", code, stderr)
		}
		if !strings.Contains(out, `gitToken="ambient-secret"`) {
			t.Errorf("designated repo should still use GITHUB_TOKEN: %s", out)
		}
		if !strings.Contains(out, "suppressed=false") {
			t.Errorf("nothing was suppressed here: %s", out)
		}
	})

	t.Run("MCP_GIT_TOKEN applies on either path", func(t *testing.T) {
		out, code, _ := runLoadConfig(t, repo, "MCP_GIT_TOKEN=deliberate")
		if code != 0 {
			t.Fatalf("exit %d", code)
		}
		if !strings.Contains(out, `gitToken="deliberate"`) {
			t.Errorf("MCP_GIT_TOKEN is a deliberate statement and must be honoured: %s", out)
		}
		if !strings.Contains(out, "suppressed=false") {
			t.Errorf("nothing to suppress when MCP_GIT_TOKEN is set: %s", out)
		}
	})

	t.Run("no GITHUB_TOKEN means nothing to report", func(t *testing.T) {
		out, code, _ := runLoadConfig(t, repo)
		if code != 0 {
			t.Fatalf("exit %d", code)
		}
		if !strings.Contains(out, "suppressed=false") {
			t.Errorf("suppressed must be false when no token was present: %s", out)
		}
	})
}

// The acknowledgement must describe what actually happened. Setting the flag
// defensively in a shared client config, in a repo that does carry the marker,
// must not produce a warning asserting the marker is missing.
func TestMarkerWaivedOnlyWhenMarkerAbsent(t *testing.T) {
	marked := tempRepo(t, true)
	unmarked := tempRepo(t, false)

	out, code, stderr := runLoadConfig(t, marked, allowAnyCwdRepoEnv+"=1")
	if code != 0 {
		t.Fatalf("exit %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(out, "markerWaived=false") {
		t.Errorf("flag set but marker present: nothing was waived, so nothing should say it was: %s", out)
	}

	out, code, stderr = runLoadConfig(t, unmarked, allowAnyCwdRepoEnv+"=1")
	if code != 0 {
		t.Fatalf("exit %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(out, "markerWaived=true") {
		t.Errorf("marker absent and waived by the flag: %s", out)
	}
}
