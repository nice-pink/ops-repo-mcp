package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/nice-pink/repo-services/pkg/exceptional"
	"github.com/nice-pink/repo-services/pkg/manifest"
	"github.com/nice-pink/repo-services/pkg/runner"
	"github.com/nice-pink/repo-services/pkg/util"

	gogit "github.com/go-git/go-git/v6"
)

// handler holds shared server state and handles both tool calls.
type handler struct {
	cfg    serverConfig
	repoMu *timedMu
}

func newHandler(cfg serverConfig) *handler {
	return &handler{
		cfg:    cfg,
		repoMu: newTimedMu(),
	}
}

// Input validation regexes — handler-level security boundary.
// Schema-level constraints are informational for the LLM; these are enforced.
var (
	reAppEnv = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	reTag    = regexp.MustCompile(`^[a-zA-Z0-9_.-]+$`)
)

// toolResult marshals v to JSON and wraps it in mcp.NewToolResultText.
// On marshal error, returns an INVALID_INPUT error result.
func toolResult(v any) (*mcp.CallToolResult, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return mcp.NewToolResultText(`{"success":false,"errorCode":"RUNNER_FAILED","errorMessage":"marshal error"}`), nil
	}
	return mcp.NewToolResultText(string(b)), nil
}

// toolError builds and returns a structured error response.
func toolError(code, message, hint, runnerOutput string) (*mcp.CallToolResult, error) {
	resp := errorResponse{
		Success:      false,
		ErrorCode:    code,
		ErrorMessage: message,
		RecoveryHint: hint,
		RunnerOutput: runnerOutput,
	}
	return toolResult(resp)
}

func toolMcpError(e *mcpError, runnerOutput string) (*mcp.CallToolResult, error) {
	return toolError(e.code, e.message, e.hint, runnerOutput)
}

// checkEnvAllowlist returns an error if env is not in the allowlist (if configured).
func (h *handler) checkEnvAllowlist(env string) *mcpError {
	if len(h.cfg.EnvAllowlist) == 0 {
		return nil
	}
	if slices.Contains(h.cfg.EnvAllowlist, env) {
		return nil
	}
	return errEnvNotAllowed(env)
}

// validateField checks that a field value matches the given regex.
func validateField(name, value string, re *regexp.Regexp) *mcpError {
	if value == "" {
		return errInvalidInput(fmt.Sprintf("field %q is required", name))
	}
	if !re.MatchString(value) {
		return errInvalidInput(fmt.Sprintf("field %q value %q does not match pattern %s", name, value, re.String()))
	}
	return nil
}

// buildManifestHandler creates a ManifestHandler using the resolved config.
func (h *handler) buildManifestHandler(exceptionalAppsFile string) *manifest.ManifestHandler {
	eh := exceptional.NewExceptionalHandler(exceptionalAppsFile)
	return manifest.NewManifestHandler(eh)
}

// buildFlags constructs GeneralFlags and GitFlags for runner calls.
// gitFlags always have Push=false and Url="" hardcoded.
func (h *handler) buildFlags(app, namespace, env, base, cluster, image, imageFile, imageHistoryFile, pathScheme, exceptionalAppsFile, srcPath, versionInfo string) (util.GeneralFlags, util.GitFlags) {
	push := false
	emptyStr := ""

	return util.NewFlagsFromValues(util.FlagValues{
		App:                 app,
		Namespace:           namespace,
		Env:                 env,
		Base:                base,
		Cluster:             cluster,
		Image:               image,
		ImageFile:           imageFile,
		ImageHistoryFile:    imageHistoryFile,
		PathScheme:          pathScheme,
		ExceptionalAppsFile: exceptionalAppsFile,
		SrcPath:             srcPath,
		VersionInfo:         versionInfo,
		// Git flags — hardcoded for MCP (never push, never clone)
		Push:    push,
		Shallow: false,
		Url:     emptyStr,
		Branch:  emptyStr,
		Token:   h.cfg.GitToken,
		User:    h.cfg.GitUser,
		Email:   h.cfg.GitEmail,
		SshKeyPath: h.cfg.GitSSHKeyPath,
	})
}

