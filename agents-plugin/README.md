# ops-repo — agent plugin

An [Agent Plugin](https://agent-plugins.org) (spec 1.0.0) that gives any supporting agent
the `mcp-server` binary from this repository: change an application's container image tag
in a GitOps ops repo, then commit the change deliberately.

## Contents

```
agents-plugin/
├── plugin.json                              # Agent Plugins manifest
├── mcp.json                                 # Agent Plugins MCP declaration
├── .claude-plugin/plugin.json               # Claude Code manifest
├── .mcp.json                                # Claude Code MCP declaration
├── MANUAL.md                                # install guide: Claude Code, Cursor, Codex
└── skills/
    ├── ops-init/
    │   └── SKILL.md                         # write .ops-repo-mcp.yaml from the repo's own tree
    ├── ops-deploy/
    │   ├── SKILL.md                         # deploy a named tag; the shared commit directive
    │   └── references/errors.md             # error codes, retry rules, server env
    ├── ops-promote/
    │   └── SKILL.md                         # copy the live tag from one env into another
    └── ops-rollback/
        └── SKILL.md                         # rollback, multi-line-change warning
```

`plugin.json` and `mcp.json` validate against the 1.0.0 schemas
([plugin](https://agent-plugins.org/schemas/1.0.0/plugin.schema.json),
[mcp](https://agent-plugins.org/schemas/1.0.0/mcp.schema.json)). The skills follow the
[Agent Skills](https://agentskills.io/specification) spec.

The `.claude-plugin/` and `.mcp.json` files are Claude Code's own format, which is not the
Agent Plugins standard — Claude Code reads `.mcp.json`, never `mcp.json`. Both MCP
declarations describe the same stdio server; keep them in sync. Install steps for each
client are in [MANUAL.md](MANUAL.md) — the short version for Claude Code is
`claude plugin marketplace add nice-pink/ops-repo-mcp`, served by
[`.claude-plugin/marketplace.json`](../.claude-plugin/marketplace.json) at the repo root,
which points back at this directory.

## MCP server

The server is local by design: it rewrites files in a clone of your ops repo on this
machine, so there is nothing hosted to point at. Install the binary
(`curl -fsSL .../install.sh | sh`, `make build`, or `go install`), then declare it:

```json
{
  "deploy-promote": {
    "type": "stdio",
    "command": "mcp-server",
    "args": [],
    "env": {
      "MCP_ENV_ALLOWLIST": "dev,staging,prod"
    }
  }
}
```

That is the whole entry when the ops repo carries a `.ops-repo-mcp.yaml` (see
below). The shipped declarations also pass every `DS_*` variable through from
your environment, so you can still override any single layout value without
editing the plugin.

There is no authentication and no network service. The only credentials involved are
`MCP_GIT_SSH_KEY_PATH` / `MCP_GIT_TOKEN`, which the server uses for the `git fetch` it
runs against the ops repo's remote before every change.

### Which repo it operates on

With `MCP_OPS_REPO_PATH` unset, the server uses the working directory the client launched it
in, walked up to the root of its git work tree. One installed plugin therefore serves every
ops repo you work in — the repo is whichever one the client is open in. The shipped
declarations pass the variable through as `${MCP_OPS_REPO_PATH:-}` (in clients that expand
it), so exporting it pins the server to one clone and leaving it unset keeps the
per-directory behaviour.

```bash
export MCP_OPS_REPO_PATH=/absolute/path/to/your/ops-repo   # only to pin one clone
```

An inferred repo has to earn it, because a git repo is not a statement that anyone wants to
deploy from it — and the resolved repo supplies the branch guard, the layout, and the remote
the pre-flight fetch authenticates against:

- It must carry a **`.ops-repo-mcp.yaml` at its root**, which doubles as the ops-repo
  marker. `MCP_ALLOW_ANY_CWD_REPO=1` waives that and accepts any git repo, with a warning
  on every start.
- HEAD must resolve. That refuses a repo with no commits yet, and a linked worktree from
  `git worktree add`, whose refs live in the main checkout's commondir where the runner
  cannot read them.
- `GITHUB_TOKEN` is not adopted as a fallback for `MCP_GIT_TOKEN` here. The token is
  attached to the pre-flight fetch unscoped by host, and an inferred repo's `origin` is
  chosen by which directory the client opened. Set `MCP_GIT_TOKEN` to use one deliberately.

Startup exits with `CONFIG_ERROR` naming the directory when any of that fails. The
`server_start` log line carries `opsRepo` and `opsRepoSource` — read it first when a deploy
lands in the wrong repo, since whether the client passes its session directory through is
the client's behaviour.

### Where the layout comes from

The repo's manifest layout — `base`, `pathScheme`, `imageFileName`, namespace,
exceptions, the promote source env — belongs in a `.ops-repo-mcp.yaml` committed at the
**ops repo** root, not in this plugin's `env` block. That is the point: the layout is a
property of that repository, so it travels with it through git instead of being re-entered
on every machine.

```yaml
version: 1
layout:
  base: base/apps
  pathScheme: "{base}/{app}/{env}"
  imageFileName: deployment.yaml
  srcEnv: staging
```

Precedence per field is `DS_*` env var > that file > built-in default, and an empty env var
counts as unset — which is why the shipped declarations pass the `DS_*` variables through
with empty defaults rather than setting values that would permanently shadow the file.

The file is layout-only and cannot set `MCP_ENV_ALLOWLIST`, the git credentials, or the
timeouts; the server exits with `CONFIG_ERROR` if it tries. The ops repo is a repository
agents write to, so a pull request against it must not be able to widen the server's
authority.

The full environment table is in the
[repo README](https://github.com/nice-pink/ops-repo-mcp#configure) and in
[`skills/ops-deploy/references/errors.md`](skills/ops-deploy/references/errors.md).

## Tools the plugin exposes

| Tool | What it does |
|---|---|
| `deploy` | Write `tag` into `app`'s manifest in `env`. |
| `promote` | Copy the tag live in `srcEnv` into `destEnv`, resolved under the lock. |
| `rollback` | Restore the tag `app` ran before the last commit touching its manifest, read from git history. |

All three take `app`, an optional `namespace`, and an optional `dryRun`. All three share
the same input patterns (`^[a-z0-9][a-z0-9-]*$` for names, `^[a-zA-Z0-9_.-]+$` for tags),
the same pre-flight (refuse a dirty tree, refuse a branch ahead of upstream, then
fast-forward), and the same per-repo lock.

### The contract the skills exist for

The server **mutates files but never commits or pushes**. Every successful response
carries a `commitDirective` — `filesToStage`, `suggestedCommitMessage`, `gitCommands` —
that the calling agent is expected to execute, plus a `recoveryHint` for backing out.

An agent that ignores this leaves the ops repo dirty, which blocks the next call with
`DIRTY_REPO`. That is the main thing `skills/ops-deploy/SKILL.md` teaches, and the other
two skills point back at it.

The rest is per-tool judgement: `ops-promote` on resolving the source tag under the lock
rather than reading it and calling `deploy`, and `ops-rollback` on the `multiLineChange`
warning, which has to reach the operator before a revert lands an old image next to new
configuration.

`ops-init` is the odd one out: it writes `.ops-repo-mcp.yaml` and calls no tool. That is
deliberate. The server exits at startup on a missing marker or an invalid layout, so the
moment the file is needed is exactly the moment `deploy`, `promote` and `rollback` are not
registered. The skill derives the layout by reading the repo's manifest tree and confirming
it, rather than asking a user to recite a path scheme they may never have written down.

## Versioning

`plugin.json` and the skills' `metadata.version` track the MCP server version they
document (currently `0.1.0`). That is a released server version, i.e. a `v<N>` release
tag minus its `v` — not the `serverVersion` default in
[`cmd/mcp-server/main.go`](../cmd/mcp-server/main.go), which is `dev` and only labels
unreleased local builds. When the tool surface or the response shape changes, update
the skills and bump the version in all seven places:

```
agents-plugin/plugin.json
agents-plugin/.claude-plugin/plugin.json
agents-plugin/skills/ops-init/SKILL.md        # metadata.version
agents-plugin/skills/ops-deploy/SKILL.md      # metadata.version
agents-plugin/skills/ops-promote/SKILL.md     # metadata.version
agents-plugin/skills/ops-rollback/SKILL.md    # metadata.version
.claude-plugin/marketplace.json               # plugins[0].version
```

Check them with `grep -rn '0\.1\.0' agents-plugin .claude-plugin`.
