package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v6"
)

// serverConfig holds all configuration loaded from environment variables at startup.
type serverConfig struct {
	OpsRepoPath string // canonical (EvalSymlinks'd) absolute path
	// OpsRepoPathSource names where OpsRepoPath came from — "MCP_OPS_REPO_PATH"
	// or "cwd" — so the startup log answers "which repo is this server on, and
	// why" without the operator having to guess.
	OpsRepoPathSource string
	EnvAllowlist      []string
	LogLevel          slog.Level
	LockTimeout       time.Duration
	RunnerTimeout     time.Duration
	GitSSHKeyPath     string
	GitToken          string
	GitUser           string
	GitEmail          string

	// DS_ defaults — validated at startup, passed through flags to the runner
	DSNamespace            string
	DSBase                 string
	DSPathScheme           string
	DSImageFileName        string
	DSImageHistoryFileName string
	DSExceptionalAppsFile  string
	DSSrcEnv               string

	// AllEnvsAllowed records that the operator explicitly opted out of the
	// environment allowlist via MCP_ALLOW_ALL_ENVS=1.
	AllEnvsAllowed bool

	// GitHubTokenSuppressed records that a GITHUB_TOKEN was present but not
	// adopted, because the ops repo was inferred rather than designated.
	GitHubTokenSuppressed bool

	// AnyCwdRepoAllowed records that the operator waived the ops-repo marker on
	// the inferred path via MCP_ALLOW_ANY_CWD_REPO=1.
	AnyCwdRepoAllowed bool

	// AllowedBranches restricts which ops-repo branch the tools may write to.
	// Empty means "infer from origin/HEAD at call time, and skip the check if
	// that cannot be resolved". AllowedBranchSource names where it came from,
	// for the error message.
	AllowedBranches     []string
	AllowedBranchSource string

	// RepoConfigFound reports whether .ops-repo-mcp.yaml was present at the ops
	// repo root. LayoutSources records, per layout field, whether the resolved
	// value came from the env block, that file, or the built-in default.
	RepoConfigFound bool
	LayoutSources   layoutSource
}

// validation regexes — applied to DS_* env vars
var (
	reNamespaceVal = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	reBaseVal      = regexp.MustCompile(`^[A-Za-z0-9_./-]+$`)
)

