---
name: ops-init
description: Create or repair the .ops-repo-mcp.yaml layout config at the root of a GitOps ops repository, deriving the layout from the repo's own manifest tree and confirming it with the user. Use when the user says "set up this ops repo", "init ops-repo-mcp", "create the ops repo config", "configure deploy/promote for this repo", or "onboard a new ops repo". Also use for any startup CONFIG_ERROR naming that file — "carries no .ops-repo-mcp.yaml, so nothing marks it as an ops repo", "version N is not supported", "contains more than one YAML document", "not found in type", "layout.pathScheme: must contain {env}" — and when a tool resolves a manifest path that does not exist, or a deploy returns RUNNER_FAILED "could not set tag", both of which mean the layout is wrong rather than the call.
compatibility: Writes a file in a local clone of the ops repo. Needs no MCP tool call, so it works even when the deploy-promote server is refusing to start.
metadata:
  author: nice-pink
  version: "0.1.0"
---

# Initialise an ops repo

`.ops-repo-mcp.yaml` at the root of the **ops repo** tells the server where manifests live
inside that repository. It does two jobs:

- **Layout.** Every key under `layout:` maps to the `DS_*` env var of the same name, so a
  repo carrying this file needs no `DS_*` entries in any client's config. The layout is a
  property of the repository, so it travels with it through git instead of being re-entered
  on every machine and by every teammate. (`branch:` is not a layout key — its env
  counterpart is `MCP_ALLOWED_BRANCHES`, and `version:` has none.)
- **Marker.** When `MCP_OPS_REPO_PATH` is unset the server operates on whichever repo the
  client was launched in, and refuses unless that repo carries this file. That is what stops
  an unrelated `.git` — a dotfiles repo at `$HOME`, say — from becoming a deploy target. The
  check only applies on that inferred path: a client that sets `MCP_OPS_REPO_PATH` never
  needs the file, and `MCP_ALLOW_ANY_CWD_REPO=1` waives it.

This skill writes a file and calls no tool, which is the point: at the moment you most need
it, the tools cannot help. Without a marker the server starts but has no repo, so `deploy`,
`promote` and `rollback` all return `NO_OPS_REPO` — that error is the usual reason to reach
for this skill. A bad layout is worse: the server exits at startup and the tools are not
registered at all.

The marker is not the only thing an inferred repo has to satisfy. Its HEAD must resolve too,
which a repo with no commits yet does not — likely here, since a brand-new ops repo is often
a bare `git init`. Committing the file you are about to write fixes both at once. A linked
worktree from `git worktree add` fails the same check for a different reason, and cannot be
fixed by committing: its refs live in the main checkout, where the runner cannot read them.

## Derive the layout, do not interrogate the user

Do not open with a list of questions. Most of this file is already visible in the repo, and
a user who is onboarding an ops repo often cannot recite their own path scheme from memory.
Read first, propose second, ask only about what the tree cannot tell you.

### 1. Find the manifests

From the repo root, locate the files that carry a container image tag:

```
rg -l --glob '!.git' '^\s*-?\s*image:\s*\S+:' .
```

Fall back to `grep -rIl --exclude-dir=.git -E '^[[:space:]]*-?[[:space:]]*image:'` if `rg`
is unavailable. The optional `-` matters: `- image: repo:tag` inside a container list is as
common as the mapping form. Look at 5–10 hits spread across the tree, not one — a single
path cannot distinguish `{base}/{app}/{env}` from `{base}/{namespace}/{app}/{env}`.

`rg` respects `.gitignore`. If it finds nothing, re-run with `--no-ignore` before concluding
anything: an ops repo that generates part of its tree may be hiding the manifests from you,
not lacking them.

If there are genuinely none, the repo templates its manifests (Helm values, Kustomize
overlays). Say so and stop: this server rewrites a tag in a concrete YAML file, so the file
that must be named as `imageFileName` is the one holding the literal `image:` line, not the
rendered output.

### 2. Read the scheme off the paths

The directory holding a manifest is `pathScheme`, and the filename is `imageFileName`:

```
base/apps/poma-mcp/prod/deployment.yaml     → base: base/apps   pathScheme: "{base}/{app}/{env}"
base/resources/default/api/staging/app.yaml → base: base/resources
                                              namespace: default
                                              pathScheme: "{base}/{namespace}/{app}/{env}"
```

`pathScheme` needs `{app}` and `{env}` and nothing else is mandatory, so literal segments
are fine — `apps/{app}/overlays/{env}` is valid and is usually the right answer for an
overlay tree. Do not distort `base` to avoid one. Each placeholder is substituted **once**,
so repeating one in a scheme does not work.

Four things to check before you believe the derivation:

- **Which segment is the app and which is the env.** Compare paths across several apps. The
  env segment repeats a small closed set (`dev`, `staging`, `prod`); the app segment does
  not. Getting these the wrong way round produces a config that validates cleanly and then
  writes to a path that does not exist.