// resolveAndCheckPath uses resolveAncestor to canonicalise the manifest path
// (which may not exist yet on first deploy) and then verifies it is inside the
// ops repo root. Returns PATH_ESCAPE if the path escapes.
func (h *handler) resolveAndCheckPath(manifestFile string) (string, *mcpError) {
	canonical, err := resolveAncestor(manifestFile)
	if err != nil {
		return "", errPathEscape(manifestFile, h.cfg.OpsRepoPath)
	}
	if pathEscape(h.cfg.OpsRepoPath, canonical) {
		return "", errPathEscape(canonical, h.cfg.OpsRepoPath)
	}
	return canonical, nil
}

// relPath returns the path of target relative to h.cfg.OpsRepoPath.
func (h *handler) relPath(target string) (string, error) {
	return filepath.Rel(h.cfg.OpsRepoPath, target)
}

// prePullSequence runs inside the runner goroutine (under the lock):
// 1. Opens the repo.
// 2. Refuses if the working tree is dirty (tracked changes only).
// 3. Refuses if the local branch is ahead of upstream.
// 4. Pulls (fetch + fast-forward).
func (h *handler) prePullSequence() error {
	rh := util.NewRepoHandle(h.cfg.GitSSHKeyPath, h.cfg.GitToken, h.cfg.GitUser, h.cfg.GitEmail)

	if err := rh.Open(h.cfg.OpsRepoPath); err != nil {
		return errPullFailed(fmt.Errorf("open: %w", err))
	}

	dirty, dirtyPaths, err := isWorkingTreeDirty(rh)
	if err != nil {
		return errPullFailed(fmt.Errorf("status: %w", err))
	}
	if dirty {
		return errDirtyRepo(dirtyPaths)
	}

	ahead, aheadBy, err := isAheadOfUpstream(rh)
	if err != nil {
		return errPullFailed(fmt.Errorf("ahead-check: %w", err))
	}
	if ahead {
		return errBranchAhead(aheadBy)
	}

	if err := rh.PullLocalRepo(h.cfg.OpsRepoPath); err != nil && err != gogit.NoErrAlreadyUpToDate {
		return errPullFailed(err)
	}

	return nil
}

