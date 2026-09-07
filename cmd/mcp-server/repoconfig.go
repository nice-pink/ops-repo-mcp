package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	yaml "gopkg.in/yaml.v3"
)

// repoConfigFileName is the layout config file the server looks for at the root
// of the ops repo. It is optional: when absent, layout comes from DS_* env vars
// and the built-in defaults, exactly as before.
const repoConfigFileName = ".ops-repo-mcp.yaml"

// repoConfigVersion is the only schema version this server understands.
const repoConfigVersion = 1

// repoConfig is the on-disk schema of .ops-repo-mcp.yaml.
//
// It intentionally carries ONLY layout fields — where manifests live inside the
// repo. Operational settings (MCP_OPS_REPO_PATH, MCP_ENV_ALLOWLIST, git
// credentials, timeouts) are deliberately absent and can never be set from this
// file: it lives in a repository that agents are expected to write to, so a pull
// request against the ops repo must not be able to widen the server's authority
// or redirect its credentials. Decoding is strict (KnownFields), so a file that
// tries to set one of those fails loudly at startup instead of being silently
// ignored.
type repoConfig struct {
	Version int              `yaml:"version"`
	Branch  string           `yaml:"branch"`
	Layout  repoConfigLayout `yaml:"layout"`
}

// repoConfigLayout mirrors the DS_* layout env vars one-for-one.
type repoConfigLayout struct {
	Base                 string `yaml:"base"`
	Namespace            string `yaml:"namespace"`
	PathScheme           string `yaml:"pathScheme"`
	ImageFileName        string `yaml:"imageFileName"`
	ImageHistoryFileName string `yaml:"imageHistoryFileName"`
	ExceptionalAppsFile  string `yaml:"exceptionalAppsFile"`
	SrcEnv               string `yaml:"srcEnv"`
}

// repoConfigPath returns the path the loader looks at for a given repo root.
func repoConfigPath(repoRoot string) string {
	return filepath.Join(repoRoot, repoConfigFileName)
}

// loadRepoConfig reads .ops-repo-mcp.yaml from repoRoot.
//
// Returns (nil, nil) when the file does not exist — that is the normal case for
// a repo that has not been configured, not an error. Any other problem (bad
// YAML, unknown field, wrong version) is returned as an error for the caller to
// turn into a CONFIG_ERROR, so this stays unit-testable without os.Exit.
//
// Layout values are NOT validated here. Validation happens in resolveLayout, on
// the value that actually wins, so that a field the operator has overridden via
// its DS_* env var cannot make the server refuse to start.
func loadRepoConfig(repoRoot string) (*repoConfig, error) {
	path := repoConfigPath(repoRoot)

	// The file is committed in a repo that agents write to, so a single bad line
	// in someone's commit would otherwise stop the server from starting at all,
	// for every tool, with no way to override it from the client config. This is
	// that override.
	if os.Getenv("MCP_IGNORE_REPO_CONFIG") == "1" {
		return nil, nil
	}

	// Confine the config file itself the same way its exceptionalAppsFile is
	// confined: a symlink in the repo pointing outside it must not be followed.
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		canonicalRoot, rootErr := filepath.EvalSymlinks(repoRoot)
		if rootErr != nil {
			return nil, fmt.Errorf("ops repo root %q: %w", repoRoot, rootErr)
		}
		target, tErr := filepath.EvalSymlinks(path)
		if tErr != nil {
			return nil, fmt.Errorf("%s: %w", repoConfigFileName, tErr)
		}
		if pathEscape(canonicalRoot, target) {
			return nil, fmt.Errorf("%s is a symlink to %q, which is outside the ops repo; refusing to read it", repoConfigFileName, target)
		}
	}

	buf, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("%s: %w", repoConfigFileName, err)
	}

	// A directory (or device) at that path is not a config file.
	if fi, statErr := os.Stat(path); statErr == nil && !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", repoConfigFileName)
	}

	var rc repoConfig
	dec := yaml.NewDecoder(bytes.NewReader(buf))
	dec.KnownFields(true)
	if err := dec.Decode(&rc); err != nil {
		if errors.Is(err, io.EOF) {
			// Present but empty (or only comments/whitespace). Treat as "no
			// overrides", not as an error.
			return &repoConfig{Version: repoConfigVersion}, nil
		}
		return nil, fmt.Errorf("%s: %w%s", repoConfigFileName, err, unknownFieldHint(err))
	}

	// Only the first YAML document is decoded. A second one would be silently
	// discarded — including any unknown key in it — which contradicts the
	// fail-loudly contract, so reject it.
	var extra repoConfig
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: contains more than one YAML document; only the first would be used, so this is rejected rather than silently ignored", repoConfigFileName)
	}

	// version 0 means the key was omitted. Accept it as the current version so a
	// hand-written file is not rejected over a missing line, but reject any
	// explicit version this server does not understand.
	if rc.Version != 0 && rc.Version != repoConfigVersion {
		return nil, fmt.Errorf("%s: version %d is not supported (this server understands version %d)", repoConfigFileName, rc.Version, repoConfigVersion)
	}
	rc.Version = repoConfigVersion

	return &rc, nil
}