- **Whether a constant segment is `base` or `{namespace}`.** For
  `base/resources/test/api/prod/…`, both `base: base/resources/test` and
  `base: base/resources` + `namespace: test` are derivable. The rule: constant across the
  whole tree → fold it into `base`. Varies per app → it is `{namespace}`, and say so, because
  callers then have to pass `namespace` on calls for apps outside the default one.
- **Whether the app and env names are callable.** Every `app`, `env` and `namespace` is
  validated at the tool boundary against `^[a-z0-9][a-z0-9-]*$`. A tree with `my_app/`,
  `Prod/` or `eu_west/` yields a file that passes every startup check and then fails every
  call with `INVALID_INPUT`. If segments do not match, stop and tell the user — this is not
  something the config can fix.
- **Whether the image name matches the app name.** The tag rewrite matches on the image's
  last path segment, which defaults to the app name. `ghcr.io/nice-pink/poma-ops-repo-mcp:1.2`
  under a directory named `poma-mcp` does not match, and deploy fails with `RUNNER_FAILED`
  *could not set tag* — after startup validated fine. Check the sampled manifests, and route
  mismatches to `exceptionalAppsFile` below.

Apps that sit at a different depth, under a different directory name, or with a different
manifest filename (`statefulset.yaml`, `cronjob.yaml`) are exceptions too. Do not bend
`pathScheme` to accommodate one outlier; that breaks the other ninety.

### 3. Ask only what the repo cannot answer

| Field | Ask about it when | Default if the user has no preference |
|---|---|---|
| `branch` | Always. Which branch does the GitOps controller actually watch? | Omit it, and the server infers from `origin/HEAD`. |
| `srcEnv` | Always. `promote <app> to prod` with no source uses this. | `staging` |
| `imageHistoryFileName` | A file alongside the manifests looks like an append-only tag log. | Omit. |
| `exceptionalAppsFile` | Step 2 turned up apps that do not fit the scheme. | Omit. |

Ask these in one message, with your derived layout shown above them so the user is
confirming a concrete proposal rather than answering an abstract quiz.

Never guess `branch` silently: a "prod deploy" made on a branch nothing deploys from
succeeds, reports success, and never reaches the cluster. And pick a `srcEnv` that is in the
client's `MCP_ENV_ALLOWLIST` — promote checks the allowlist for its **source** as well as its
destination, so a default of `staging` against an allowlist of `dev,prod` makes every
source-less promote fail with `ENV_NOT_ALLOWED`.

## The file

```yaml
version: 1
branch: main
layout:
  base: base/apps
  pathScheme: "{base}/{app}/{env}"
  imageFileName: deployment.yaml
  srcEnv: staging
```

Every field is optional, `version` included — omitted, and `0`, are both read as the current
version. Write `version: 1` anyway: it makes the schema explicit to the next reader, and any
other explicit value is refused.

Omit a field rather than writing an empty value; an empty string means "unset" and only adds
noise. A repo whose layout already matches the defaults needs nothing but the file itself —
one containing only comments is valid and still counts as the marker. (The shipped
`.ops-repo-mcp.yaml.example` does write `namespace: ""` and friends. That is a template
documenting the available keys, not a model for a generated file.)

Two YAML traps, both of which fail at startup:

- **Quote `pathScheme`.** Unquoted, `{base}/{app}/{env}` is a YAML parse error
  (`did not find expected key`). Quote only that value — `version: "1"` fails the other way,
  with `cannot unmarshal !!str into int`.
- **No `---` after the content.** A trailing separator is a second document even with
  nothing following it, and a second document is rejected rather than silently dropped. A
  leading `---` and a trailing `...` are both fine.

### What the server refuses to start on

Validation runs at startup on the value that actually wins, so a bad line here takes the
whole server down with `CONFIG_ERROR` — the three tools included.

| Field | Rule |
|---|---|
| `version` | Omitted or `0` is fine. Any other explicit value is rejected. |
| `base` | `^[A-Za-z0-9_./-]+$`, relative, no `..`. |
| `namespace` | `^[a-z0-9][a-z0-9-]*$`. |
| `pathScheme` | Must contain both `{app}` and `{env}`; no `..`. |
| `imageFileName` | No `/` and no `..`. It is a filename, not a path. |
| `imageHistoryFileName` | Same, when set. Relative to the manifest folder. |
| `exceptionalAppsFile` | Relative to the repo root, **must already exist**, must be a regular file, and must stay inside the repo even through a symlink. |
| any other key | Rejected. Decoding is strict, so a typo like `pathscheme` is a startup failure, not a silently ignored line. |

