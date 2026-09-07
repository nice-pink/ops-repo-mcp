# ops-repo-mcp

MCP server that drives image-tag changes against a GitOps / ops repository.
It exposes the deploy / promote / rollback flows over the Model Context
Protocol so an LLM client (Claude Code, Cursor, etc.) can operate on an ops
repo. The server speaks stdio and is launched per session by the client.

The server **mutates files but never commits or pushes**. Every successful
response includes a `commitDirective` (filesToStage, suggestedCommitMessage,
gitCommands) that the calling LLM is expected to execute. This keeps the
human-visible audit trail in git history and lets the operator inspect the
diff before publishing.

## Relationship to repo-services

The deploy / promote / rollback logic itself lives in
[nice-pink/repo-services](https://github.com/nice-pink/repo-services) and is
consumed here as a Go module dependency — this repo is the MCP transport,
config, locking, and response layer around it:

```
github.com/nice-pink/repo-services/pkg/runner       deploy / promote
github.com/nice-pink/repo-services/pkg/manifest     manifest read / rewrite
github.com/nice-pink/repo-services/pkg/util         flags, repo handle
github.com/nice-pink/repo-services/pkg/exceptional  per-app path overrides
```

`repo-services` is pinned in `go.mod`. It carries `v*` tags for its container
images which are **not** usable as Go module versions (`v2`+ without a
matching `/vN` module path), so the pin is a pseudo-version against a commit.
To move to a newer repo-services:

```
go get github.com/nice-pink/repo-services@<commit-or-tag>
go mod tidy
go test ./...
```

`examples/` is a copy of the repo-services fixture tree. It is what
`cmd/mcp-server/stdout_safety_test.go` runs against — that test asserts the
runner writes nothing to stdout, because any stdout write corrupts the MCP
JSON-RPC framing. If `examples/repo` goes missing the test **skips** rather
than fails, so keep it in place.

## Install

### Prebuilt binary (recommended)

Prebuilt binaries are attached to every `v*` release for macOS
(Intel + Apple Silicon) and Linux (amd64 + arm64). The install script detects
your platform, verifies the SHA-256 checksum, and drops the binary on disk:

```
curl -fsSL https://raw.githubusercontent.com/nice-pink/ops-repo-mcp/main/install.sh | sh
```

It installs to `/usr/local/bin` when that is writable, otherwise
`~/.local/bin`. Nothing is run with `sudo` on your behalf — if you want a
root-owned install, do it explicitly:

```
curl -fsSL https://raw.githubusercontent.com/nice-pink/ops-repo-mcp/main/install.sh -o install.sh
sudo env INSTALL_DIR=/usr/local/bin sh install.sh
```

On success the script prints the install path, the output of
`mcp-server --version`, and a ready-to-paste `.mcp.json` snippet with the
correct `command` path filled in.

| Variable | Default | Description |
|----------|---------|-------------|
| `INSTALL_DIR` | `/usr/local/bin` if writable, else `~/.local/bin` | Target directory. Created if missing. |
| `VERSION` | latest `v*` release | Pin a specific release tag, e.g. `v2`. Drafts and prereleases are never auto-selected; a prerelease can be pinned explicitly, a draft cannot be installed at all (no tag, no public download path). |
| `GITHUB_TOKEN` | _(none)_ | Sent only to the GitHub API, to lift the unauthenticated rate limit when resolving the latest release. Never sent with the asset download. |
| `SKIP_CHECKSUM` | `0` | Set to `1` to install without verifying the SHA-256. Only useful if `checksums.txt` is missing from a release or no `sha256sum`/`shasum` is available. |

Downgrading or pinning:

```
curl -fsSL https://raw.githubusercontent.com/nice-pink/ops-repo-mcp/main/install.sh | VERSION=v2 sh
```

To uninstall, delete the binary (`rm "$(command -v mcp-server)"`). The script
writes nothing else — no config, no shell-profile edits.

Requirements: `curl` or `wget`, plus `tar`, `awk`, `grep`, `mktemp`, `uname`,
and `sha256sum` or `shasum`. Supported
platforms are `darwin/amd64`, `darwin/arm64`, `linux/amd64`, `linux/arm64`;
anything else exits with an error rather than installing the wrong binary. A
failed or missing checksum aborts the install — it does not fall back to
installing unverified.

Reinstalling over a running server is safe: the new binary is staged in the
target directory and renamed into place, so an MCP client holding the old
binary open does not cause `ETXTBSY`.

If `~/.local/bin` is not on your `PATH`, the script says so and prints the
`export` line to add. MCP clients are usually given an absolute path anyway,
so `PATH` only matters for running the binary by hand.

### Manual download

Grab the tarball for your platform from the
[releases page](https://github.com/nice-pink/ops-repo-mcp/releases) and
verify it against `checksums.txt`:

```
# linux
sha256sum --ignore-missing -c checksums.txt
# macOS (anchor on ": OK" — a bare grep for the filename matches ": FAILED" too)
shasum -a 256 -c checksums.txt 2>/dev/null | grep "^mcp-server_darwin_arm64.tar.gz: OK"

tar -xzf mcp-server_darwin_arm64.tar.gz
install -m 0755 mcp-server /usr/local/bin/mcp-server
```

### From source

```
make build
```

Produces `bin/mcp-server`. `go install github.com/nice-pink/ops-repo-mcp/cmd/mcp-server@latest`
also works and lands the binary in `$(go env GOPATH)/bin`.

## Agent plugin

`agents-plugin/` packages this server for coding agents: the MCP declaration
plus four skills that teach the agent the parts the tool schemas cannot.
`ops-deploy`, `ops-promote` and `ops-rollback` cover the three tools, chiefly
that a successful call leaves the ops repo dirty and the agent must execute the
returned `commitDirective` — skipping that blocks the next call with
`DIRTY_REPO`. `ops-init` writes the `.ops-repo-mcp.yaml` that an ops repo needs,
deriving the layout from the manifest tree; it calls no tool, so it still works
when the server is refusing to start.

It is an [Agent Plugins](https://agent-plugins.org) 1.0.0 package
(`plugin.json` + `mcp.json`) that also carries Claude Code's own format
(`.claude-plugin/plugin.json` + `.mcp.json`), because Claude Code reads
`.mcp.json` and never `mcp.json`. Both MCP declarations describe the same
stdio server and must be kept in sync. The repo root's
`.claude-plugin/marketplace.json` publishes the directory as the `nice-pink`
marketplace:

```
claude plugin marketplace add nice-pink/ops-repo-mcp && claude plugin install ops-repo@nice-pink
```

Installing the plugin does **not** install the binary — do that first (see
**Install** above). The plugin needs no repo path: with `MCP_OPS_REPO_PATH`
unset it operates on the repo the client is open in. Cursor and Codex install
steps, and the `npx plugins` route, are in
[`agents-plugin/MANUAL.md`](agents-plugin/MANUAL.md); the plugin's own layout
and versioning rules are in
[`agents-plugin/README.md`](agents-plugin/README.md). The plugin version is
repeated across seven files — bump them together when the tool surface or the
response shape changes.

## Release

Release tags are plain major integers: `v1`, `v2`, and so on. `make deploy`
reads the highest existing `v<N>` tag, creates the next one locally, and prints
the push command. It refuses to tag a working tree with uncommitted changes,
since the tag would not contain them:

```
make deploy
git push origin v2
```

Pushing the tag is the step that publishes. It runs
`.github/workflows/release-mcp-server.yml`, which runs the tests,
cross-compiles the four platform targets, and creates a GitHub release with
the tarballs, `checksums.txt`, and `install.sh`. `make deploy` never pushes on
its own.

The tag's version (minus the prefix) is compiled into the binary via
`-ldflags -X main.serverVersion=...` and reported by `mcp-server --version`.
The prefix is what `install.sh` filters the release listing on, so releases
cut under any other tag name will not be found by the installer.

`workflow_dispatch` runs the same build and uploads the tarballs as workflow
artifacts without creating a release — useful for testing the pipeline.

## Configure

Configuration comes from two places. **Layout** — where manifests live inside
the ops repo — belongs in a `.ops-repo-mcp.yaml` committed at the ops repo
root. **Operational** settings — which repo, which envs are permitted,
credentials, timeouts — stay in the MCP client's `env` block.

Precedence for every layout field: `DS_*` env var > `.ops-repo-mcp.yaml` >
built-in default. An empty env var counts as unset, so a client config may pass
`DS_*` through without shadowing the file.

Once the ops repo carries the file, a client entry needs almost nothing:

```json
{
  "mcpServers": {
    "deploy-promote": {
      "command": "/absolute/path/to/ops-repo-mcp/bin/mcp-server",
      "args": [],
      "env": {
        "MCP_ENV_ALLOWLIST": "dev,staging,prod"
      }
    }
  }
}
```

`.mcp.json.example` has the fully-populated form for repos with no config file,
where every layout value comes from the `env` block instead. That form sets
`MCP_OPS_REPO_PATH`, and has to: a repo with no `.ops-repo-mcp.yaml` cannot be
inferred from the working directory.

### Which repo the server operates on

`MCP_OPS_REPO_PATH` names the clone. When it is unset, the server uses the
working directory the client launched it in, walked up to the root of its git
work tree — so the entry above serves any number of ops repos: the repo is
whichever one you have the client open in, and the same user-level config works
everywhere. Set the variable when you want one server pinned to one clone
regardless of where the client is opened.

An inferred repo must **carry a `.ops-repo-mcp.yaml` at its root**. The file is
otherwise optional, but on this path it doubles as the marker that says a human
intended this repository to be deployed from. A git repo on its own is not that
statement, and the consequences of resolving the wrong one are not confined to a
failed call: the inferred repo supplies the branch guard, the layout, and the
remote that the pre-flight `git fetch` authenticates against. Without a marker,
any ancestor `.git` becomes a deploy target — a dotfiles repo at `$HOME` would
make `$HOME` the ops repo for every client launched anywhere below it.

`MCP_ALLOW_ANY_CWD_REPO=1` waives the marker and accepts any git repo the client
is opened in. It logs a warning on every start. It does not waive the git
requirement: without a repo there is no branch guard, no dirty check, and no
rollback history.

`GITHUB_TOKEN` is **not** adopted as a fallback for `MCP_GIT_TOKEN` on the
inferred path. The token is attached as HTTP Basic auth to the pre-flight fetch
and is not scoped by host, so it goes to whatever `origin` the resolved repo
has. While the repo was always `MCP_OPS_REPO_PATH`, you chose that remote; an
inferred repo is chosen by which directory the client happened to open, and an
ambient `GITHUB_TOKEN` is exported on most developer machines for unrelated
reasons. Set `MCP_GIT_TOKEN` to use a token here deliberately; the suppression
is logged when a `GITHUB_TOKEN` was present and ignored.

Both paths resolve a path inside a usable work tree to that work tree's root. A
designated `MCP_OPS_REPO_PATH` is then accepted whether or not it is a git repo
at all, exactly as before; an inferred one has to be a work tree the server can
actually operate on.

"Usable" means HEAD resolves, which is the property the tools need rather than
the one that is convenient to check. Two things fail it: a repo with no commits
yet, and a linked worktree from `git worktree add`, whose refs live in the main
checkout's commondir where the runner cannot read them. Accepting a worktree
would produce a detached-HEAD error on a branch that is not detached, and a
`DIRTY_REPO` listing every tracked file.

One consequence for a designated path: if it points *inside* one of those, it
cannot be walked to a root and is used verbatim. That is the case where
`.ops-repo-mcp.yaml` goes unread and the branch guard quietly falls back to
`origin/HEAD`, so startup logs `ops_repo_not_walkable` saying which it was.

The resolution is logged on every start: `server_start` carries `opsRepo` and
`opsRepoSource` (`MCP_OPS_REPO_PATH` or `cwd`), and the `cwd` case also logs an
`ops_repo_from_cwd` warning. Check that line first if a deploy lands somewhere
unexpected — whether the client passes its session directory as the server's
working directory is the client's behaviour, not this server's.

Three consequences worth knowing:

- Changing directory inside a running session does not move the server. The repo
  is resolved once, at startup, so the client has to restart it.
- A submodule is its own work tree. A client opened inside one resolves to the
  submodule, not the repo containing it; the marker requirement is what stops
  that becoming a silent misplacement.
- Startup failing with `CONFIG_ERROR` is the intended outcome for a working
  directory that is not a marked ops repo, including `/`.

### Branch guard

The tools refuse to write unless the ops repo is on an allowed branch and that
branch can actually be pushed. Without this, a "deploy to prod" made while the
clone sits on a feature branch succeeds, reports success, and never reaches the
cluster — the one failure the operator cannot see in the response.

The allowed set is resolved once at startup, in this order:

1. `MCP_ALLOWED_BRANCHES` — comma-separated, env-only.
2. `branch:` in `.ops-repo-mcp.yaml` — a single branch, committed with the repo.
3. `refs/remotes/origin/HEAD` — the repo's own default branch, which a normal
   `git clone` sets.

If none resolves (a repo built with `git init` + `git remote add` has no
`origin/HEAD`), the check is **skipped** and the startup log says so under
`branch_guard_inferred`. Set one of the first two to close that gap.

A detached HEAD is always refused: it cannot be pushed. So is a branch with no
`origin/<branch>` ref, with `NO_UPSTREAM` — a commit there could never be
published, and the bare `git push` in the commit directive would fail after the
commit had already landed.

### Layout config file

Optional when `MCP_OPS_REPO_PATH` is set, required when it is not — see **Which
repo the server operates on**. `.ops-repo-mcp.yaml` at the root of the
**ops repo** (not this repo);
`.ops-repo-mcp.yaml.example` is a documented template to copy. Read once, at
startup — commit a change and restart the client for it to take effect. The
`exceptionalAppsFile` it points at is the exception: that is re-read on every
call.

```yaml
version: 1
branch: main
layout:
  base: base/apps
  namespace: ""
  pathScheme: "{base}/{app}/{env}"
  imageFileName: deployment.yaml
  imageHistoryFileName: ""
  exceptionalAppsFile: ""
  srcEnv: staging
```

Each key maps to the `DS_*` variable of the same name. Validation runs once, on
the value that actually wins, so the file is not a way around a check — and a
field you have overridden with its `DS_*` variable cannot make the server refuse
to start, however broken the committed value is.

`base` and `pathScheme` must be relative and `..`-free; `imageFileName` and
`imageHistoryFileName` must not contain `/` or `..`. `exceptionalAppsFile` is
relative to the repo root, must be a regular file, and is confined to the repo
even through a symlink — unlike `DS_EXCEPTIONAL_APPS_FILE`, which an operator
sets and may point anywhere on the machine. The config file itself is confined
the same way.

The server logs the resolved layout and the source of each value
(`env` / `repo-file` / `default`) at startup under `layout_resolved`, in a stable
field order so two startups can be diffed.

`MCP_IGNORE_REPO_CONFIG=1` skips reading the file. It does not skip the
marker check above, which only tests that the file exists — a broken committed
file should not silently turn the repo into an unmarked one. It is the escape hatch for a
committed file that breaks startup: the ops repo is a repo agents write to, so a
bad line in someone else's commit should not leave you with no way to start the
server.

**This file carries layout only, by design.** It cannot set
`MCP_ENV_ALLOWLIST`, `MCP_OPS_REPO_PATH`, the git credentials, or the timeouts.
Decoding is strict, so a file that tries to set one of them makes the server
exit with `CONFIG_ERROR` rather than being quietly ignored. The ops repo is a
repository agents are expected to write to; a pull request against it must not
be able to widen what the server may touch or where it sends credentials. An
unrecognised `version` is rejected the same way, as is a second YAML document
(only the first would be read, so silently dropping the rest would contradict
the point).

### Required environment

| Variable | Description |
|----------|-------------|
| `MCP_ENV_ALLOWLIST` | Environments the server may write to. Required unless `MCP_ALLOW_ALL_ENVS=1` is set: an empty allowlist means every environment, including production, so it must be opted into rather than reached by default. |

### Optional environment

Every `DS_*` row can instead come from `.ops-repo-mcp.yaml` (see above); the env
var wins when both are set. The `MCP_*` rows are env-only.

| Variable | Default | Description |
|----------|---------|-------------|
| `MCP_OPS_REPO_PATH` | _(the working directory, walked up to its git root)_ | Absolute path to a local clone of the ops repo. Set but missing, not a directory, or unresolvable exits with `REPO_NOT_FOUND`. Unset, the working directory must be inside a git work tree, that repo's HEAD must resolve, and it must carry `.ops-repo-mcp.yaml`; any of the three failing exits with `CONFIG_ERROR`. See **Which repo the server operates on**. |
| `MCP_ALLOW_ANY_CWD_REPO` | _(unset)_ | Set to `1` to let an inferred ops repo skip the `.ops-repo-mcp.yaml` marker, accepting any git repo the client is opened in. Logs an error-level acknowledgement on every start where it actually waived the marker. No effect when `MCP_OPS_REPO_PATH` is set, and it does not waive the git or HEAD requirements. |
| `MCP_ALLOWED_BRANCHES` | _(inferred from `origin/HEAD`)_ | Comma-separated branches the tools may write to. Wins over `branch:` in `.ops-repo-mcp.yaml`. See **Branch guard**. |
| `MCP_ENV_ALLOWLIST` | _(none — startup fails)_ | Comma-separated list of envs the server may write to. **Required** unless `MCP_ALLOW_ALL_ENVS=1`. |
| `MCP_ALLOW_ALL_ENVS` | _(unset)_ | Set to `1` to run with no environment allowlist, accepting every env including production. Logs a warning on every start. |
| `MCP_LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error`. Logs always go to stderr; stdout is reserved for MCP framing. |
| `MCP_LOCK_TIMEOUT` | `30s` | Max wait for the per-repo lock. Returns `LOCK_TIMEOUT` on expiry. |
| `MCP_RUNNER_TIMEOUT` | `60s` | Max wall-clock for a runner invocation. Returns `RUNNER_TIMEOUT` on expiry; the goroutine is leaked and holds the lock until it finishes. |
| `MCP_GIT_SSH_KEY_PATH` | _(none)_ | SSH key used for `git fetch` / `git pull` against the ops repo's remote. |
| `MCP_GIT_TOKEN` | falls back to `GITHUB_TOKEN`, but only when `MCP_OPS_REPO_PATH` is set | HTTPS token for the same. See **Which repo the server operates on** for why the fallback is withheld from an inferred repo. |
| `MCP_GIT_USER` | `mcp-server` | Author name for any commit objects the underlying runner creates. |
| `MCP_GIT_EMAIL` | _(empty)_ | Author email. |
| `DS_BASE` | _(empty)_ | Base folder inside the ops repo (e.g. `base/apps`). |
| `DS_NAMESPACE` | _(empty)_ | k8s namespace, also expanded into `DS_PATH_SCHEME`. |
| `DS_PATH_SCHEME` | `{base}/{namespace}/{app}/{env}` | Template for the manifest folder. Must contain `{app}` and `{env}`; `..` is rejected. |
| `DS_IMAGE_FILE_NAME` | `deployment.yaml` | Filename of the manifest the server rewrites. No `/` or `..`. |
| `DS_IMAGE_HISTORY_FILE_NAME` | _(empty)_ | If set, the server appends the new tag to this file (relative to the manifest folder) on successful deploy/promote/rollback. |
| `DS_EXCEPTIONAL_APPS_FILE` | _(empty)_ | Path to a YAML file describing apps whose image name / path deviates from the default scheme. Must exist on disk if set. |
| `DS_SRC_ENV` | `staging` | Default source env for `promote` when the caller doesn't pass one. Should match `^[a-z0-9][a-z0-9-]*$`; a value that doesn't logs a startup warning and makes `promote` return `INVALID_INPUT` unless `srcEnv` is passed explicitly. |
| `MCP_IGNORE_REPO_CONFIG` | _(unset)_ | Set to `1` to skip **reading** `.ops-repo-mcp.yaml` and take layout from the env vars and defaults only. It does not skip the marker check, which only tests that the file exists. |

## Tools

All three tools take `app` and an environment, share the same input validation
(`^[a-z0-9][a-z0-9-]*$` for app/env/namespace, `^[a-zA-Z0-9_.-]+$` for tag),
the same pre-checks, and the same per-repo lock.

Inside the lock, before any write, every tool checks in this order: the ops repo
is on an allowed branch (`BRANCH_NOT_ALLOWED`), the working tree is clean
(`DIRTY_REPO`), and the branch is not ahead of its upstream (`BRANCH_AHEAD`) —
then it fast-forwards. `NO_UPSTREAM` comes out of that third check, so a dirty
tree on a branch with no upstream reports `DIRTY_REPO` first.

### `deploy`

Set `app`'s image tag to `tag` in `env`.

| Field | Required | Notes |
|-------|----------|-------|
| `app` | yes | Application name. |
| `env` | yes | Target environment; must pass the allowlist if one is set. |
| `tag` | yes | Image tag to write. |
| `namespace` | no | Overrides `DS_NAMESPACE`. |
| `dryRun` | no | If true, runs the pre-pull and computes affected paths without mutating files. |

### `promote`

Copy the current image tag of `app` from `srcEnv` to `destEnv`.

| Field | Required | Notes |
|-------|----------|-------|
| `app` | yes | |
| `destEnv` | yes | Target env. Must differ from `srcEnv`. |
| `srcEnv` | no | Defaults to `DS_SRC_ENV`. |
| `namespace` | no | |
| `dryRun` | no | Reads and reports the resolved tag without mutating. |

### `rollback`

Revert `app`'s manifest in `env` to the image tag it held in the **commit
immediately preceding the most recent commit that touched the manifest file**.
The previous tag is read from git history; no caller-supplied tag is needed.

| Field | Required | Notes |
|-------|----------|-------|
| `app` | yes | |
| `env` | yes | |
| `namespace` | no | |
| `dryRun` | no | Computes the previous tag and the change-set warning without mutating. |
| `acknowledgeMultiLineChange` | no | Required for a non-dry-run rollback when `multiLineChange` would be true; without it the call is refused with `MULTI_LINE_CHANGE` and nothing is written. |

Response fields specific to `rollback`:

- `currentTag` / `previousTag` — what's in HEAD and what's being reverted to.
- `lastCommit` / `parentCommit` / `lastCommitMessage` — the commit being
  rolled back, and its parent (the rollback target).
- `multiLineChange` — `true` if the last commit touched lines in the manifest
  beyond the image-tag substitution. When true, the response also includes a
  human-readable `warning` and a `nonTagLineChanges` count.

  A non-dry-run rollback in this state is **refused** with `MULTI_LINE_CHANGE`
  unless `acknowledgeMultiLineChange: true` is passed. A tag-only revert of a
  wider commit leaves the other edits in place, so the environment ends up
  running an old image against new configuration — a combination that has never
  run anywhere. Dry runs still report it without refusing, so the caller can see
  what it would be acknowledging.

  One caveat: if `currentTag` cannot be read from the manifest, every differing
  line counts as a non-tag change, so `nonTagLineChanges` may overstate the
  commit. The error message says so when that happens.

Errors specific to `rollback`:

- `NO_PREVIOUS_VERSION` — the manifest has no prior history (initial commit
  only), or the previous tag couldn't be extracted. Deploy an earlier tag
  explicitly via `deploy` instead.
- `MULTI_LINE_CHANGE` — the commit being reverted changed more than the image
  tag. Re-call with `acknowledgeMultiLineChange: true` after showing the
  operator that commit, or use `git revert` to undo the whole thing.

## Response shape

Success:

```json
{
  "success": true,
  "dryRun": false,
  "app": "poma-mcp",
  "env": "prod",
  "previousTag": "v0.0.7",
  "multiLineChange": false,
  "nonTagLineChanges": 0,
  "opsRepoPath": "/abs/path",
  "commitDirective": {
    "action": "git-commit-and-push",
    "filesToStage": ["base/apps/poma-mcp/prod/deployment.yaml"],
    "suggestedCommitMessage": "Rollback poma-mcp(prod) to version: v0.0.7",
    "gitCommands": ["git -C /abs/path add -- ...", "git -C /abs/path commit -m ...", "git -C /abs/path push"]
  },
  "recoveryHint": {
    "detectCommand": "git -C /abs/path status --short -- ...",
    "discardCommand": "git -C /abs/path checkout -- ...",
    "completeCommand": "git -C /abs/path add -- ... && git -C /abs/path commit -m ... && git -C /abs/path push"
  },
  "runnerOutput": "<runner slog lines>"
}
```

Error:

```json
{
  "success": false,
  "errorCode": "DIRTY_REPO",
  "errorMessage": "working tree has uncommitted changes: [...]",
  "recoveryHint": "complete or discard prior changes before retrying (see recoveryHint)",
  "runnerOutput": ""
}
```

Error codes: `INVALID_INPUT`, `ENV_NOT_ALLOWED`, `SAME_ENV`, `PATH_ESCAPE`,
`REPO_NOT_FOUND`, `CONFIG_ERROR`, `DIRTY_REPO`, `BRANCH_NOT_ALLOWED`,
`BRANCH_AHEAD`, `NO_UPSTREAM`, `PULL_FAILED`, `NO_CURRENT_TAG`,
`NO_PREVIOUS_VERSION`, `MULTI_LINE_CHANGE`, `RUNNER_FAILED`, `RUNNER_PANIC`,
`RUNNER_TIMEOUT`, `LOCK_TIMEOUT`.

`RUNNER_FAILED`, `RUNNER_PANIC` and `RUNNER_TIMEOUT` are the only errors raised
after the runner may have written. Those responses carry `mayHaveWritten: true`,
a `dirtyPaths` list of the affected files git currently reports as modified, and
a `postFailureRecovery` object with detect / discard / complete commands. Every
other error is raised before the write phase and leaves the repo untouched.

After a `RUNNER_TIMEOUT` the leaked goroutine keeps holding the repo lock, so
subsequent calls return `LOCK_TIMEOUT` — and that response says how many earlier
runners never returned, because a restart is needed rather than a retry.

