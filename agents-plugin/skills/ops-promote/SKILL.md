---
name: ops-promote
description: Copy an application's live container image tag from one environment to another in a GitOps ops repository — read what is running in a source environment and write that same tag into a destination environment of the same app. Use when the user says "promote <app> to prod", "ship what's in staging to production", "put the same version in prod as staging", "promote the current dev build", "match prod to staging", or asks which tag an environment currently runs. No tag argument is needed; the tag is read from the source environment's manifest.
compatibility: Requires the deploy-promote MCP server from this plugin, running on the same machine as a local clone of the ops repo.
metadata:
  author: nice-pink
  version: "0.1.0"
---

# Promote between environments

`promote` takes `app` and `destEnv` — **no tag**. It reads the image tag currently written
in the source environment's manifest and writes that same tag into the destination
environment's manifest for the same app.

Only the destination file is modified. The source environment is read and left alone, so a
promote can never disturb what it is copying from.

Like `deploy` and `rollback`, the server mutates the file and **never commits or pushes**.
The response carries a `commitDirective` you are expected to execute.

## Use promote, not deploy, to move a version between environments

Resolve nothing by hand. It is tempting to read the source tag, then call `deploy` with it —
don't. `promote` resolves the source tag and writes the destination **under the same
per-repo lock**, so it cannot pick up a tag that changed in between. Splitting it into two
calls opens exactly that window, and gives you two chances to typo a version.

Use `deploy` only when the user names a specific tag. Use `promote` whenever the intent is
"make B run what A runs".

## Arguments

| Argument | Required | Notes |
|---|---|---|
| `app` | yes | Application name. `^[a-z0-9][a-z0-9-]*$` |
| `destEnv` | yes | The environment to promote **INTO**. Must differ from `srcEnv`. |
| `srcEnv` | no | The environment to promote **FROM**. See the default below. |
| `namespace` | no | Overrides the server's namespace. Only needed when the path scheme includes `{namespace}` and the app lives outside the default one. |
| `dryRun` | no | Resolve and report the source tag and the destination path without writing. |

Passing any other argument fails with `INVALID_INPUT` — the tool accepts only these five.

### Where `srcEnv` comes from when you omit it

In order: the server's `DS_SRC_ENV` env var, then `layout.srcEnv` in the ops repo's
`.ops-repo-mcp.yaml`, then the built-in default `staging`.

**Say which source you are using.** "Promote poma-mcp to prod" does not name a source, and
the configured default may not be what the user pictures. Either state it
("promoting from staging, the configured default") or pass `srcEnv` explicitly. When the
default happens to equal `destEnv`, the call fails with `SAME_ENV` rather than doing
anything — that error usually means only `destEnv` was passed.

## Dry-run, then confirm

A promote changes what a running environment serves. Before the first non-dry-run call:

1. Call with `dryRun: true`. The response's `resolvedTag` is the tag that would be written
   and `commitDirective.filesToStage` is the file that would change.
2. Tell the user the actual version and both environments: "staging runs `v0.1.4`;
   promoting that into prod".
3. Ask for confirmation. Approval to promote into `staging` is not approval for `prod`.

Once a specific promote is approved, carry it out — including the commit — without asking
again for that same change.

A dry run still runs the pre-flight `git pull`, so it is not read-only with respect to the
ops repo's git state.

## Response shape

`promote` uses `srcEnv` / `destEnv` / `resolvedTag` where `deploy` uses `env` / `tag`:

```json
{
  "success": true,
  "dryRun": false,
  "app": "test-app",
  "srcEnv": "dev",
  "destEnv": "prod",
  "resolvedTag": "v0.0.1",
  "opsRepoPath": "/abs/path/to/ops-repo",
  "commitDirective": {
    "action": "git-commit-and-push",
    "filesToStage": ["base/resources/test/test-app/prod/deployment.yaml"],
    "suggestedCommitMessage": "Promote test-app(prod) version: v0.0.1",
    "gitCommands": ["git -C /abs/path add -- ...", "git -C /abs/path commit -m ...", "git -C /abs/path push"]
  },
  "recoveryHint": { "detectCommand": "...", "discardCommand": "...", "completeCommand": "..." },
  "runnerOutput": "<runner log lines>"
}
```

`suggestedCommitMessage` names the **destination**: `Promote <app>(<destEnv>) version: <tag>`.

On a dry run, `action` is `dry-run-preview`, `gitCommands` is empty, and `recoveryHint` is
absent. A dry run also never lists the history file in `filesToStage` even when
`imageHistoryFileName` is configured, so a preview can legitimately show one path fewer
than the real call — that is not a discrepancy.

## Executing the commit directive

Run `gitCommands` in order; each carries its own `git -C`, so the working directory does
not matter. Stage only the paths in `filesToStage` — the ops repo may hold unrelated work
you must not sweep in. Keep `suggestedCommitMessage` unless the user asked for different
wording; the fixed shape is what makes the history greppable.

**Never leave the mutation uncommitted.** A modified manifest makes the ops repo dirty, and
the next call — yours or a teammate's — fails with `DIRTY_REPO`. If the user backs out,
run `recoveryHint.discardCommand`.

## Errors specific to promote

| Code | Meaning | What to do |
|---|---|---|
| `SAME_ENV` | `srcEnv` and `destEnv` are the same, so the promote would be a no-op. Checked **before** the environment allowlist. | Name the real source. Usually only `destEnv` was passed and it matches the configured `srcEnv` default. |
| `NO_CURRENT_TAG` | The source environment's manifest has no image tag — `cannot promote <app> from <env> — manifest has no current tag`. | Nothing to copy. Deploy to the source first, or promote from an environment that is actually live. Do not substitute a tag you guessed. |
| `ENV_NOT_ALLOWED` | Either environment is outside `MCP_ENV_ALLOWLIST`. **Both** `destEnv` and `srcEnv` are checked. | Use permitted environments. Do not suggest widening the allowlist unless the user raises it. |
| `PATH_ESCAPE` | A resolved path lands outside the ops repo. For promote this covers the **source** manifest as well as the destination. | A repo-configuration problem, almost always an exceptional-apps entry pointing out of the tree. Do not retry: the server correctly refused to read or write outside the repo. |

A failed call is not an MCP protocol error — `isError` is never set, so check the `success`
field on every response. The shared codes (`BRANCH_NOT_ALLOWED`, `NO_UPSTREAM`,
`DIRTY_REPO`, `BRANCH_AHEAD`, `LOCK_TIMEOUT`, `RUNNER_*`) and the pre-flight behaviour
behind them are in
[`../ops-deploy/references/errors.md`](../ops-deploy/references/errors.md).

## One asymmetry worth knowing

`promote` copies whatever string sits in the source manifest, and that string is not
checked against the `tag` pattern that `deploy` enforces. A source tag containing a slash
promotes cleanly:

```
promote srcEnv=prod destEnv=dev   →  resolvedTag "v0.0.1/beta", success
deploy  env=dev tag=v0.0.1/beta   →  INVALID_INPUT, does not match ^[a-zA-Z0-9_.-]+$
```

So do not "fall back" to `deploy` with a tag that `promote` reported — it can be rejected
even though the promote would have worked. If a promote fails and the user wants that exact
version deployed instead, tell them the tag is not one this server will accept as a
`deploy` argument.