`srcEnv` is the exception: it is not validated at startup. A value outside
`^[a-z0-9][a-z0-9-]*$` only logs a warning, then makes `promote` return `INVALID_INPUT`
unless the caller passes `srcEnv` explicitly. Write a valid one anyway.

### `exceptionalAppsFile`

Write the file **before** pointing at it. A key naming a file that does not exist yet is a
startup failure, so setting it with the intent of creating it next takes the server down.

```yaml
apps:
- name: test-envs
  namespace: streaming
  envs:
  - name: prod
    path: base/resources/test/test-envs/prod/edge
    file: deployment-edge.yaml
- name: test-app-image
  namespace: test
  image: some-other-image-name
```

`path` is relative to the repo root and replaces the whole computed folder; `file` replaces
`imageFileName` for that env; `image` replaces the image name the tag rewrite matches on.
One trap: the env overrides match on `name` alone, but `image` matches on `name` **and**
`namespace`, so an `image:` entry carrying a `namespace:` that the layout does not set will
silently never apply. This file is re-read on **every call**, unlike `.ops-repo-mcp.yaml`
itself.

### What must never go in it

Only `version`, `branch` and `layout` are accepted. The environment allowlist, the ops repo
path, the git credentials and the timeouts are all deliberately impossible to set from here,
and attempting it fails the server at startup rather than being ignored.

This is not an oversight to work around. The ops repo is a repository agents write to, so a
pull request against it must not be able to widen what the server may touch or redirect
where it sends a token. If the user wants to change any of those, they belong in the MCP
client's `env` block. Say that plainly rather than looking for another way in.

## After writing it

1. **Show the file and the paths it implies.** Not just the YAML — expand it for one real
   app: "for `poma-mcp` in `prod` this resolves to `base/apps/poma-mcp/prod/deployment.yaml`",
   and confirm that file exists on disk.
2. **Commit *and push* it.** Both halves matter. The tools refuse to work on a dirty tree
   (`DIRTY_REPO`) or on a branch ahead of its upstream (`BRANCH_AHEAD`), so an uncommitted or
   unpushed config blocks the very call you are about to make to verify it. Stage only this
   file. Push to the branch you named in `branch:` — the config can only be verified on the
   branch it declares, and committing it to a feature branch gives `BRANCH_NOT_ALLOWED`.
3. **Restart the MCP client.** The file is read **once, at startup**, and so is the
   decision about which repo the server works on — a server that came up with `NO_OPS_REPO`
   keeps returning it until it is relaunched. Nothing about the
   running server changes when you write it, so a tool that failed a moment ago will keep
   failing until the client relaunches the server. Say this explicitly — it is the most
   common reason someone concludes the config "did not work".
4. **Verify with `promote`, dry-run.** Call `promote` with `dryRun: true` for a real app,
   using an env that is actually live as `srcEnv`. It resolves the source manifest under the
   lock and returns `NO_CURRENT_TAG` when the path is wrong, the file is missing, or the
   image name does not match — the three failures step 2 can get wrong.

   **Do not verify with `deploy`.** A dry-run deploy returns before it touches the
   filesystem, so `filesToStage` is a rendered template, not a file that exists: a wholly
   fictional app returns `success: true`. It proves the scheme renders, nothing more.

If step 4 fails, read the code before re-editing the layout. `BRANCH_AHEAD`, `DIRTY_REPO`,
`BRANCH_NOT_ALLOWED` and `NO_UPSTREAM` are step 2 not being finished, not a layout problem —
see **Pre-flight checks that can block you** in
[`../ops-deploy/SKILL.md`](../ops-deploy/SKILL.md). `PULL_FAILED` on a private HTTPS remote
is a third thing again: writing the marker is what puts the server into cwd-inferred mode,
and in that mode an ambient `GITHUB_TOKEN` is deliberately not adopted. The startup log says
so with `github_token_suppressed`; the fix is to set `MCP_GIT_TOKEN`.

Otherwise check the server's stderr. `layout_resolved` logs every layout value with its
source (`env` / `repo-file` / `default`), which distinguishes "my file is being ignored" from
"my file says something I did not intend". A value showing `env` means a `DS_*` variable in
the client config is overriding the file; that variable wins, so remove it from the client
rather than editing the file again. `branch:` is not in that line — it has its own
`branch_guard` entry with its own `source` field.

## Editing an existing file

Read it before touching it. Preserve fields you were not asked to change, and keep the
existing key order — this file is reviewed in pull requests, and a diff that reshuffles
seven lines to change one is a diff nobody can review. The same startup validation applies,
so an edit is as capable of taking the server down as an initial write.

If the file is currently making the server exit, the operator's escape hatch is
`MCP_IGNORE_REPO_CONFIG=1`, which skips **reading** the file so they can start the server
and fix it. It does not skip the marker check, which only tests that the file exists — so a
file that fails to parse does not also stop the repo counting as an ops repo.