// unknownFieldHint appends the layout-only explanation, but only for the decode
// error that actually means "you tried to set something we do not accept".
// Attaching it to a plain syntax error would be a non-sequitur.
func unknownFieldHint(err error) string {
	if err == nil || !strings.Contains(err.Error(), "not found in type") {
		return ""
	}
	return " (only 'version', 'branch' and 'layout' are accepted; operational settings such as envAllowlist, credentials and timeouts must stay in the client's env block, because this file lives in a repo that agents write to)"
}

// resolveExceptionalAppsFile turns the relative layout.exceptionalAppsFile into
// an absolute path, confines it to the repo, and confirms it is a regular file.
//
// The textual checks live here rather than in a general validate() because this
// is only reached when DS_EXCEPTIONAL_APPS_FILE is unset — i.e. only when the
// file's value is the one that wins.
func (rc *repoConfig) resolveExceptionalAppsFile(repoRoot string) (string, error) {
	if rc == nil || rc.Layout.ExceptionalAppsFile == "" {
		return "", nil
	}
	rel := rc.Layout.ExceptionalAppsFile

	// Unlike DS_EXCEPTIONAL_APPS_FILE, which an operator sets and may point
	// anywhere on the machine, this value comes from inside the repo. It must be
	// relative and must stay in the repo.
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("layout.exceptionalAppsFile %q must be relative to the repo root, not absolute", rel)
	}
	if strings.Contains(rel, "..") {
		return "", fmt.Errorf("layout.exceptionalAppsFile %q must not contain '..'", rel)
	}

	// Canonicalise the root before comparing. resolveAncestor returns a
	// symlink-resolved path, so an unresolved root (/var/... vs /private/var/...)
	// would make every containment check fail. loadConfig already passes a
	// canonical path, but this must not depend on that.
	canonicalRoot, err := filepath.EvalSymlinks(repoRoot)
	if err != nil {
		return "", fmt.Errorf("ops repo root %q: %w", repoRoot, err)
	}

	resolved, err := resolveAncestor(filepath.Join(canonicalRoot, rel))
	if err != nil {
		return "", fmt.Errorf("layout.exceptionalAppsFile %q: %w", rel, err)
	}
	// Re-checked after symlink resolution, so an in-repo symlink pointing out of
	// the repo is caught too.
	if pathEscape(canonicalRoot, resolved) {
		return "", fmt.Errorf("layout.exceptionalAppsFile %q escapes the ops repo root", rel)
	}

	fi, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("layout.exceptionalAppsFile %q does not exist or is not accessible: %v", rel, err)
	}
	// A directory (or device/socket) here would panic deep inside the runner's
	// exceptional-apps reader, which is not recovered on the pre-flight path.
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("layout.exceptionalAppsFile %q is not a regular file", rel)
	}
	return resolved, nil
}

// layoutSource records where each resolved layout value came from, so startup
// logging can show it. Silent config precedence is its own debugging tax.
type layoutSource map[string]string

const (
	srcFromEnv     = "env"
	srcFromFile    = "repo-file"
	srcFromDefault = "default"
)

// String renders the sources in a stable order. Go randomises map iteration, so
// formatting the map directly would reorder the fields on every restart and make
// two startup logs impossible to diff.
func (ls layoutSource) String() string {
	keys := make([]string, 0, len(ls))
	for k := range ls {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteString(" ")
		}
		fmt.Fprintf(&b, "%s=%s", k, ls[k])
	}
	return b.String()
}