// HandleDeploy is the MCP tool handler for the "deploy" tool.
func (h *handler) HandleDeploy(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// --- Input validation ---
	app := req.GetString("app", "")
	env := req.GetString("env", "")
	tag := req.GetString("tag", "")
	namespace := req.GetString("namespace", h.cfg.DSNamespace)
	dryRun := req.GetBool("dryRun", false)

	if e := validateField("app", app, reAppEnv); e != nil {
		return toolMcpError(e, "")
	}
	if e := validateField("env", env, reAppEnv); e != nil {
		return toolMcpError(e, "")
	}
	if e := validateField("tag", tag, reTag); e != nil {
		return toolMcpError(e, "")
	}
	if namespace != "" {
		if e := validateField("namespace", namespace, reAppEnv); e != nil {
			return toolMcpError(e, "")
		}
	}

	// Reject unknown extra fields (additionalProperties: false enforcement).
	args := req.GetArguments()
	allowed := map[string]bool{"app": true, "env": true, "tag": true, "namespace": true, "dryRun": true}
	for k := range args {
		if !allowed[k] {
			return toolMcpError(errInvalidInput(fmt.Sprintf("unknown field %q (additionalProperties: false)", k)), "")
		}
	}

	// Env allowlist check
	if e := h.checkEnvAllowlist(env); e != nil {
		return toolMcpError(e, "")
	}

	slog.Default().Info("tool_called", "tool", "deploy", "app", app, "env", env, "dryRun", dryRun)

	// --- Resolve config ---
	base := h.cfg.DSBase
	if base == "" {
		base = "."
	}
	cluster := base // default cluster == base
	image := app    // default image name == app name
	pathScheme := h.cfg.DSPathScheme
	imageFile := h.cfg.DSImageFileName
	imageHistoryFile := h.cfg.DSImageHistoryFileName
	exceptionalAppsFile := h.cfg.DSExceptionalAppsFile
	srcPath := h.cfg.OpsRepoPath

	flags, gitFlags := h.buildFlags(app, namespace, env, base, cluster, image, imageFile, imageHistoryFile, pathScheme, exceptionalAppsFile, srcPath, "")

	// --- Pre-flight: compute affected paths (before lock, read-only) ---
	mh := h.buildManifestHandler(exceptionalAppsFile)
	appObj := mh.BuildApp(flags, tag)

	manifestCanonical, pathErr := h.resolveAndCheckPath(appObj.File)
	if pathErr != nil {
		return toolMcpError(pathErr, "")
	}
	relManifest, err := h.relPath(manifestCanonical)
	if err != nil {
		return toolMcpError(errPathEscape(appObj.File, h.cfg.OpsRepoPath), "")
	}

	var relHistory string
	if appObj.History != "" {
		histCanonical, pathErr := h.resolveAndCheckPath(appObj.History)
		if pathErr != nil {
			return toolMcpError(pathErr, "")
		}
		relHistory, err = h.relPath(histCanonical)
		if err != nil {
			return toolMcpError(errPathEscape(appObj.History, h.cfg.OpsRepoPath), "")
		}
	}

	// --- Runner invocation (under lock) ---
	var historyExists bool

	fn := func() error {
		// 1. Pre-pull (dirty/ahead checks + pull)
		if e := h.prePullSequence(); e != nil {
			return e
		}

		if dryRun {
			// Dry-run: pull ran, no manifest mutation.
			return nil
		}

		// 2. Run deploy
		if err := runner.Deploy(tag, exceptionalAppsFile, flags, gitFlags); err != nil {
			return errRunnerFailed(err)
		}

		// 3. Post-call: check if history file exists (inside lock)
		if appObj.History != "" {
			_, statErr := os.Stat(appObj.History)
			historyExists = statErr == nil
		}
		return nil
	}

	runnerOutput, runErr := h.callRunner(ctx, "deploy", fn)

	if runErr != nil {
		if me, ok := runErr.(*mcpError); ok {
			return toolMcpError(me, runnerOutput)
		}
		return toolMcpError(errRunnerFailed(runErr), runnerOutput)
	}

	// --- Build response ---
	filesToStage := []string{relManifest}
	if !dryRun && historyExists && relHistory != "" {
		filesToStage = append(filesToStage, relHistory)
	}

	cd, rh := buildDeployCommitDirective(app, env, tag, h.cfg.OpsRepoPath, filesToStage, dryRun)

	resp := deploySuccessResponse{
		Success:         true,
		DryRun:          dryRun,
		App:             app,
		Env:             env,
		Tag:             tag,
		OpsRepoPath:     h.cfg.OpsRepoPath,
		CommitDirective: cd,
		RecoveryHint:    rh,
		RunnerOutput:    runnerOutput,
	}
	return toolResult(resp)
}

