---
name: ops-deploy
description: Set an application's container image tag to a specific version in a GitOps ops repository. Use when the user names a tag to roll out — "deploy <app> <tag> to <env>", "ship v1.2.3", "bump the image tag", "set staging to this build" — or asks what tag an app currently runs. Also documents the git commit directive every tool in this plugin returns and the pre-flight checks that can block any change. For copying a tag between environments use ops-promote; for reverting use ops-rollback.
compatibility: Requires the deploy-promote MCP server from this plugin, running on the same machine as a local clone of the ops repo.
metadata:
  author: nice-pink
  version: "0.1.0"
---

# Deploy a tag in the ops repo

The server rewrites the image tag inside a manifest YAML in a local ops-repo clone. It
**never commits and never pushes**. Every successful response carries a
`commitDirective` — executing it is your job, not the server's.

That split is the point: the operator sees the diff before it is published, and git
history stays the audit trail.

## Pick the tool

| The user wants | Tool | Required arguments |
|---|---|---|
| A specific tag live in an environment | `deploy` | `app`, `env`, `tag` |
| Whatever is live in A to also be live in B | `promote` | see the `ops-promote` skill |
| To undo the last change to a manifest | `rollback` | see the `ops-rollback` skill |

If the user has not named a version, `deploy` is probably the wrong tool. "Ship staging to
prod" is a `promote`; "go back to the previous version" is a `rollback`. Never read a tag
out of one environment and hand it to `deploy` — `promote` does that read under the same
lock as the write, so it cannot pick up a tag that changed in between.

`deploy` also accepts:

- `namespace` — overrides the server's `DS_NAMESPACE`. Only needed when the path scheme
  includes `{namespace}` and the app lives outside the default one.
- `dryRun` — runs the pre-flight (including the `git pull`) and reports the paths that
  would change, without touching a file. `commitDirective.action` comes back as
  `dry-run-preview` with an empty `gitCommands`, and `recoveryHint` is absent.

## Confirm before mutating

Deploying is an outward-facing change to a running environment. Before the first
non-dry-run call in a conversation:

1. Say which app, which tag, and which environment you are about to change.
2. For anything the user described as production, prefer `dryRun: true` first and show
   the resolved paths and tag.
3. Ask for confirmation. Approval for `staging` is not approval for `prod`.

Once the user has approved a specific change, execute it — including the commit — without
asking again for that same change.

## Executing the commit directive

A successful mutation returns:

```json
{
  "success": true,
  "dryRun": false,
  "app": "poma-mcp",
  "env": "prod",
  "tag": "v0.1.4",
  "opsRepoPath": "/abs/path/to/ops-repo",
  "commitDirective": {
    "action": "git-commit-and-push",
    "filesToStage": ["base/apps/poma-mcp/prod/deployment.yaml"],
    "suggestedCommitMessage": "Deploy poma-mcp(prod) version: v0.1.4",
    "gitCommands": ["git -C /abs/path add -- ...", "git -C /abs/path commit -m ...", "git -C /abs/path push"]
  },
  "recoveryHint": { "detectCommand": "...", "discardCommand": "...", "completeCommand": "..." },
  "runnerOutput": "<runner log lines>"
}
```

Run `gitCommands` in order, from any directory — each one carries its own `git -C`. Stage
only the paths in `filesToStage`; the ops repo may hold unrelated work you must not sweep
in. Keep `suggestedCommitMessage` unless the user asked for different wording: the
`Deploy <app>(<env>) version: <tag>` shape is what makes the history greppable.

On a real call, `filesToStage` gains a second entry when `DS_IMAGE_HISTORY_FILE_NAME` is
configured and that file exists. A **dry run never lists it**, even when it is configured,
so a dry-run preview legitimately shows one path fewer than the call it previews. Do not
report that as a discrepancy.

**Leaving the mutation uncommitted is worse than not making it.** An uncommitted manifest
change makes the working tree dirty, and the next call — yours or anyone else's — fails
with `DIRTY_REPO`. If the user declines the commit, offer `recoveryHint.discardCommand`.

## Pre-flight checks that can block you

Inside the per-repo lock, before any write, the server checks in order: the ops repo is on
an allowed branch, that branch has an upstream, the working tree is clean, the branch is
not ahead of upstream — then it fast-forwards. So:

- `BRANCH_NOT_ALLOWED` and `NO_UPSTREAM` mean the change could not have reached the
  cluster from where the repo currently sits. **Never route around them** by suggesting a
  different branch or a config override — surface which branch the repo is on and let the
  operator move it. A deploy onto an unwatched branch is the one failure that otherwise
  reports success.
- `DIRTY_REPO` and `BRANCH_AHEAD` are about the **ops repo**, not the repo you are working
  in. Report the path from `opsRepoPath`.
- A `dryRun` still pulls. It is not read-only with respect to the clone's git state.
- Calls serialize on one lock per repo. A second call waits up to `MCP_LOCK_TIMEOUT`, then
  returns `LOCK_TIMEOUT` — that is contention, not failure. Retry once.

Full error table, including which codes are worth retrying:
[`references/errors.md`](references/errors.md).

## Argument validation

The handler re-checks every field and is the real boundary — the patterns in the tool
schemas are informational, so a bad value reaches the server and comes back as
`INVALID_INPUT`. Fix the value rather than retrying:

| Argument | Pattern |
|---|---|
| `app`, `env`, `srcEnv`, `destEnv`, `namespace` | `^[a-z0-9][a-z0-9-]*$` — lowercase, no underscores, no slashes |
| `tag` | `^[a-zA-Z0-9_.-]+$` — no `/`, so `registry/image:tag` is not a tag |

`INVALID_INPUT` also covers a missing required field and an **unknown** one: each tool
rejects any argument outside its own set (`additionalProperties: false`), so an invented
`environment`, `image`, or `force` fails here rather than being ignored. Read
`errorMessage` before hunting for a bad character.

`env` must also pass `MCP_ENV_ALLOWLIST` if the server sets one (`ENV_NOT_ALLOWED`).

A failed call is not an MCP protocol error — `isError` is never set, so check the `success`
field on every response. Full table: [`references/errors.md`](references/errors.md).
