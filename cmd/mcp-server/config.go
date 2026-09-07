package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// serverConfig holds all configuration loaded from environment variables at startup.
type serverConfig struct {
	OpsRepoPath   string // canonical (EvalSymlinks'd) absolute path
	EnvAllowlist  []string
	LogLevel      slog.Level
	LockTimeout   time.Duration
	RunnerTimeout time.Duration
	GitSSHKeyPath string
	GitToken      string
	GitUser       string
	GitEmail      string

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

	// MCP_OPS_REPO_PATH — required
	raw := os.Getenv("MCP_OPS_REPO_PATH")
	if raw == "" {
		fatal(codeConfigError, "MCP_OPS_REPO_PATH is not set")
	}
	canonical, err := evalSymlinksOrFatal(raw)
	if err != nil {
		fatal(codeRepoNotFound, fmt.Sprintf("MCP_OPS_REPO_PATH %q: %v", raw, err))
	}
	cfg.OpsRepoPath = canonical

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

	// MCP_GIT_TOKEN — falls back to GITHUB_TOKEN
	cfg.GitToken = os.Getenv("MCP_GIT_TOKEN")
	if cfg.GitToken == "" {
		cfg.GitToken = os.Getenv("GITHUB_TOKEN")
	}

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

	// DS_SRC_PATH — warn if set (not honoured; MCP always uses MCP_OPS_REPO_PATH)
	if os.Getenv("DS_SRC_PATH") != "" {
		slog.Default().Warn("ds_src_path_ignored", "msg", "DS_SRC_PATH is set but ignored by mcp-server; MCP_OPS_REPO_PATH is always used")
	}

	cfg.RepoConfigFound = rc != nil
	cfg.LayoutSources = sources

	return cfg
}

func fatal(code, msg string) {
	fmt.Fprintf(os.Stderr, "FATAL: %s: %s\n", code, msg)
	os.Exit(2)
}

func evalSymlinksOrFatal(path string) (string, error) {
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
