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

Nothing was written for any code in the "Retry unchanged", "Fix the ops repo", "Fix the
call", or "Fix the server config" groups below. Those all return before the runner is
invoked, so there is no partial state — except the pre-existing mess `DIRTY_REPO` is
reporting, which an earlier call left behind.

`RUNNER_FAILED` and `RUNNER_TIMEOUT` are different: the runner had already started, so the
manifest may be modified on disk. **Check `git -C <opsRepoPath> status` before deciding
what to do next**, and do not report the operation as a clean no-op.

## Retry unchanged

| Code | Meaning | What to do |
|---|---|---|
| `LOCK_TIMEOUT` | This call waited longer than `MCP_LOCK_TIMEOUT` for the per-repo lock and gave up. The timeout bounds *your* wait, not the holder's runtime. | Retry once. If it recurs, a leaked runner goroutine is holding the lock (see `RUNNER_TIMEOUT`) — restart the server. |
| `PULL_FAILED` | The pre-flight failed before any write. Either the `git fetch` / fast-forward against the remote failed, or the repo could not be opened or inspected at all. | Check the `errorMessage` prefix. `open:`, `status:`, or `ahead-check:` means `MCP_OPS_REPO_PATH` is not a usable git clone — fix the path, do not retry. An unprefixed message is the remote: retry once for a transient network error, otherwise check `MCP_GIT_SSH_KEY_PATH` / `MCP_GIT_TOKEN`. |

## Fix the ops repo, then retry

These are about the ops-repo clone at `opsRepoPath`, not the repository the user is
editing. Say which path you mean when you report them.

| Code | Meaning | What to do |
|---|---|---|
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
| `CONFIG_ERROR` | Invalid or absent configuration at startup: `MCP_OPS_REPO_PATH` **not set**, a `pathScheme` missing `{app}` or `{env}`, a `..` in a path template, an image filename containing `/`, a missing exceptional-apps file, or an invalid `.ops-repo-mcp.yaml` (unknown key, unsupported `version`, a layout value failing the same patterns as the env var). | Fix the client's `env` block or that file. The server exits at startup on this, so you normally see a server that never connects rather than a tool result — the stderr line names the offending key. |
| `REPO_NOT_FOUND` | `MCP_OPS_REPO_PATH` **is** set but unusable — the path does not exist, is not a directory, or its symlinks cannot be resolved. | Point it at a real local clone. Also a startup-time exit. An unset variable is `CONFIG_ERROR`, not this. |
| `PATH_ESCAPE` | A path the call would read or write resolved outside the ops repo root. Checked for the manifest, the history file, and `promote`'s **source** manifest, both in the pre-flight and again after the pull. | Almost always a per-app/per-env `path` in the exceptional-apps file pointing out of the tree. Surfaces per call, not at startup. Report it as a repo-configuration problem and do not retry — the server refused to read or write outside the repo, which is the correct outcome. |

## Nothing to retry

| Code | Meaning | What to do |
|---|---|---|
| `NO_CURRENT_TAG` | `promote` found no image tag in the source environment's manifest. | Deploy to `srcEnv` first, or promote from an environment that is actually live. |
| `NO_PREVIOUS_VERSION` | `rollback` found no prior tag in the manifest's git history. | The information does not exist. Ask which tag to return to and use `deploy`. |

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
| `MCP_OPS_REPO_PATH` | _(required)_ | Absolute path to the ops-repo clone. Unset is `CONFIG_ERROR` at startup. |
| `MCP_ENV_ALLOWLIST` | _(empty — every env accepted)_ | Comma-separated envs the server will touch. The server logs a startup warning when empty. This plugin ships `dev,staging,prod` instead, deliberately. |
| `MCP_LOG_LEVEL` | `info` | `debug`/`info`/`warn`/`error`. Affects stderr only. |
| `MCP_LOCK_TIMEOUT` | `30s` | How long a call waits for the per-repo lock. |
| `MCP_RUNNER_TIMEOUT` | `60s` | Wall-clock bound on a runner invocation. Raise it for a large ops repo where `git pull` is slow. |
| `MCP_GIT_SSH_KEY_PATH` | _(none)_ | SSH key for the pre-flight fetch. |
| `MCP_GIT_TOKEN` | falls back to `GITHUB_TOKEN` | HTTPS token for the same. |
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