// loadConfig reads env vars, validates DS_* fields, and returns a populated
// serverConfig. It calls os.Exit(2) on any validation failure (CONFIG_ERROR or
// REPO_NOT_FOUND) so the operator sees an immediate clear fatal message.
func loadConfig() serverConfig {
	var cfg serverConfig

	// MCP_OPS_REPO_PATH — optional; falls back to the working directory the
	// client launched this process in. See resolveOpsRepoPath.
	cfg.OpsRepoPath, cfg.OpsRepoPathSource = resolveOpsRepoPath()

	// MCP_ENV_ALLOWLIST — the guardrail on which environments the tools may
	// touch. An empty allowlist used to mean "every environment", with only a
	// startup warning, so an out-of-the-box server could write to prod. It is
	// now fail-closed: running unrestricted requires saying so explicitly.
	if v := os.Getenv("MCP_ENV_ALLOWLIST"); v != "" {
		parts := strings.Split(v, ",")
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				cfg.EnvAllowlist = append(cfg.EnvAllowlist, p)
			}
		}
	}
	if len(cfg.EnvAllowlist) == 0 {
		if os.Getenv("MCP_ALLOW_ALL_ENVS") != "1" {
			fatal(codeConfigError, "MCP_ENV_ALLOWLIST is empty. Set it to the environments this server may write to (e.g. \"dev,staging\"), or set MCP_ALLOW_ALL_ENVS=1 to accept every environment including production.")
		}
		cfg.AllEnvsAllowed = true
	}

	// MCP_LOG_LEVEL
	cfg.LogLevel = parseLogLevel(os.Getenv("MCP_LOG_LEVEL"))

	// MCP_LOCK_TIMEOUT
	cfg.LockTimeout = parseDuration(os.Getenv("MCP_LOCK_TIMEOUT"), 30*time.Second)

	// MCP_RUNNER_TIMEOUT
	cfg.RunnerTimeout = parseDuration(os.Getenv("MCP_RUNNER_TIMEOUT"), 60*time.Second)

	// MCP_GIT_SSH_KEY_PATH
	cfg.GitSSHKeyPath = os.Getenv("MCP_GIT_SSH_KEY_PATH")

	// MCP_GIT_TOKEN — falls back to GITHUB_TOKEN, but only for a designated repo.
	//
	// The token is attached as HTTP Basic auth to the pre-flight fetch, and
	// go-git does not scope that by host: it goes to whatever `origin` the ops
	// repo has. While the repo was always MCP_OPS_REPO_PATH, the operator chose
	// that remote. An inferred repo is chosen by which directory the client
	// happened to open, so an ambient GITHUB_TOKEN — exported for entirely
	// unrelated reasons on most developer machines — would be sent to the remote
	// of any clone the agent is pointed at. MCP_GIT_TOKEN still applies: setting
	// it is a deliberate statement about this server.
	cfg.GitToken = os.Getenv("MCP_GIT_TOKEN")
	if cfg.GitToken == "" && cfg.OpsRepoPathSource == opsRepoPathEnv {
		cfg.GitToken = os.Getenv("GITHUB_TOKEN")
	}
	cfg.GitHubTokenSuppressed = cfg.GitToken == "" &&
		cfg.OpsRepoPathSource == opsRepoPathCwd &&
		os.Getenv("GITHUB_TOKEN") != ""

	// MCP_GIT_USER
	cfg.GitUser = os.Getenv("MCP_GIT_USER")
	if cfg.GitUser == "" {
		cfg.GitUser = "mcp-server"
	}

	// MCP_GIT_EMAIL
	cfg.GitEmail = os.Getenv("MCP_GIT_EMAIL")

	// .ops-repo-mcp.yaml — optional layout config committed in the ops repo.
	// Layout only: it can never set credentials, timeouts, or the env allowlist
	// (see repoConfig). Precedence for every field is
	// env var > repo file > built-in default, applied by resolveLayout.
	rc, rcErr := loadRepoConfig(cfg.OpsRepoPath)
	if rcErr != nil {
		fatal(codeConfigError, rcErr.Error())
	}
	var fl repoConfigLayout
	if rc != nil {
		fl = rc.Layout
	}

	lv, sources, layoutErr := resolveLayout(os.Getenv, fl)
	if layoutErr != nil {
		fatal(codeConfigError, layoutErr.Error())
	}
	cfg.DSNamespace = lv.Namespace
	cfg.DSBase = lv.Base
	cfg.DSPathScheme = lv.PathScheme
	cfg.DSImageFileName = lv.ImageFileName
	cfg.DSImageHistoryFileName = lv.ImageHistoryFileName
	cfg.DSSrcEnv = lv.SrcEnv

	// DS_SRC_ENV is only ever used as a default for promote's srcEnv, and the
	// promote handler re-validates it per call (returning INVALID_INPUT). Warn
	// rather than refuse to start: an odd-but-set value used to break only
	// promote-without-srcEnv, and turning that into a startup exit would take
	// deploy and rollback down with it.
	if !reNamespaceVal.MatchString(cfg.DSSrcEnv) {
		slog.Default().Warn("invalid_src_env",
			"value", cfg.DSSrcEnv,
			"source", sources["srcEnv"],
			"msg", "does not match ^[a-z0-9][a-z0-9-]*$; promote will return INVALID_INPUT unless srcEnv is passed explicitly",
		)
	}

	// DS_EXCEPTIONAL_APPS_FILE — must exist on disk if set. The env var may point
	// anywhere the operator likes; the repo-file form is relative to the repo root
	// and is confined to it.
	cfg.DSExceptionalAppsFile = os.Getenv("DS_EXCEPTIONAL_APPS_FILE")
	if cfg.DSExceptionalAppsFile != "" {
		sources["exceptionalAppsFile"] = srcFromEnv
		fi, err := os.Stat(cfg.DSExceptionalAppsFile)
		if err != nil {
			fatal(codeConfigError, fmt.Sprintf("DS_EXCEPTIONAL_APPS_FILE %q does not exist or is not accessible: %v", cfg.DSExceptionalAppsFile, err))
		}
		// A non-regular file panics deep in the runner's reader on a code path
		// that is not recovered.
		if !fi.Mode().IsRegular() {
			fatal(codeConfigError, fmt.Sprintf("DS_EXCEPTIONAL_APPS_FILE %q is not a regular file", cfg.DSExceptionalAppsFile))
		}
	} else {
		resolved, err := rc.resolveExceptionalAppsFile(cfg.OpsRepoPath)
		if err != nil {
			fatal(codeConfigError, fmt.Sprintf("%s: %v", repoConfigFileName, err))
		}
		cfg.DSExceptionalAppsFile = resolved
		if resolved != "" {
			sources["exceptionalAppsFile"] = srcFromFile
		} else {
			sources["exceptionalAppsFile"] = srcFromDefault
		}
	}

	// MCP_ALLOWED_BRANCHES — operational, env-only, wins over the repo file.
	// Guards against writing a "prod deploy" onto a branch nothing deploys from:
	// the mutation succeeds, the agent reports success, and the cluster never
	// sees it.
	if v := os.Getenv("MCP_ALLOWED_BRANCHES"); v != "" {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				cfg.AllowedBranches = append(cfg.AllowedBranches, p)
			}
		}
		cfg.AllowedBranchSource = "MCP_ALLOWED_BRANCHES"
	} else if rc != nil && rc.Branch != "" {
		cfg.AllowedBranches = []string{rc.Branch}
		cfg.AllowedBranchSource = repoConfigFileName + " branch"
	}
	if len(cfg.AllowedBranches) == 0 {
		cfg.AllowedBranchSource = "origin/HEAD"
	}

	// DS_SRC_PATH — warn if set (not honoured; the ops repo is the resolved
	// MCP_OPS_REPO_PATH / cwd, never a runner flag)
	if os.Getenv("DS_SRC_PATH") != "" {
		slog.Default().Warn("ds_src_path_ignored", "msg", "DS_SRC_PATH is set but ignored by mcp-server; the ops repo is MCP_OPS_REPO_PATH, or the working directory when that is unset")
	}

	cfg.AnyCwdRepoAllowed = cfg.OpsRepoPathSource == opsRepoPathCwd && os.Getenv(allowAnyCwdRepoEnv) == "1"
	cfg.RepoConfigFound = rc != nil
	cfg.LayoutSources = sources

	return cfg
}

