# ops-repo-mcp error codes

## How failures arrive

A failed call is **not** an MCP protocol error. The server marshals its own envelope and
returns it as an ordinary successful tool result, so `isError` is never set:

```json
{
  "success": false,
  "errorCode": "DIRTY_REPO",
  "errorMessage": "working tree has uncommitted changes: [base/resources/test/test-app/dev/deployment.yaml]",
  "recoveryHint": "complete or discard prior changes before retrying (see recoveryHint)",
  "runnerOutput": ""
}
```

**Read `success` on every response.** A client that branches on `isError` alone treats
every failure as a success and will happily report a deploy that never happened.

Then branch on `errorCode`, never on `errorMessage` text. `recoveryHint` is a plain string
here and is omitted when the code carries no hint — the structured `recoveryHint` object
with `detectCommand` / `discardCommand` / `completeCommand` appears only on **success**
responses.

## What may already have been written

**The response tells you.** Errors raised after the runner started carry:

```json
{
  "success": false,
  "errorCode": "RUNNER_FAILED",
  "mayHaveWritten": true,
  "dirtyPaths": ["base/apps/poma-mcp/prod/deployment.yaml"],
  "postFailureRecovery": { "detectCommand": "...", "discardCommand": "...", "completeCommand": "..." }
}
```

`mayHaveWritten` appears only on `RUNNER_FAILED`, `RUNNER_PANIC` and `RUNNER_TIMEOUT` —
the three codes that can be returned once the runner is writing. Every other code is
raised before the write phase and leaves the repo untouched, except the pre-existing mess
`DIRTY_REPO` is reporting.

`dirtyPaths` is what git reports as modified **now**. An empty or absent `dirtyPaths`
alongside `mayHaveWritten: true` means the runner failed before changing anything — say
that, rather than alarming the user. When it is non-empty, show the paths and offer
`postFailureRecovery.discardCommand`; never leave them for the next call to trip over.

After a `RUNNER_TIMEOUT` the runner is **still running**, so `dirtyPaths` is a snapshot of
a moving target. Re-check with `detectCommand` before acting.

## Retry unchanged

| Code | Meaning | What to do |
|---|---|---|
| `LOCK_TIMEOUT` | This call waited longer than `MCP_LOCK_TIMEOUT` for the per-repo lock and gave up. The timeout bounds *your* wait, not the holder's runtime. | **Read the message.** Ordinary contention ("could not acquire repo lock within MCP_LOCK_TIMEOUT") is worth one retry. If it says *"N earlier runner call(s) exceeded MCP_RUNNER_TIMEOUT and never returned"*, retrying is pointless — the lock is held by a goroutine that cannot be cancelled. Tell the user to restart the server, and to check `git status` on the ops repo first because that runner may still be writing. |
| `PULL_FAILED` | The pre-flight failed before any write. Either the `git fetch` / fast-forward against the remote failed, or the repo could not be opened or inspected at all. | Check the `errorMessage` prefix. `open:`, `status:`, or `ahead-check:` means the ops repo is not a usable git clone — fix `MCP_OPS_REPO_PATH`, or the directory the client was launched in, and do not retry. An unprefixed message is the remote: retry once for a transient network error, otherwise check `MCP_GIT_SSH_KEY_PATH` / `MCP_GIT_TOKEN`. |

## Fix the ops repo, then retry

These are about the ops-repo clone at `opsRepoPath`, not the repository the user is
editing. Say which path you mean when you report them.

| Code | Meaning | What to do |
|---|---|---|
| `BRANCH_NOT_ALLOWED` | The ops repo is on a branch the server may not write to, or on a detached HEAD. Checked **first**, before the dirty-tree check. | Do not work around this. It exists because a deploy onto an unwatched branch succeeds and never reaches the cluster. Tell the user which branch the repo is on and which are allowed (both are in `errorMessage`), and let them check out the right one. Only suggest `MCP_ALLOWED_BRANCHES` if they say writing to that branch is intended. |
| `NO_UPSTREAM` | The branch has no `origin/<branch>` ref, so nothing could be pushed. | The commit directive's `git push` would fail *after* the commit landed, leaving the change committed but unpublished. Have the user run `git push -u origin <branch>` in the ops repo, or switch to a tracked branch. |
| `DIRTY_REPO` | The working tree has uncommitted tracked changes. | Almost always an earlier mutation whose commit directive was never executed. Commit it or discard it, then retry. `errorMessage` lists the paths. |
| `BRANCH_AHEAD` | The local branch has unpushed commits, so the server refuses to pull. | `git push` to publish them, or hard-reset the branch onto its upstream to discard them. Ask before discarding. |

## Fix the call

Terminal. Retrying the identical call fails identically.

