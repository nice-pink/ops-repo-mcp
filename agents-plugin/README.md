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
      "MCP_OPS_REPO_PATH": "${MCP_OPS_REPO_PATH}",
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

`MCP_OPS_REPO_PATH` is required and the server exits at startup without it, so export it
before starting the client:

```bash
export MCP_OPS_REPO_PATH=/absolute/path/to/your/ops-repo
```

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

## Versioning

`plugin.json` and the skills' `metadata.version` track the MCP server version they
document (currently `0.1.0`, matching `serverVersion` in
[`cmd/mcp-server/main.go`](../cmd/mcp-server/main.go)). When the tool surface or the
response shape changes, update the skills and bump the version in all six places:

```
agents-plugin/plugin.json
agents-plugin/.claude-plugin/plugin.json
agents-plugin/skills/ops-deploy/SKILL.md      # metadata.version
agents-plugin/skills/ops-promote/SKILL.md     # metadata.version
agents-plugin/skills/ops-rollback/SKILL.md    # metadata.version
.claude-plugin/marketplace.json               # plugins[0].version
```

Check them with `grep -rn '0\.1\.0' agents-plugin .claude-plugin`.