// opsRepoPathEnv / opsRepoPathCwd name the two ways the ops repo is located.
const (
	opsRepoPathEnv     = "MCP_OPS_REPO_PATH"
	opsRepoPathCwd     = "cwd"
	allowAnyCwdRepoEnv = "MCP_ALLOW_ANY_CWD_REPO"
)

// resolveOpsRepoPath decides which repository this server operates on, and
// returns it alongside the name of the source it came from.
//
// MCP_OPS_REPO_PATH is the operator's deliberate designation and stays
// authoritative: it is accepted whether or not it is a git repo, exactly as
// before. It is only normalised — a path inside a work tree resolves to that
// work tree's root, because the runner opens the repo with git.PlainOpen and a
// path pointing at a subdirectory fails every call with PULL_FAILED while the
// layout file goes silently unread.
//
// When it is unset, the working directory the client launched this process in
// is used, resolved up to the root of its git work tree. That makes one client
// entry serve any number of ops repos — the repo is whichever one the session
// is open in — instead of pinning the server to a single clone.
//
// An inferred path is held to a much higher bar than a designated one, because
// nobody chose it and the consequences of picking the wrong repo are not
// confined to a failed call: the branch guard, the layout, and the remote that
// the pre-flight fetch authenticates against all come from whatever repo is
// resolved here. So it must be a git work tree the server can actually operate
// on, and it must carry the ops-repo marker (see opsRepoMarkerOK). Without the
// marker, any ancestor .git turns its repo into a deploy target — a dotfiles
// repo at $HOME makes $HOME the ops repo for every client launched below it.
//
// It exits the process on failure, like the rest of loadConfig.
func resolveOpsRepoPath() (path, source string) {
	if raw := os.Getenv(opsRepoPathEnv); raw != "" {
		canonical, err := canonicalDir(raw)
		if err != nil {
			fatal(codeRepoNotFound, fmt.Sprintf("%s %q: %v", opsRepoPathEnv, raw, err))
		}
		// Normalise only when the walk succeeds. A designated path that is not a
		// usable work tree keeps its literal value: refusing it here would break
		// setups that work today, and the tools report the real reason per call.
		if root, err := gitWorkTreeRoot(canonical); err == nil && root != canonical {
			slog.Default().Info("ops_repo_normalised",
				"given", canonical,
				"repoRoot", root,
				"msg", opsRepoPathEnv+" points inside a git work tree; using its root, which is what every manifest path is relative to",
			)
			canonical = root
		}
		return canonical, opsRepoPathEnv
	}

	wd, err := os.Getwd()
	if err != nil {
		fatal(codeConfigError, fmt.Sprintf("%s is not set and the working directory cannot be determined (%v). Set %s to the ops repo clone.", opsRepoPathEnv, err, opsRepoPathEnv))
	}
	canonical, err := canonicalDir(wd)
	if err != nil {
		fatal(codeConfigError, fmt.Sprintf("%s is not set and the working directory %q is unusable: %v. Set %s to the ops repo clone.", opsRepoPathEnv, wd, err, opsRepoPathEnv))
	}
	root, err := gitWorkTreeRoot(canonical)
	if err != nil {
		fatal(codeConfigError, fmt.Sprintf("%s is not set, so the working directory %q would be used as the ops repo, but %v. Open the client in an ops repo clone, or set %s.", opsRepoPathEnv, canonical, err, opsRepoPathEnv))
	}
	if err := opsRepoMarkerOK(root); err != nil {
		fatal(codeConfigError, fmt.Sprintf("%s is not set, so the working directory resolved to the git repo %q, but %v. Commit a %s at that repo's root to mark it as an ops repo, or set %s to designate the repo explicitly, or set %s=1 to deploy into any git repo the client is opened in.", opsRepoPathEnv, root, err, repoConfigFileName, opsRepoPathEnv, allowAnyCwdRepoEnv))
	}
	return root, opsRepoPathCwd
}

