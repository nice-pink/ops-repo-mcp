package main

import "fmt"

// Error codes as defined in the spec.
const (
	codeInvalidInput     = "INVALID_INPUT"
	codeEnvNotAllowed    = "ENV_NOT_ALLOWED"
	codeSameEnv          = "SAME_ENV"
	codePathEscape       = "PATH_ESCAPE"
	codeRepoNotFound     = "REPO_NOT_FOUND"
	codeConfigError      = "CONFIG_ERROR"
	codeDirtyRepo        = "DIRTY_REPO"
	codeBranchAhead      = "BRANCH_AHEAD"
	codeBranchNotAllowed = "BRANCH_NOT_ALLOWED"
	codeNoUpstream       = "NO_UPSTREAM"
	codePullFailed       = "PULL_FAILED"
	codeNoCurrentTag     = "NO_CURRENT_TAG"
	codeNoPrevVersion    = "NO_PREVIOUS_VERSION"
	codeMultiLineChange  = "MULTI_LINE_CHANGE"
	codeRunnerFailed     = "RUNNER_FAILED"
	codeRunnerPanic      = "RUNNER_PANIC"
	codeRunnerTimeout    = "RUNNER_TIMEOUT"
	codeLockTimeout      = "LOCK_TIMEOUT"
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

func errBranchNotAllowed(current string, allowed []string, source string) *mcpError {
	return &mcpError{
		code:    codeBranchNotAllowed,
		message: fmt.Sprintf("ops repo is on branch %q, which is not in the allowed set %v (from %s)", current, allowed, source),
		hint:    fmt.Sprintf("check out an allowed branch in the ops repo, or set MCP_ALLOWED_BRANCHES to include %q if writing there is intended", current),
	}
}

func errDetachedHead(reason string) *mcpError {
	return &mcpError{
		code:    codeBranchNotAllowed,
		message: fmt.Sprintf("cannot determine the ops repo branch: %s", reason),
		hint:    "check out a branch in the ops repo; a detached HEAD cannot be pushed",
	}
}

func errNoUpstream(branch string) *mcpError {
	return &mcpError{
		code:    codeNoUpstream,
		message: fmt.Sprintf("branch %q has no upstream (origin/%s); a commit here could not be pushed", branch, branch),
		hint:    fmt.Sprintf("run 'git push -u origin %s' in the ops repo, or switch to a tracked branch", branch),
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

// errMultiLineChange refuses a rollback whose target commit changed more than
// the image tag. Reverting only the tag would leave the other edits in place,
// producing old image + new configuration — a combination that has never run
// anywhere. The caller must look at the commit and opt in explicitly.
func errMultiLineChange(app, env, currentTag, previousTag, lastCommit string, nonTagLines int, manifest string) *mcpError {
	current := currentTag
	extra := ""
	if current == "" {
		current = "(could not be read)"
		// An unreadable current tag makes every differing line count as a
		// non-tag change, so the count is not evidence of a wide commit.
		extra = " The current tag could not be read from the manifest, so nonTagLineChanges counts every differing line and may overstate the change."
	}
	return &mcpError{
		code: codeMultiLineChange,
		message: fmt.Sprintf(
			"refusing to roll back %s(%s) from %s to %s: commit %s changed %d line(s) in %s beyond the image tag, and a tag-only revert would leave those in place (old image, new configuration).%s",
			app, env, current, previousTag, lastCommit, nonTagLines, manifest, extra,
		),
		hint: fmt.Sprintf(
			"inspect it with 'git -C <opsRepoPath> show %s'. To revert the tag only, call rollback again with acknowledgeMultiLineChange=true. To undo the whole commit, use git revert instead.",
			lastCommit,
		),
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

// errLockHeldByLeakedRunner distinguishes "someone else is mid-call" from
// "a previous call timed out and its runner never returned". The second needs a
// restart, not a retry, and the ops repo may have been written meanwhile.
func errLockHeldByLeakedRunner(n int64) *mcpError {
	return &mcpError{
		code:    codeLockTimeout,
		message: fmt.Sprintf("could not acquire the repo lock: %d earlier runner call(s) exceeded MCP_RUNNER_TIMEOUT and never returned, so the lock is still held", n),
		hint:    "this will not clear by retrying — restart the MCP server. Check 'git -C <opsRepoPath> status' first: the leaked runner may have written the manifest after its call already returned an error",
	}
}

func errLockTimeout(err error) *mcpError {
	return &mcpError{code: codeLockTimeout, message: fmt.Sprintf("could not acquire repo lock within MCP_LOCK_TIMEOUT: %v", err)}
}
