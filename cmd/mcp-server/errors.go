package main

import "fmt"

// Error codes as defined in the spec.
const (
	codeInvalidInput   = "INVALID_INPUT"
	codeEnvNotAllowed  = "ENV_NOT_ALLOWED"
	codeSameEnv        = "SAME_ENV"
	codePathEscape     = "PATH_ESCAPE"
	codeRepoNotFound   = "REPO_NOT_FOUND"
	codeConfigError    = "CONFIG_ERROR"
	codeDirtyRepo      = "DIRTY_REPO"
	codeBranchAhead    = "BRANCH_AHEAD"
	codePullFailed     = "PULL_FAILED"
	codeNoCurrentTag   = "NO_CURRENT_TAG"
	codeNoPrevVersion  = "NO_PREVIOUS_VERSION"
	codeRunnerFailed   = "RUNNER_FAILED"
	codeRunnerPanic    = "RUNNER_PANIC"
	codeRunnerTimeout  = "RUNNER_TIMEOUT"
	codeLockTimeout    = "LOCK_TIMEOUT"
)

type mcpError struct {
	code    string
	message string
	hint    string // optional recovery hint for the caller
}

func (e *mcpError) Error() string {
	return fmt.Sprintf("[%s] %s", e.code, e.message)
}

func errInvalidInput(msg string) *mcpError {
	return &mcpError{code: codeInvalidInput, message: msg}
}

func errEnvNotAllowed(env string) *mcpError {
	return &mcpError{code: codeEnvNotAllowed, message: fmt.Sprintf("env %q is not in MCP_ENV_ALLOWLIST", env)}
}

func errSameEnv(env string) *mcpError {
	return &mcpError{code: codeSameEnv, message: fmt.Sprintf("srcEnv and destEnv are both %q; promote would be a no-op", env)}
}

func errPathEscape(computed, root string) *mcpError {
	return &mcpError{code: codePathEscape, message: fmt.Sprintf("computed path %q escapes ops repo root %q", computed, root)}
}

func errDirtyRepo(paths []string) *mcpError {
	return &mcpError{
		code:    codeDirtyRepo,
		message: fmt.Sprintf("working tree has uncommitted changes: %v", paths),
		hint:    "complete or discard prior changes before retrying (see recoveryHint)",
	}
}

func errBranchAhead(n int) *mcpError {
	return &mcpError{
		code:    codeBranchAhead,
		message: fmt.Sprintf("local branch is %d commit(s) ahead of upstream; refusing to pull", n),
		hint:    "run 'git push' to publish local commits, or 'git reset --hard origin/<branch>' to discard them",
	}
}

func errPullFailed(err error) *mcpError {
	return &mcpError{code: codePullFailed, message: err.Error()}
}

func errNoCurrentTag(app, env string) *mcpError {
	return &mcpError{
		code:    codeNoCurrentTag,
		message: fmt.Sprintf("cannot promote %s from %s — manifest has no current tag", app, env),
		hint:    fmt.Sprintf("run a deploy to %s first", env),
	}
}

func errNoPrevVersion(app, env, reason string) *mcpError {
	return &mcpError{
		code:    codeNoPrevVersion,
		message: fmt.Sprintf("cannot roll back %s(%s): %s", app, env, reason),
		hint:    "the manifest's git history does not expose a prior tag; deploy an earlier tag explicitly with the deploy tool",
	}
}

func errRunnerFailed(err error) *mcpError {
	return &mcpError{code: codeRunnerFailed, message: err.Error()}
}

func errRunnerPanic(v any) *mcpError {
	return &mcpError{code: codeRunnerPanic, message: fmt.Sprintf("runner panic: %v", v)}
}

func errRunnerTimeout() *mcpError {
	return &mcpError{code: codeRunnerTimeout, message: "runner goroutine did not return within MCP_RUNNER_TIMEOUT; goroutine leaked and lock held until it completes"}
}

func errLockTimeout(err error) *mcpError {
	return &mcpError{code: codeLockTimeout, message: fmt.Sprintf("could not acquire repo lock within MCP_LOCK_TIMEOUT: %v", err)}
}
