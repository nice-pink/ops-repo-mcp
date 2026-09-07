---
name: ops-rollback
description: Roll an application in a GitOps ops repository back to the image tag it ran before the most recent change to its manifest. Use when the user says "roll back <app>", "revert prod", "undo the last deploy", "go back to the previous version", or reports a bad release. The previous tag is read from git history, so no tag argument is needed. Covers the multi-line-change warning that must be surfaced before committing.
compatibility: Requires the deploy-promote MCP server from this plugin, running on the same machine as a local clone of the ops repo.
metadata:
  author: nice-pink
  version: "0.1.0"
---

# Roll back in the ops repo

`rollback` takes `app` and `env` — no tag. It finds the most recent commit that touched
that app's manifest, reads the image tag from that commit's **parent**, and writes it back.

That is a tag-level revert of one manifest, not `git revert`. Nothing else in the commit
is undone, and no history is rewritten. Like `deploy` and `promote`, the server mutates
the file and returns a `commitDirective` for you to execute.

Optional arguments: `namespace` (overrides `DS_NAMESPACE`) and `dryRun` (resolves the
previous tag and computes the warning below without writing).

## Always dry-run first

A rollback is a reaction to something already going wrong, which is exactly when a second
wrong change is most expensive. Call `rollback` with `dryRun: true`, then tell the user:

- `currentTag` → `previousTag` — what is live, and what it becomes.
- `lastCommitMessage` and `lastCommit` — the change being undone.
- `multiLineChange` — see below.

Then ask for confirmation before the real call. If the user has already named the tag they
want, use `deploy` instead; it is one step and does not depend on history being clean.

## The multi-line-change warning

`multiLineChange: true` means the commit being rolled back edited lines in the manifest
beyond the image tag — replica counts, resource limits, env vars, an added container.
`nonTagLineChanges` counts them and `warning` carries the human-readable text.

One false positive to recognise: if `currentTag` comes back as `""` the server cannot
diff tag lines, so it counts every differing line instead and `multiLineChange` is `true`
even for a tag-only change. An empty `currentTag` alongside a suspiciously high
`nonTagLineChanges` means "could not read the current tag", not "this commit changed a
lot" — say so rather than reporting a large blast radius.

**Surface this to the user before running the commit directive.** Reverting the tag alone
leaves those other edits in place, so the environment ends up in a combination that has
never run anywhere: old image, new configuration. Show the operator the commit
(`git -C <opsRepoPath> show <lastCommit>`) and let them decide between the tag-only
rollback and reverting the whole commit by hand.

Do not commit a `multiLineChange: true` rollback on the strength of an earlier "yes, roll
it back". The user agreed to a rollback, not to this one's blast radius.

## Response fields specific to rollback

```json
{
  "success": true,
  "dryRun": false,
  "app": "poma-mcp",
  "env": "prod",
  "currentTag": "v0.1.4",
  "previousTag": "v0.1.3",
  "lastCommit": "9812432...",
  "lastCommitMessage": "Deploy poma-mcp(prod) version: v0.1.4",
  "parentCommit": "1a2b3c4...",
  "multiLineChange": false,
  "nonTagLineChanges": 0,
  "opsRepoPath": "/abs/path/to/ops-repo",
  "commitDirective": { "action": "git-commit-and-push", "filesToStage": ["..."], "suggestedCommitMessage": "Rollback poma-mcp(prod) to version: v0.1.3", "gitCommands": ["..."] },
  "recoveryHint": { "detectCommand": "...", "discardCommand": "...", "completeCommand": "..." },
  "runnerOutput": "<runner log lines>"
}
```

Execute `gitCommands` in order and stage only `filesToStage`, as described in the
`ops-deploy` skill. Never leave the mutation uncommitted — a dirty ops repo blocks the
next call with `DIRTY_REPO`, and a half-done rollback is the worst state to page someone
into. Discard with `recoveryHint.discardCommand` if the user backs out.

## `NO_PREVIOUS_VERSION`

The manifest has no prior tag in history — it was added in a single commit, or the tag
could not be extracted from the parent. Nothing to retry: the information does not exist.
Ask the user which tag they want and call `deploy` with it.

Every other error code, and the pre-flight checks that produce `DIRTY_REPO` /
`BRANCH_AHEAD` / `LOCK_TIMEOUT`, are in
[`../ops-deploy/references/errors.md`](../ops-deploy/references/errors.md).