| Code | Meaning | What to do |
|---|---|---|
| `INVALID_INPUT` | Three different causes: a value failed its pattern, a required field was missing, or an **unknown field** was passed. | Read `errorMessage` before assuming a bad character. `unknown field "x" (additionalProperties: false)` means you invented an argument — each tool accepts only its own set (`deploy`: `app`/`env`/`tag`/`namespace`/`dryRun`; `promote`: `app`/`destEnv`/`srcEnv`/`namespace`/`dryRun`; `rollback`: `app`/`env`/`namespace`/`dryRun`). Patterns are `^[a-z0-9][a-z0-9-]*$` for names, `^[a-zA-Z0-9_.-]+$` for `tag`; a `tag` containing `/` means a full image reference was passed where a bare tag belongs. |
| `ENV_NOT_ALLOWED` | The env is not in `MCP_ENV_ALLOWLIST`. | Use an allowed env. Do not suggest widening the allowlist unless the user raises it — it is the guardrail on which environments this server may touch. |
| `SAME_ENV` | `promote` got the same value for `srcEnv` and `destEnv`. | Name the real source. `srcEnv` defaults to `DS_SRC_ENV`, so this usually means only `destEnv` was passed and it matches that default. |

These are enforced in the handler, not by the JSON schema. The patterns advertised in the
tool schemas are informational for the model; the handler re-checks every field and is the
actual boundary, so a malformed value reaches the server and comes back as
`INVALID_INPUT` rather than being rejected before the call.

## Fix the server config

Not caller mistakes. Report them as misconfiguration rather than retrying or rewording
arguments.

| Code | Meaning | What to do |
|---|---|---|
| `CONFIG_ERROR` | Invalid or absent configuration at startup: `MCP_ENV_ALLOWLIST` **not set**, `MCP_OPS_REPO_PATH` unset with a working directory that is **outside any git work tree**, is a **linked worktree**, or resolves to a git repo carrying **no `.ops-repo-mcp.yaml`** to mark it as an ops repo, a `pathScheme` missing `{app}` or `{env}`, a `..` in a path template, an image filename containing `/`, a missing exceptional-apps file, or an invalid `.ops-repo-mcp.yaml` (unknown key, unsupported `version`, a layout value failing the same patterns as the env var). | Fix the client's `env` block or that file. The server exits at startup on this, so you normally see a server that never connects rather than a tool result — the stderr line names the offending key. For a missing or wrong `.ops-repo-mcp.yaml`, use the `ops-init` skill: it writes one from the repo's own manifest tree and needs no working server. |
| `REPO_NOT_FOUND` | `MCP_OPS_REPO_PATH` **is** set but unusable — the path does not exist, is not a directory, or its symlinks cannot be resolved. | Point it at a real local clone. Also a startup-time exit. Unset is not an error: the working directory is used instead, and its own failures are `CONFIG_ERROR`, not this. |
| `PATH_ESCAPE` | A path the call would read or write resolved outside the ops repo root. Checked for the manifest, the history file, and `promote`'s **source** manifest, both in the pre-flight and again after the pull. | Almost always a per-app/per-env `path` in the exceptional-apps file pointing out of the tree. Surfaces per call, not at startup. Report it as a repo-configuration problem and do not retry — the server refused to read or write outside the repo, which is the correct outcome. |

## Nothing to retry

| Code | Meaning | What to do |
|---|---|---|
| `NO_CURRENT_TAG` | `promote` found no image tag in the source environment's manifest. | Deploy to `srcEnv` first, or promote from an environment that is actually live. |
| `NO_PREVIOUS_VERSION` | `rollback` found no prior tag in the manifest's git history. | The information does not exist. Ask which tag to return to and use `deploy`. |
| `MULTI_LINE_CHANGE` | The commit `rollback` would revert changed lines beyond the image tag, so the write was refused. Nothing was written. | **Not a retry-with-a-flag situation.** Show the operator the commit (`git -C <opsRepoPath> show <hash>` — the hash is in the message) and let them choose: re-call with `acknowledgeMultiLineChange: true` for a tag-only revert, or `git revert` for the whole commit. Do not set the flag on your own initiative or on the strength of an earlier "yes, roll it back". |

## Runner failures

| Code | Meaning | What to do |
|---|---|---|
| `RUNNER_FAILED` | The underlying repo-services runner returned an error, possibly after writing the manifest. | Read `runnerOutput` — it carries the runner's log lines and usually names the missing path or unparseable manifest. Check `git status` on the ops repo before retrying, so you do not stack a second write on a half-finished one. |
| `RUNNER_PANIC` | The runner panicked. The server recovered and stayed up. | A bug. Report `errorMessage` and `runnerOutput` verbatim. Inspect the manifest before retrying. |
| `RUNNER_TIMEOUT` | The runner did not return within `MCP_RUNNER_TIMEOUT`. | **The goroutine is leaked and still holds the repo lock**, so subsequent calls return `LOCK_TIMEOUT` until it finishes — and it is still running, so the file may be written after you get this response. Check the ops repo with `git -C <opsRepoPath> status` before doing anything else. Never retry into this state. |