// pickLayout resolves one layout field by precedence: env var, then the repo
// config file, then the built-in default. Returns the value and its source.
//
// An empty env var is treated as unset — os.Getenv cannot distinguish "unset"
// from "set to empty", and a client config that passes DS_BASE through as an
// empty string must not shadow the repo file.
func pickLayout(envVal, fileVal, def string) (string, string) {
	if envVal != "" {
		return envVal, srcFromEnv
	}
	if fileVal != "" {
		return fileVal, srcFromFile
	}
	return def, srcFromDefault
}

// layoutValues is the resolved layout, independent of where each value came from.
type layoutValues struct {
	Namespace            string
	Base                 string
	PathScheme           string
	ImageFileName        string
	ImageHistoryFileName string
	SrcEnv               string
}

// resolveLayout applies the precedence rules to every layout field and validates
// the value that won. Keeping resolution and validation in one pure function
// means there is a single place where a layout value is checked (no divergence
// between the env path and the file path) and it is testable without os.Exit.
//
// getenv is injected so tests can drive it without mutating the process env.
func resolveLayout(getenv func(string) string, fl repoConfigLayout) (layoutValues, layoutSource, error) {
	var lv layoutValues
	src := layoutSource{}

	// describe names the offending setting the way the operator wrote it, so the
	// error points at the env var or at the file — whichever actually supplied it.
	describe := func(field, envVar string) string {
		if src[field] == srcFromFile {
			return fmt.Sprintf("%s layout.%s", repoConfigFileName, field)
		}
		return envVar
	}

	lv.Namespace, src["namespace"] = pickLayout(getenv("DS_NAMESPACE"), fl.Namespace, "")
	if lv.Namespace != "" && !reNamespaceVal.MatchString(lv.Namespace) {
		return lv, src, fmt.Errorf("%s %q does not match ^[a-z0-9][a-z0-9-]*$", describe("namespace", "DS_NAMESPACE"), lv.Namespace)
	}

	lv.Base, src["base"] = pickLayout(getenv("DS_BASE"), fl.Base, "")
	if lv.Base != "" {
		if !reBaseVal.MatchString(lv.Base) {
			return lv, src, fmt.Errorf("%s %q does not match ^[A-Za-z0-9_./-]+$", describe("base", "DS_BASE"), lv.Base)
		}
		// reBaseVal permits '.' and '/', so it matches ".." and "/etc" on its
		// own. Both would place the manifest folder outside the repo.
		if strings.Contains(lv.Base, "..") {
			return lv, src, fmt.Errorf("%s %q must not contain '..'", describe("base", "DS_BASE"), lv.Base)
		}
		if strings.HasPrefix(lv.Base, "/") {
			return lv, src, fmt.Errorf("%s %q must be relative to the ops repo root, not absolute", describe("base", "DS_BASE"), lv.Base)
		}
	}

	lv.PathScheme, src["pathScheme"] = pickLayout(getenv("DS_PATH_SCHEME"), fl.PathScheme, "{base}/{namespace}/{app}/{env}")
	if err := validatePathScheme(lv.PathScheme); err != nil {
		return lv, src, fmt.Errorf("%s: %w", describe("pathScheme", "DS_PATH_SCHEME"), err)
	}

	lv.ImageFileName, src["imageFileName"] = pickLayout(getenv("DS_IMAGE_FILE_NAME"), fl.ImageFileName, "deployment.yaml")
	if !validateFileName(lv.ImageFileName) {
		return lv, src, fmt.Errorf("%s %q: must not contain '/' or '..'", describe("imageFileName", "DS_IMAGE_FILE_NAME"), lv.ImageFileName)
	}

	lv.ImageHistoryFileName, src["imageHistoryFileName"] = pickLayout(getenv("DS_IMAGE_HISTORY_FILE_NAME"), fl.ImageHistoryFileName, "")
	if lv.ImageHistoryFileName != "" && !validateFileName(lv.ImageHistoryFileName) {
		return lv, src, fmt.Errorf("%s %q: must not contain '/' or '..'", describe("imageHistoryFileName", "DS_IMAGE_HISTORY_FILE_NAME"), lv.ImageHistoryFileName)
	}

	lv.SrcEnv, src["srcEnv"] = pickLayout(getenv("DS_SRC_ENV"), fl.SrcEnv, "staging")

	return lv, src, nil
}
