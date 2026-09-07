# Install guide

This plugin ships three things:

- **`skills/ops-deploy`**, **`skills/ops-promote`** and **`skills/ops-rollback`** —
  instructions the agent loads when a task matches, one per tool.
- **`skills/ops-init`** — writes the `.ops-repo-mcp.yaml` an ops repo needs, deriving the
  layout from the repo's own manifest tree. It calls no tool, so it works while the server
  is still refusing to start.
- **One MCP server** — `deploy-promote`, the `mcp-server` binary from this repository, run
  over stdio. It provides the `deploy`, `promote`, and `rollback` tools.

The server is **local only**. It rewrites files in a clone of your ops repo on this
machine, so there is no hosted endpoint to point at — install the binary first, then
declare it in each client's own config format. That is why the same server appears three
times below with different syntax.

## Install the binary first

```bash
curl -fsSL https://raw.githubusercontent.com/nice-pink/ops-repo-mcp/main/install.sh | sh
```

It lands in `/usr/local/bin` when writable, else `~/.local/bin`, and prints the install
path plus a ready-to-paste `.mcp.json` snippet. `make build` or
`go install github.com/nice-pink/ops-repo-mcp/cmd/mcp-server@latest` work too. Full
options — version pinning, checksums, manual download — are in the
[repo README](https://github.com/nice-pink/ops-repo-mcp#install).

Every config below uses the bare command name `mcp-server`, which requires the install
directory to be on `PATH`. MCP clients do not always inherit your shell `PATH`; if the
server fails to start, replace `mcp-server` with the absolute path the installer printed.

Verify it runs:

```bash
mcp-server --version
```

## Configure the environment

The server reads its configuration from the `env` block of the client entry.

`MCP_OPS_REPO_PATH` says which clone to operate on, and is **optional**. Unset, the server
uses the working directory the client launched it in, walked up to the root of its git work
tree — so one installed plugin serves every ops repo you work in. Set the variable only to
pin the server to one clone regardless of where the client is opened:

```bash
export MCP_OPS_REPO_PATH=/absolute/path/to/your/ops-repo   # only to pin one clone
```

An inferred repo must carry a **`.ops-repo-mcp.yaml` at its root** — the layout file doubles
as the marker that this repository is meant to be deployed from. Without it, any ancestor
`.git` would become a deploy target, and the resolved repo is what supplies the branch
guard, the layout, and the remote the pre-flight fetch authenticates against.
`MCP_ALLOW_ANY_CWD_REPO=1` waives the marker, and says so at error level on every start
where it actually waived one. It waives nothing else: the directory must still be in a git
work tree whose HEAD resolves, which rules out a repo with no commits yet and a linked
worktree from `git worktree add`.

`GITHUB_TOKEN` is not used as a fallback for `MCP_GIT_TOKEN` on the inferred path, because
the token reaches whatever remote that repo has. Set `MCP_GIT_TOKEN` to use one there.

The declarations in this plugin pass the path through as `${MCP_OPS_REPO_PATH:-}`, so in a
client that expands it an unset variable stays unset rather than becoming a literal. A path
that *is* set but does not exist is `REPO_NOT_FOUND` at startup. `server_start` logs the
resolved `opsRepo` and its `opsRepoSource` (`MCP_OPS_REPO_PATH` or `cwd`) — read that line
before assuming the server picked the wrong repo, because whether a client passes its
session directory through is the client's behaviour, not this server's.

`MCP_ENV_ALLOWLIST` is the other one that matters. **The server refuses to start without
it**, because an empty allowlist would accept every environment including production. This
plugin ships `dev,staging,prod`, so the shipped config starts cleanly; narrow it if this
server should not reach prod. `MCP_ALLOW_ALL_ENVS=1` opts out of the allowlist entirely and
logs a warning on every start — reach for an explicit list first.

Everything else has a default, and the layout should not live here at all — see the next
section. The full environment table is in
[`skills/ops-deploy/references/errors.md`](skills/ops-deploy/references/errors.md) and the
[repo README](https://github.com/nice-pink/ops-repo-mcp#configure).

## Put the layout in the ops repo, not here

Where manifests live inside the ops repo — `base`, `pathScheme`, `imageFileName`,
namespace, exceptions, the `promote` source env — belongs in a `.ops-repo-mcp.yaml`
committed at the **ops repo** root. Copy
[`.ops-repo-mcp.yaml.example`](https://github.com/nice-pink/ops-repo-mcp/blob/main/.ops-repo-mcp.yaml.example)
there and fill it in:

```yaml
version: 1
layout:
  base: base/apps
  pathScheme: "{base}/{app}/{env}"
  imageFileName: deployment.yaml
  srcEnv: staging
```

Commit it once and every teammate and every agent session resolves the same paths with no
`DS_*` variables anywhere. Precedence per field is `DS_*` env var > that file > built-in
default; an empty env var counts as unset, which is why this plugin passes the `DS_*`
variables through with empty defaults instead of setting values that would permanently
shadow the file. Export one to override a single value:

```bash
export DS_PATH_SCHEME='{base}/{namespace}/{app}/{env}'
```

Three things to know:

- **It is read once, at startup.** Commit a change, then restart the client. The
  `exceptionalAppsFile` it points at is the exception — that is re-read on every call.
- **It is layout-only, deliberately.** It cannot set `MCP_ENV_ALLOWLIST`,
  `MCP_OPS_REPO_PATH`, the git credentials, or the timeouts, and the server exits with
  `CONFIG_ERROR` rather than ignoring a file that tries. The ops repo is a repository
  agents write to, so a pull request against it must not be able to widen the server's
  authority. An unrecognised `version` is rejected the same way.
- **Startup logs say where each value came from.** Look for `layout_resolved` on stderr;
  its `sources` map marks every field `env`, `repo-file`, or `default`.

## The `plugins` CLI

[`vercel-labs/plugins`](https://github.com/vercel-labs/plugins) installs an Agent Plugins
package into whichever agent tools it detects:

```bash
npx plugins add https://github.com/nice-pink/ops-repo-mcp
```

Point it at the repository, not at this directory — the CLI shallow-clones the repo to
`~/.cache/plugins/` and scans it. Use the full `https://github.com/...` URL. Restrict it
with `--target claude-code` and choose `--scope user|project|local`;
`npx plugins discover <url>` previews what it found without installing, and
`npx plugins targets` lists the tools it detected.

Two things to know before relying on it:

- The CLI discovers plugins under a top-level `plugins/` directory. This plugin lives in
  `agents-plugin/`, so discovery may find nothing — check with `discover` first and fall
  back to the manual routes below.
- Its Claude Code translation copies `skills/` but does not generate a `.mcp.json`, so a
  plugin that declares its server only in the standard `mcp.json` installs with skills and
  no tools. This plugin ships its own `.mcp.json` for that reason.

The manual routes need no extra tooling. They all reference a local clone:

```bash
git clone https://github.com/nice-pink/ops-repo-mcp && export OPS_PLUGIN="$PWD/ops-repo-mcp/agents-plugin"
```

`$OPS_PLUGIN` stands in for that path below. Set it again in each new shell, or substitute
the path directly.

## Which file each client reads

The plugin carries two manifests on purpose — the portable one and Claude Code's.

| File | Purpose | Claude Code | Cursor | Codex |
|---|---|---|---|---|
| `plugin.json` | [Agent Plugins 1.0.0](https://agent-plugins.org) manifest | ignored | ignored | ignored |
| `mcp.json` | Agent Plugins MCP declaration | ignored | ignored | ignored |
| `.claude-plugin/plugin.json` | Claude Code manifest | **read** | ignored | ignored |
| `.mcp.json` | Claude Code MCP declaration | **read** | ignored | ignored |
| `skills/<name>/SKILL.md` | Agent Skills | **read** | copy into `.cursor/skills/` | copy into `~/.codex/skills/` |

Claude Code does not implement the Agent Plugins standard: it reads `.mcp.json`, not
`mcp.json`. For a stdio server the two differ only in that the standard one carries a
`$schema` pointer and `"type": "stdio"` explicitly; the `env` blocks are identical. Keep
them in sync when you change the command or the `env` block.

---

## Claude Code

Two routes give you both pieces: a one-session flag for trying it, and the marketplace
this repo ships for keeping it. A third route gives you the skills only.

### Try it for one session

```bash
claude --plugin-dir "$OPS_PLUGIN"
```

The skills become `/ops-repo:ops-init`, `/ops-repo:ops-deploy`, `/ops-repo:ops-promote`
and `/ops-repo:ops-rollback`; the server registers as `plugin:ops-repo:deploy-promote`. Run `/reload-plugins` after editing a file.

### Install it permanently

Use the marketplace route in [Distribute to a team](#distribute-to-a-team) below — it
works against a local clone as well as GitHub, and it is the only permanent route that
registers the MCP server.

### Skills only, without the server

`~/.claude/skills/` holds one skill per directory, each with its own `SKILL.md` at the top
level, and is never scanned for `.mcp.json`. So copy the three skill directories
individually — **not** the plugin directory, which would nest them a level too deep and
load nothing:

```bash
for s in ops-init ops-deploy ops-promote ops-rollback; do cp -R "$OPS_PLUGIN/skills/$s" ~/.claude/skills/"$s"; done
```

Symlink instead if you want repo edits to take effect immediately. This gives you the
skills in every session with **no tools** — pair it with a project-level `.mcp.json` (next
section) or the marketplace route. Remove with
`rm -rf ~/.claude/skills/ops-{deploy,promote,rollback}`.

### Verify

```bash
claude plugin validate "$OPS_PLUGIN"
```

Then, in a session, `/help` lists the four skills under the `ops-repo` namespace and `/mcp`
shows `deploy-promote` as connected with three tools. `claude plugin validate .` at the
repo root validates the marketplace manifest instead.

### Per-project config instead of a plugin

If only one repository needs these tools, skip the plugin and copy
[`.mcp.json.example`](https://github.com/nice-pink/ops-repo-mcp/blob/main/.mcp.json.example)
to `.mcp.json` at that project's root with the paths filled in. You get the tools without
the skills — which means the agent has the `commitDirective` contract described in
`skills/ops-deploy/SKILL.md` available only through each tool's own description.

### Distribute to a team

The repo root carries a `.claude-plugin/marketplace.json` naming the marketplace
`nice-pink` and pointing at `agents-plugin/`, so teammates install straight from GitHub:

```bash
claude plugin marketplace add nice-pink/ops-repo-mcp && claude plugin install ops-repo@nice-pink
```

Add `--scope project` to the marketplace command to declare it for a repo rather than for
your user. Undo with `claude plugin marketplace remove nice-pink`, which takes the
installed plugin with it.

A local clone works as a marketplace source too, which is the quickest way to test a
change to the manifest before pushing it:

```bash
claude plugin marketplace add "$PWD" && claude plugin install ops-repo@nice-pink
```

Installing the plugin does **not** install the `mcp-server` binary. Each teammate still
needs it on `PATH` — the plugin ships the skills and the server declaration, not the
server. They do not need `MCP_OPS_REPO_PATH`: unset, each session operates on the ops repo
that session is open in, provided it carries a `.ops-repo-mcp.yaml`.

---

## Cursor

Cursor reads skills from `.cursor/skills/` (project) or `~/.cursor/skills/` (global), and
MCP servers from `.cursor/mcp.json` or `~/.cursor/mcp.json`. It does not read a plugin
directory, so point it at the two pieces separately.

### Skills

```bash
mkdir -p ~/.cursor/skills && for s in ops-init ops-deploy ops-promote ops-rollback; do ln -s "$OPS_PLUGIN/skills/$s" ~/.cursor/skills/"$s"; done
```

Use `.cursor/skills/` inside a repo instead when the skills should only apply to that
project. Cursor also accepts `.agents/skills/` and `~/.agents/skills/`.

### MCP server

Add to `~/.cursor/mcp.json`:

```json
{
  "mcpServers": {
    "deploy-promote": {
      "command": "mcp-server",
      "args": [],
      "env": {
        "MCP_ENV_ALLOWLIST": "dev,staging,prod"
      }
    }
  }
}
```

Cursor infers the transport from the presence of `command`, so there is no `type` field.
Check Settings → MCP; the server should list `deploy`, `promote`, and `rollback`.

Add `MCP_OPS_REPO_PATH` only to pin one clone, and `DS_*` entries only to override the ops
repo's `.ops-repo-mcp.yaml`; with that file committed, the one variable above is all this
entry needs.

---

## Codex

Codex reads skills from `~/.codex/skills/` (personal) or `.agents/skills/` (project, also
searched at the repo root), and MCP servers from `~/.codex/config.toml`.

### Skills

```bash
mkdir -p ~/.codex/skills && for s in ops-init ops-deploy ops-promote ops-rollback; do ln -s "$OPS_PLUGIN/skills/$s" ~/.codex/skills/"$s"; done
```

Invoke one explicitly with `$ops-deploy`, or let Codex match it against the skill's
`description`.

### MCP server

Add to `~/.codex/config.toml`:

```toml
[mcp_servers.deploy-promote]
command = "mcp-server"
args = []

[mcp_servers.deploy-promote.env]
MCP_ENV_ALLOWLIST = "dev,staging,prod"
```

Same as Cursor: add `MCP_OPS_REPO_PATH` only to pin one clone, and `DS_*` keys only to
override the ops repo's `.ops-repo-mcp.yaml`.

Verify with `codex mcp list`.

---

## Troubleshooting

| Symptom | Cause |
|---|---|
| Skills load, no `deploy` / `promote` / `rollback` tools | The server process is not starting. Run `mcp-server --version` by hand, then check the client's stderr log — the server exits at startup on `REPO_NOT_FOUND` and `CONFIG_ERROR`. |
| Server exits immediately, `CONFIG_ERROR: MCP_ENV_ALLOWLIST is empty` | No environment allowlist. Set `MCP_ENV_ALLOWLIST` to the environments this server may write to, or `MCP_ALLOW_ALL_ENVS=1` to accept every one including prod. |
| Server exits immediately, `CONFIG_ERROR: ... not inside a git work tree` | `MCP_OPS_REPO_PATH` is unset and the working directory the client launched the server in is not in a git repo. Open the client in an ops repo clone, or set the variable. |
| Server exits immediately, `CONFIG_ERROR: ... carries no .ops-repo-mcp.yaml` | The inferred repo is not marked as an ops repo. Run the `ops-init` skill to write one, or set `MCP_OPS_REPO_PATH`, or set `MCP_ALLOW_ANY_CWD_REPO=1`. |
| Server exits immediately, `CONFIG_ERROR: ... git HEAD cannot be resolved` | Either the repo has no commits yet — make one — or the client was launched in a linked worktree (`git worktree add`), whose refs the runner cannot read; use the main checkout instead. |
| `PULL_FAILED` against a private remote that used to work | `GITHUB_TOKEN` is no longer adopted when the ops repo is inferred from the working directory. Set `MCP_GIT_TOKEN`, or set `MCP_OPS_REPO_PATH`. |
| Server exits immediately, `REPO_NOT_FOUND` | The variable *is* set, but the path does not exist, is not a directory, or its symlinks cannot be resolved. |
| Tools work but write to the wrong repo | `MCP_OPS_REPO_PATH` is unset and the client launched the server somewhere other than the repo you expected. Check `opsRepo` / `opsRepoSource` in the `server_start` stderr line, then set the variable to pin it. |
| `spawn mcp-server ENOENT` | The install directory is not on the `PATH` the client sees. Use the absolute path the installer printed. |
| Nothing loads in Claude Code | Wrong directory passed to `--plugin-dir`. It must be `agents-plugin`, the directory holding `skills/`. |
| Installed via `npx plugins add`, skills work, no tools | The CLI did not pick up `.mcp.json`. Declare the server manually per the routes above. |
| Every call returns `DIRTY_REPO` | A previous mutation was never committed. The server refuses to work on a dirty tree. `git -C <ops-repo> status` and either commit or discard. |
| Every call returns `LOCK_TIMEOUT` | A leaked runner goroutine holds the per-repo lock after a `RUNNER_TIMEOUT`. Restart the server. |
| Paths resolve wrong, or `PATH_ESCAPE` | The resolved `pathScheme` does not match the ops repo's layout. Check the `layout_resolved` line on stderr: its `sources` map says whether the value came from `env`, `repo-file`, or `default`. A stale `DS_PATH_SCHEME` exported in your shell silently outranks the committed file. |
| Edited `.ops-repo-mcp.yaml`, nothing changed | It is read once at startup. Restart the client. |
| `CONFIG_ERROR: ... field X not found in type main.repoConfig` | `.ops-repo-mcp.yaml` has an unknown key. It accepts only `version` and `layout`; operational settings stay in the client's `env` block by design. |
| Tools work, but the manifest change is never pushed | Expected: the server never commits. The agent has to execute `commitDirective.gitCommands`. Load the `ops-deploy` skill so it knows to. |