## Server environment

Set in the MCP client's `env` block, not per call. Defaults below are the **server's** own
defaults; the values this plugin ships in `.mcp.json` differ where noted.

Every `DS_*` value can also come from a `.ops-repo-mcp.yaml` committed at the ops repo
root, under a `layout:` key using the short names (`base`, `namespace`, `pathScheme`,
`imageFileName`, `imageHistoryFileName`, `exceptionalAppsFile`, `srcEnv`). Precedence is
env var > that file > default, and the startup log line `layout_resolved` reports which
source won for each field. The `MCP_*` rows are env-only and the file cannot set them —
that is a deliberate boundary, so do not suggest moving an allowlist or a credential into
it. If a call resolves an unexpected path, the `sources` map in `layout_resolved` is the
fastest way to find out why.

| Variable | Default | Effect |
|---|---|---|
| `MCP_OPS_REPO_PATH` | _(the working directory, walked up to its git root)_ | Absolute path to the ops-repo clone. Unset means the server operates on whichever repo the client was launched in; that repo must be a git work tree with a resolvable HEAD and must carry `.ops-repo-mcp.yaml`, or startup exits with `CONFIG_ERROR`. |
| `MCP_ALLOW_ANY_CWD_REPO` | _(unset)_ | `1` waives the `.ops-repo-mcp.yaml` marker on an inferred repo. No effect when `MCP_OPS_REPO_PATH` is set. |
| `MCP_ALLOWED_BRANCHES` | _(inferred from `origin/HEAD`)_ | Comma-separated branches the tools may write to; overrides `branch:` in `.ops-repo-mcp.yaml`. When none resolves the check is skipped and startup warns. |
| `MCP_ENV_ALLOWLIST` | _(none — the server refuses to start)_ | Comma-separated envs the server may touch. Required: an empty allowlist would accept every env including production, so the server exits with `CONFIG_ERROR` unless `MCP_ALLOW_ALL_ENVS=1` is set. This plugin ships `dev,staging,prod`. |
| `MCP_ALLOW_ALL_ENVS` | _(unset)_ | `1` opts out of the allowlist entirely. If a user hits the startup `CONFIG_ERROR`, prefer helping them write an allowlist over suggesting this. |
| `MCP_LOG_LEVEL` | `info` | `debug`/`info`/`warn`/`error`. Affects stderr only. |
| `MCP_LOCK_TIMEOUT` | `30s` | How long a call waits for the per-repo lock. |
| `MCP_RUNNER_TIMEOUT` | `60s` | Wall-clock bound on a runner invocation. Raise it for a large ops repo where `git pull` is slow. |
| `MCP_GIT_SSH_KEY_PATH` | _(none)_ | SSH key for the pre-flight fetch. |
| `MCP_GIT_TOKEN` | falls back to `GITHUB_TOKEN`, but only when `MCP_OPS_REPO_PATH` is set | HTTPS token for the pre-flight fetch. The fallback is withheld from an inferred repo: the token is not scoped by host, and an inferred repo's `origin` is decided by which directory the client opened. |
| `MCP_GIT_USER` | `mcp-server` | Author name for commit objects the runner creates. |
| `MCP_GIT_EMAIL` | _(empty)_ | Author email for the same. |
| `DS_BASE` | _(empty)_ | Base folder inside the ops repo, e.g. `base/apps`. |
| `DS_NAMESPACE` | _(empty)_ | Namespace, also expanded into `DS_PATH_SCHEME`. Overridden per call by `namespace`. |
| `DS_PATH_SCHEME` | `{base}/{namespace}/{app}/{env}` | Manifest folder template. Must contain `{app}` and `{env}`; `..` is rejected. |
| `DS_IMAGE_FILE_NAME` | `deployment.yaml` | Manifest filename. No `/` or `..`. |
| `DS_IMAGE_HISTORY_FILE_NAME` | _(empty)_ | If set, the new tag is appended to this file and it joins `filesToStage` — on real calls only, never on a dry run. |
| `DS_EXCEPTIONAL_APPS_FILE` | _(empty)_ | YAML describing apps whose image name or path deviates from the scheme. Must exist on disk if set. Re-read on every call, so edits apply without a restart. The `.ops-repo-mcp.yaml` equivalent is relative to the repo root and confined to it. |
| `DS_SRC_ENV` | `staging` | Default `srcEnv` for `promote`. A value outside `^[a-z0-9][a-z0-9-]*$` only warns at startup; `promote` then returns `INVALID_INPUT` unless `srcEnv` is passed explicitly. |
| `MCP_IGNORE_REPO_CONFIG` | _(unset)_ | `1` ignores `.ops-repo-mcp.yaml` entirely. The escape hatch when a committed layout file breaks startup. |

Nothing but MCP framing is ever written to stdout — if you see protocol corruption, that is
a bug worth reporting, not a config issue.