// HandleRollback is the MCP tool handler for the "rollback" tool.
// It reverts an app's image tag in a given env to the value the manifest had
// before the most recent commit that touched the manifest file. If that
// commit modified more than the image-tag line, the response includes
// MultiLineChange=true and a Warning string so the caller can stop and
// review before proceeding to commit the revert.
func (h *handler) HandleRollback(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// --- Input validation ---
	app := req.GetString("app", "")
	env := req.GetString("env", "")
	namespace := req.GetString("namespace", h.cfg.DSNamespace)
	dryRun := req.GetBool("dryRun", false)

	if e := validateField("app", app, reAppEnv); e != nil {
		return toolMcpError(e, "")
	}
	if e := validateField("env", env, reAppEnv); e != nil {
		return toolMcpError(e, "")
	}
	if namespace != "" {
		if e := validateField("namespace", namespace, reAppEnv); e != nil {
			return toolMcpError(e, "")
		}
	}

	args := req.GetArguments()
	allowed := map[string]bool{"app": true, "env": true, "namespace": true, "dryRun": true}
	for k := range args {
		if !allowed[k] {
			return toolMcpError(errInvalidInput(fmt.Sprintf("unknown field %q (additionalProperties: false)", k)), "")
		}
	}

	if e := h.checkEnvAllowlist(env); e != nil {
		return toolMcpError(e, "")
	}

	slog.Default().Info("tool_called", "tool", "rollback", "app", app, "env", env, "dryRun", dryRun)

	// --- Resolve config ---
	base := h.cfg.DSBase
	if base == "" {
		base = "."
	}
	cluster := base
	image := app
	pathScheme := h.cfg.DSPathScheme
	imageFile := h.cfg.DSImageFileName
	imageHistoryFile := h.cfg.DSImageHistoryFileName
	exceptionalAppsFile := h.cfg.DSExceptionalAppsFile
	srcPath := h.cfg.OpsRepoPath

	flags, gitFlags := h.buildFlags(app, namespace, env, base, cluster, image, imageFile, imageHistoryFile, pathScheme, exceptionalAppsFile, srcPath, "")

	mh := h.buildManifestHandler(exceptionalAppsFile)
	appObj := mh.BuildApp(flags, "")

	manifestCanonical, pathErr := h.resolveAndCheckPath(appObj.File)
	if pathErr != nil {
		return toolMcpError(pathErr, "")
	}
	relManifest, err := h.relPath(manifestCanonical)
	if err != nil {
		return toolMcpError(errPathEscape(appObj.File, h.cfg.OpsRepoPath), "")
	}

	var relHistory string
	if appObj.History != "" {
		histCanonical, pathErr := h.resolveAndCheckPath(appObj.History)
		if pathErr != nil {
			return toolMcpError(pathErr, "")
		}
		relHistory, err = h.relPath(histCanonical)
		if err != nil {
			return toolMcpError(errPathEscape(appObj.History, h.cfg.OpsRepoPath), "")
		}
	}

	// --- Runner invocation (under lock) ---
	var analysis *rollbackAnalysis
	var historyExists bool

	fn := func() error {
		if e := h.prePullSequence(); e != nil {
			return e
		}

		// Open repo to walk file history (post-pull).
		rh := util.NewRepoHandle(h.cfg.GitSSHKeyPath, h.cfg.GitToken, h.cfg.GitUser, h.cfg.GitEmail)
		if err := rh.Open(h.cfg.OpsRepoPath); err != nil {
			return errPullFailed(fmt.Errorf("open: %w", err))
		}

		// Use forward-slash path relative to repo root for go-git's FileName filter.
		relForGit := filepath.ToSlash(relManifest)
		imgPattern := mh.ImagePattern(appObj)

		var aErr error
		analysis, aErr = analyseRollback(rh.Repo(), relForGit, imgPattern)
		if aErr != nil {
			return errNoPrevVersion(app, env, aErr.Error())
		}
		if analysis.previousTag == "" {
			return errNoPrevVersion(app, env, "previous tag could not be determined")
		}

		if dryRun {
			return nil
		}

		// Apply the previous tag via the same path Deploy uses (atomic write +
		// history-file append).
		if err := runner.Deploy(analysis.previousTag, exceptionalAppsFile, flags, gitFlags); err != nil {
			return errRunnerFailed(err)
		}

		if appObj.History != "" {
			_, statErr := os.Stat(appObj.History)
			historyExists = statErr == nil
		}
		return nil
	}

	runnerOutput, runErr := h.callRunner(ctx, "rollback", fn)
	if runErr != nil {
		if me, ok := runErr.(*mcpError); ok {
			return toolMcpError(me, runnerOutput)
		}
		return toolMcpError(errRunnerFailed(runErr), runnerOutput)
	}

	// --- Build response ---
	filesToStage := []string{relManifest}
	if !dryRun && historyExists && relHistory != "" {
		filesToStage = append(filesToStage, relHistory)
	}

	cd, recovHint := buildRollbackCommitDirective(app, env, analysis.previousTag, h.cfg.OpsRepoPath, filesToStage, dryRun)

	multiLine := analysis.nonTagLines > 0
	var warning string
	if multiLine {
		warning = fmt.Sprintf(
			"the commit being rolled back (%s) modified %d line(s) in %s beyond the image tag — a tag-only revert may not restore the prior intent of the manifest; review the diff before committing",
			analysis.lastCommitHash[:7], analysis.nonTagLines, relManifest,
		)
	}

	resp := rollbackSuccessResponse{
		Success:           true,
		DryRun:            dryRun,
		App:               app,
		Env:               env,
		CurrentTag:        analysis.currentTag,
		PreviousTag:       analysis.previousTag,
		LastCommit:        analysis.lastCommitHash,
		LastCommitMessage: analysis.lastCommitMsg,
		ParentCommit:      analysis.parentCommitHash,
		MultiLineChange:   multiLine,
		NonTagLineChanges: analysis.nonTagLines,
		Warning:           warning,
		OpsRepoPath:       h.cfg.OpsRepoPath,
		CommitDirective:   cd,
		RecoveryHint:      recovHint,
		RunnerOutput:      runnerOutput,
	}
	return toolResult(resp)
}

