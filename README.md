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

Prebuilt binaries are attached to every `mcp-server-v*` release for macOS
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
| `VERSION` | latest `mcp-server-v*` release | Pin a specific release tag, e.g. `mcp-server-v0.1.0`. Drafts and prereleases are never auto-selected; a prerelease can be pinned explicitly, a draft cannot be installed at all (no tag, no public download path). |
| `GITHUB_TOKEN` | _(none)_ | Sent only to the GitHub API, to lift the unauthenticated rate limit when resolving the latest release. Never sent with the asset download. |
| `SKIP_CHECKSUM` | `0` | Set to `1` to install without verifying the SHA-256. Only useful if `checksums.txt` is missing from a release or no `sha256sum`/`shasum` is available. |

Downgrading or pinning:

```
curl -fsSL https://raw.githubusercontent.com/nice-pink/ops-repo-mcp/main/install.sh | VERSION=mcp-server-v0.1.0 sh
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

## Release

Binaries are built by `.github/workflows/release-mcp-server.yml`. Pushing a
tag with the `mcp-server-v` prefix runs the tests, cross-compiles the four
platform targets, and publishes a GitHub release with the tarballs,
`checksums.txt`, and `install.sh`:

```
git tag mcp-server-v0.1.0
git push origin mcp-server-v0.1.0
```

The tag's version (minus the prefix) is compiled into the binary via
`-ldflags -X main.serverVersion=...` and reported by `mcp-server --version`.
The prefix is what `install.sh` filters the release listing on, so releases
cut under any other tag name will not be found by the installer.

`workflow_dispatch` runs the same build and uploads the tarballs as workflow
artifacts without creating a release — useful for testing the pipeline.

## Configure

Point your MCP client at the binary. A copy-paste example lives in
`.mcp.json.example`; minimal config:

```json
{
  "mcpServers": {
    "deploy-promote": {
      "command": "/absolute/path/to/ops-repo-mcp/bin/mcp-server",
      "args": [],
      "env": {
        "MCP_OPS_REPO_PATH":   "/absolute/path/to/your/ops-repo",
        "MCP_ENV_ALLOWLIST":   "dev,staging,prod",
        "DS_BASE":             "base/apps",
        "DS_PATH_SCHEME":      "{base}/{app}/{env}",
        "DS_IMAGE_FILE_NAME":  "deployment.yaml",
        "DS_SRC_ENV":          "dev"
      }
    }
  }
}
```

### Required environment

| Variable | Description |
|----------|-------------|
| `MCP_OPS_REPO_PATH` | Absolute path to a local clone of the ops repo. Required; server exits with `REPO_NOT_FOUND` if missing. |

### Optional environment

| Variable | Default | Description |
|----------|---------|-------------|
| `MCP_ENV_ALLOWLIST` | _(empty — all envs accepted)_ | Comma-separated list of envs the server will operate on. Strongly recommended. |
| `MCP_LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error`. Logs always go to stderr; stdout is reserved for MCP framing. |
| `MCP_LOCK_TIMEOUT` | `30s` | Max wait for the per-repo lock. Returns `LOCK_TIMEOUT` on expiry. |
| `MCP_RUNNER_TIMEOUT` | `60s` | Max wall-clock for a runner invocation. Returns `RUNNER_TIMEOUT` on expiry; the goroutine is leaked and holds the lock until it finishes. |
| `MCP_GIT_SSH_KEY_PATH` | _(none)_ | SSH key used for `git fetch` / `git pull` against the ops repo's remote. |
| `MCP_GIT_TOKEN` | falls back to `GITHUB_TOKEN` | HTTPS token for the same. |
| `MCP_GIT_USER` | `mcp-server` | Author name for any commit objects the underlying runner creates. |
| `MCP_GIT_EMAIL` | _(empty)_ | Author email. |
| `DS_BASE` | _(empty)_ | Base folder inside the ops repo (e.g. `base/apps`). |
| `DS_NAMESPACE` | _(empty)_ | k8s namespace, also expanded into `DS_PATH_SCHEME`. |
| `DS_PATH_SCHEME` | `{base}/{namespace}/{app}/{env}` | Template for the manifest folder. Must contain `{app}` and `{env}`; `..` is rejected. |
| `DS_IMAGE_FILE_NAME` | `deployment.yaml` | Filename of the manifest the server rewrites. No `/` or `..`. |
| `DS_IMAGE_HISTORY_FILE_NAME` | _(empty)_ | If set, the server appends the new tag to this file (relative to the manifest folder) on successful deploy/promote/rollback. |
| `DS_EXCEPTIONAL_APPS_FILE` | _(empty)_ | Path to a YAML file describing apps whose image name / path deviates from the default scheme. Must exist on disk if set. |
| `DS_SRC_ENV` | `staging` | Default source env for `promote` when the caller doesn't pass one. |

## Tools

All three tools take `app` and an environment, share the same input validation
(`^[a-z0-9][a-z0-9-]*$` for app/env/namespace, `^[a-zA-Z0-9_.-]+$` for tag),
the same dirty-tree / branch-ahead pre-checks, and the same per-repo lock.

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

Response fields specific to `rollback`:

- `currentTag` / `previousTag` — what's in HEAD and what's being reverted to.
- `lastCommit` / `parentCommit` / `lastCommitMessage` — the commit being
  rolled back, and its parent (the rollback target).
- `multiLineChange` — `true` if the last commit touched lines in the manifest
  beyond the image-tag substitution. When true, the response also includes a
  human-readable `warning` and a `nonTagLineChanges` count. The calling LLM
  should surface this to the operator before executing the commit directive,
  because a tag-only revert won't restore those other changes.

Errors specific to `rollback`:

- `NO_PREVIOUS_VERSION` — the manifest has no prior history (initial commit
  only), or the previous tag couldn't be extracted. Deploy an earlier tag
  explicitly via `deploy` instead.

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
`REPO_NOT_FOUND`, `CONFIG_ERROR`, `DIRTY_REPO`, `BRANCH_AHEAD`, `PULL_FAILED`,
`NO_CURRENT_TAG`, `NO_PREVIOUS_VERSION`, `RUNNER_FAILED`, `RUNNER_PANIC`,
`RUNNER_TIMEOUT`, `LOCK_TIMEOUT`.

