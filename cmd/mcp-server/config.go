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
	OpsRepoPath     string // canonical (EvalSymlinks'd) absolute path
	EnvAllowlist    []string
	LogLevel        slog.Level
	LockTimeout     time.Duration
	RunnerTimeout   time.Duration
	GitSSHKeyPath   string
	GitToken        string
	GitUser         string
	GitEmail        string

	// DS_ defaults — validated at startup, passed through flags to the runner
	DSNamespace             string
	DSBase                  string
	DSPathScheme            string
	DSImageFileName         string
	DSImageHistoryFileName  string
	DSExceptionalAppsFile   string
	DSSrcEnv                string
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

	// MCP_ENV_ALLOWLIST — optional, comma-separated
	if v := os.Getenv("MCP_ENV_ALLOWLIST"); v != "" {
		parts := strings.Split(v, ",")
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				cfg.EnvAllowlist = append(cfg.EnvAllowlist, p)
			}
		}
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

	// DS_NAMESPACE
	cfg.DSNamespace = os.Getenv("DS_NAMESPACE")
	if cfg.DSNamespace != "" && !reNamespaceVal.MatchString(cfg.DSNamespace) {
		fatal(codeConfigError,fmt.Sprintf("DS_NAMESPACE %q does not match ^[a-z0-9][a-z0-9-]*$", cfg.DSNamespace))
	}

	// DS_BASE
	cfg.DSBase = os.Getenv("DS_BASE")
	if cfg.DSBase != "" && !reBaseVal.MatchString(cfg.DSBase) {
		fatal(codeConfigError,fmt.Sprintf("DS_BASE %q does not match ^[A-Za-z0-9_./-]+$", cfg.DSBase))
	}

	// DS_PATH_SCHEME
	cfg.DSPathScheme = os.Getenv("DS_PATH_SCHEME")
	if cfg.DSPathScheme == "" {
		cfg.DSPathScheme = "{base}/{namespace}/{app}/{env}"
	}
	if err := validatePathScheme(cfg.DSPathScheme); err != nil {
		fatal(codeConfigError,fmt.Sprintf("DS_PATH_SCHEME: %v", err))
	}

	// DS_IMAGE_FILE_NAME
	cfg.DSImageFileName = os.Getenv("DS_IMAGE_FILE_NAME")
	if cfg.DSImageFileName == "" {
		cfg.DSImageFileName = "deployment.yaml"
	}
	if !validateFileName(cfg.DSImageFileName) {
		fatal(codeConfigError,fmt.Sprintf("DS_IMAGE_FILE_NAME %q: must not contain '/' or '..'", cfg.DSImageFileName))
	}

	// DS_IMAGE_HISTORY_FILE_NAME
	cfg.DSImageHistoryFileName = os.Getenv("DS_IMAGE_HISTORY_FILE_NAME")
	if cfg.DSImageHistoryFileName != "" && !validateFileName(cfg.DSImageHistoryFileName) {
		fatal(codeConfigError,fmt.Sprintf("DS_IMAGE_HISTORY_FILE_NAME %q: must not contain '/' or '..'", cfg.DSImageHistoryFileName))
	}

	// DS_EXCEPTIONAL_APPS_FILE — must exist on disk if set
	cfg.DSExceptionalAppsFile = os.Getenv("DS_EXCEPTIONAL_APPS_FILE")
	if cfg.DSExceptionalAppsFile != "" {
		if _, err := os.Stat(cfg.DSExceptionalAppsFile); err != nil {
			fatal(codeConfigError,fmt.Sprintf("DS_EXCEPTIONAL_APPS_FILE %q does not exist or is not accessible: %v", cfg.DSExceptionalAppsFile, err))
		}
	}

	// DS_SRC_PATH — warn if set (not honoured; MCP always uses MCP_OPS_REPO_PATH)
	if os.Getenv("DS_SRC_PATH") != "" {
		slog.Default().Warn("ds_src_path_ignored", "msg", "DS_SRC_PATH is set but ignored by mcp-server; MCP_OPS_REPO_PATH is always used")
	}

	// DS_SRC_ENV
	cfg.DSSrcEnv = os.Getenv("DS_SRC_ENV")
	if cfg.DSSrcEnv == "" {
		cfg.DSSrcEnv = "staging"
	}

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