// HandlePromote is the MCP tool handler for the "promote" tool.
func (h *handler) HandlePromote(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// --- Input validation ---
	app := req.GetString("app", "")
	destEnv := req.GetString("destEnv", "")
	srcEnv := req.GetString("srcEnv", h.cfg.DSSrcEnv)
	namespace := req.GetString("namespace", h.cfg.DSNamespace)
	dryRun := req.GetBool("dryRun", false)

	if e := validateField("app", app, reAppEnv); e != nil {
		return toolMcpError(e, "")
	}
	if e := validateField("destEnv", destEnv, reAppEnv); e != nil {
		return toolMcpError(e, "")
	}
	if e := validateField("srcEnv", srcEnv, reAppEnv); e != nil {
		return toolMcpError(e, "")
	}
	if namespace != "" {
		if e := validateField("namespace", namespace, reAppEnv); e != nil {
			return toolMcpError(e, "")
		}
	}

	// Reject unknown extra fields
	args := req.GetArguments()
	allowed := map[string]bool{"app": true, "destEnv": true, "srcEnv": true, "namespace": true, "dryRun": true}
	for k := range args {
		if !allowed[k] {
			return toolMcpError(errInvalidInput(fmt.Sprintf("unknown field %q (additionalProperties: false)", k)), "")
		}
	}

	// SAME_ENV guard
	if srcEnv == destEnv {
		return toolMcpError(errSameEnv(srcEnv), "")
	}

	// Env allowlist checks
	if e := h.checkEnvAllowlist(destEnv); e != nil {
		return toolMcpError(e, "")
	}
	if e := h.checkEnvAllowlist(srcEnv); e != nil {
		return toolMcpError(e, "")
	}

	slog.Default().Info("tool_called", "tool", "promote", "app", app, "srcEnv", srcEnv, "destEnv", destEnv, "dryRun", dryRun)

	// --- Resolve config ---
	base := h.cfg.DSBase
	if base == "" {
		base = "."
	}
	cluster := base
	image := app
	pathScheme := h.cfg.DSPathScheme
	imageFile := h.cfg.DSImageFileName
	imageHistoryFile := h.cfg.DSImageHistoryFileName
	exceptionalAppsFile := h.cfg.DSExceptionalAppsFile
	srcPath := h.cfg.OpsRepoPath

	// Build flags for destEnv (the env we're promoting INTO)
	destFlags, gitFlags := h.buildFlags(app, namespace, destEnv, base, cluster, image, imageFile, imageHistoryFile, pathScheme, exceptionalAppsFile, srcPath, "")

	// Build flags for srcEnv (the env we're promoting FROM)
	srcFlags, _ := h.buildFlags(app, namespace, srcEnv, base, cluster, image, imageFile, imageHistoryFile, pathScheme, exceptionalAppsFile, srcPath, "")

	// --- Pre-flight: compute affected paths (before lock, read-only) ---
	mh := h.buildManifestHandler(exceptionalAppsFile)
	destApp := mh.BuildApp(destFlags, "")

	manifestCanonical, pathErr := h.resolveAndCheckPath(destApp.File)
	if pathErr != nil {
		return toolMcpError(pathErr, "")
	}
	relManifest, err := h.relPath(manifestCanonical)
	if err != nil {
		return toolMcpError(errPathEscape(destApp.File, h.cfg.OpsRepoPath), "")
	}

	var relHistory string
	if destApp.History != "" {
		histCanonical, pathErr := h.resolveAndCheckPath(destApp.History)
		if pathErr != nil {
			return toolMcpError(pathErr, "")
		}
		relHistory, err = h.relPath(histCanonical)
		if err != nil {
			return toolMcpError(errPathEscape(destApp.History, h.cfg.OpsRepoPath), "")
		}
	}

	// --- Runner invocation (under lock) ---
	var resolvedTag string
	var historyExists bool

	fn := func() error {
		// 1. Pre-pull
		if e := h.prePullSequence(); e != nil {
			return e
		}

		// 2. Resolve srcEnv current tag (after pull, under lock)
		srcApp := mh.BuildApp(srcFlags, "")
		currentSrcTag := mh.GetCurrentTag(srcApp)
		if currentSrcTag == "" {
			return errNoCurrentTag(app, srcEnv)
		}

		if dryRun {
			resolvedTag = currentSrcTag
			return nil
		}

		// 3. Run promote
		// runner.Promote reconstructs src from flags by overwriting Env with srcEnv
		if err := runner.Promote(srcEnv, exceptionalAppsFile, destFlags, gitFlags); err != nil {
			return errRunnerFailed(err)
		}

		// 4. Post-call: read resolved tag from destEnv manifest (inside lock)
		destAppUpdated := mh.BuildApp(destFlags, "")
		resolvedTag = mh.GetCurrentTag(destAppUpdated)

		// 5. Post-call: check if history file exists (inside lock)
		if destApp.History != "" {
			_, statErr := os.Stat(destApp.History)
			historyExists = statErr == nil
		}
		return nil
	}

	runnerOutput, runErr := h.callRunner(ctx, "promote", fn)

	if runErr != nil {
		if me, ok := runErr.(*mcpError); ok {
			return toolMcpError(me, runnerOutput)
		}
		return toolMcpError(errRunnerFailed(runErr), runnerOutput)
	}

	// --- Build response ---
	filesToStage := []string{relManifest}
	if !dryRun && historyExists && relHistory != "" {
		filesToStage = append(filesToStage, relHistory)
	}

	cd, recovHint := buildPromoteCommitDirective(app, destEnv, resolvedTag, h.cfg.OpsRepoPath, filesToStage, dryRun)

	resp := promoteSuccessResponse{
		Success:         true,
		DryRun:          dryRun,
		App:             app,
		SrcEnv:          srcEnv,
		DestEnv:         destEnv,
		ResolvedTag:     resolvedTag,
		OpsRepoPath:     h.cfg.OpsRepoPath,
		CommitDirective: cd,
		RecoveryHint:    recovHint,
		RunnerOutput:    runnerOutput,
	}
	return toolResult(resp)
}