// opsRepoMarkerOK reports whether root declares itself an ops repo, by carrying
// a .ops-repo-mcp.yaml at its root.
//
// This is only ever applied to an inferred path. A git repo is not evidence of
// an ops repo, and everything downstream trusts whatever is resolved here — the
// path-escape check is rooted at it, so it can certify "inside the resolved
// repo" but never "inside the right repo". The marker is the cheapest positive
// signal available that a human intended this repository to be deployed from.
//
// Presence is the whole test: the file's contents are parsed later by
// loadRepoConfig, and MCP_IGNORE_REPO_CONFIG=1 suppresses that parse without
// suppressing this check, because the file is being read here as a marker
// rather than as configuration.
//
// MCP_ALLOW_ANY_CWD_REPO=1 opts out, mirroring MCP_ALLOW_ALL_ENVS. It is a real
// choice with a real cost — see the credential note in main.go's startup
// warning — so it has to be made explicitly rather than reached by default.
func opsRepoMarkerOK(root string) error {
	if os.Getenv(allowAnyCwdRepoEnv) == "1" {
		return nil
	}
	fi, err := os.Lstat(filepath.Join(root, repoConfigFileName))
	if err != nil {
		return fmt.Errorf("it carries no %s, so nothing marks it as an ops repo", repoConfigFileName)
	}
	if fi.IsDir() {
		return fmt.Errorf("its %s is a directory, not a file", repoConfigFileName)
	}
	return nil
}

// gitWorkTreeRoot walks up from dir to the root of the git work tree containing
// it, so launching the client in a subdirectory of the ops repo still resolves
// to the repo root — every manifest path this server builds is relative to that
// root.
//
// HEAD must resolve, which is the property the tools actually need rather than
// the one that is convenient to check. A linked worktree from `git worktree add`
// opens cleanly here but keeps its refs in the main repo's commondir, and the
// runner opens the repo with git.PlainOpen, which does not read it: every call
// would then report a detached HEAD on a branch that is not detached, and a
// DIRTY_REPO listing every tracked file. Failing at startup names the reason.
func gitWorkTreeRoot(dir string) (string, error) {
	repo, err := gogit.PlainOpenWithOptions(dir, &gogit.PlainOpenOptions{DetectDotGit: true})
	if err != nil {
		return "", fmt.Errorf("it is not inside a git work tree: %w", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		return "", fmt.Errorf("its git repository has no usable work tree: %w", err)
	}
	if _, err := repo.Head(); err != nil {
		return "", fmt.Errorf("its git HEAD cannot be resolved (%w) — a linked worktree from `git worktree add` looks like this, and the runner cannot read its refs", err)
	}
	root, err := canonicalDir(wt.Filesystem.Root())
	if err != nil {
		return "", fmt.Errorf("its work tree root is unusable: %w", err)
	}
	return root, nil
}

func fatal(code, msg string) {
	fmt.Fprintf(os.Stderr, "FATAL: %s: %s\n", code, msg)
	os.Exit(2)
}

// canonicalDir stats path, requires it to be a directory, and returns it with
// symlinks resolved.
func canonicalDir(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("path does not exist: %w", err)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("path exists but is not a directory")
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("EvalSymlinks: %w", err)
	}
	return canonical, nil
}

func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func parseDuration(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return def
	}
	return d
}

func validatePathScheme(scheme string) error {
	if !strings.Contains(scheme, "{app}") {
		return fmt.Errorf("must contain {app}")
	}
	if !strings.Contains(scheme, "{env}") {
		return fmt.Errorf("must contain {env}")
	}
	if strings.Contains(scheme, "..") {
		return fmt.Errorf("must not contain '..'")
	}
	return nil
}

func validateFileName(name string) bool {
	if strings.Contains(name, "/") {
		return false
	}
	if strings.Contains(name, "..") {
		return false
	}
	return true
}
