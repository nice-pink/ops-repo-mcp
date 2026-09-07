package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

const serverName = "deploy-promote-mcp"

// serverVersion is overridden at build time with
// -ldflags "-X main.serverVersion=<version>" by .github/workflows/release-mcp-server.yml,
// which passes the release tag minus its "v". The "dev" default is therefore
// what an unreleased local build reports, and is never what ships.
// It must stay a var: -X cannot patch a const.
var serverVersion = "dev"

func main() {
	// Handle --version flag before anything else (so stdout contains only JSON)
	for _, arg := range os.Args[1:] {
		if arg == "--version" || arg == "-version" || arg == "version" {
			versionJSON, _ := json.Marshal(map[string]string{
				"name":            serverName,
				"version":         serverVersion,
				"protocolVersion": mcp.LATEST_PROTOCOL_VERSION,
			})
			fmt.Printf("%s\n", versionJSON)
			return
		}
	}

	// Load and validate config from env vars (exits on CONFIG_ERROR or REPO_NOT_FOUND)
	cfg := loadConfig()

	// Configure slog to write to stderr
	logHandler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel})
	setBaseHandler(logHandler)

	// Startup logs
	slog.Default().Info("server_start",
		"opsRepo", cfg.OpsRepoPath,
		"opsRepoSource", cfg.OpsRepoPathSource,
		"envAllowlist", cfg.EnvAllowlist,
		"runnerTimeoutS", cfg.RunnerTimeout.Seconds(),
		"lockTimeoutS", cfg.LockTimeout.Seconds(),
	)
	if cfg.OpsRepoPathSource == opsRepoPathCwd {
		slog.Default().Warn("ops_repo_from_cwd",
			"opsRepo", cfg.OpsRepoPath,
			"msg", "MCP_OPS_REPO_PATH is not set, so the working directory the client launched this server in is being used as the ops repo. Set MCP_OPS_REPO_PATH to pin one clone.",
		)
	}
	if cfg.GitHubTokenSuppressed {
		slog.Default().Warn("github_token_suppressed",
			"msg", "GITHUB_TOKEN is set but was not adopted: the ops repo is inferred from the working directory, and the pre-flight fetch would send the token to whatever remote that repo has. Set MCP_GIT_TOKEN to use a token here deliberately.",
		)
	}
	if cfg.AnyCwdRepoAllowed {
		slog.Default().Warn("any_cwd_repo_allowed",
			"opsRepo", cfg.OpsRepoPath,
			"msg", allowAnyCwdRepoEnv+"=1 is set: any git repo the client is opened in becomes a deploy target, with no "+repoConfigFileName+" marking it as an ops repo. Its origin also determines where the pre-flight fetch sends MCP_GIT_TOKEN.",
		)
	}
	if cfg.AllEnvsAllowed {
		slog.Default().Warn("all_envs_allowed",
			"msg", "MCP_ALLOW_ALL_ENVS=1 is set: every environment value is accepted, including production. Set MCP_ENV_ALLOWLIST instead to constrain this.",
		)
	}

	slog.Default().Info("branch_guard",
		"allowedBranches", cfg.AllowedBranches,
		"source", cfg.AllowedBranchSource,
	)
	if len(cfg.AllowedBranches) == 0 {
		slog.Default().Warn("branch_guard_inferred",
			"msg", "no MCP_ALLOWED_BRANCHES and no branch in "+repoConfigFileName+"; the allowed branch is inferred from refs/remotes/origin/HEAD, and if the ops repo has no such ref the branch check is skipped entirely",
		)
	}

	// Report the resolved layout and where each value came from. Config
	// precedence that is not logged is config precedence nobody can debug.
	slog.Default().Info("layout_resolved",
		"repoConfigFile", repoConfigFileName,
		"repoConfigFound", cfg.RepoConfigFound,
		"base", cfg.DSBase,
		"namespace", cfg.DSNamespace,
		"pathScheme", cfg.DSPathScheme,
		"imageFileName", cfg.DSImageFileName,
		"imageHistoryFileName", cfg.DSImageHistoryFileName,
		"exceptionalAppsFile", cfg.DSExceptionalAppsFile,
		"srcEnv", cfg.DSSrcEnv,
		"sources", cfg.LayoutSources.String(),
	)

	// Build the handler
	h := newHandler(cfg)

	// Create MCP server
	// WithRecovery: a panic inside a tool handler must not take the process down.
	// The runner goroutine has its own recover, but the pre-flight path (which
	// calls BuildApp, and through it the exceptional-apps reader that panics on a
	// read failure) runs outside it.
	s := server.NewMCPServer(
		serverName,
		serverVersion,
		server.WithToolCapabilities(false),
		server.WithRecovery(),
	)

	// Register deploy tool
	deployTool := mcp.NewTool("deploy",
		mcp.WithDescription("Set the image tag for an application in a given environment by updating its manifest YAML in the ops repo. Files are mutated but not committed or pushed — the commitDirective in the response instructs the calling LLM to run git add / commit / push."),
		mcp.WithString("app",
			mcp.Required(),
			mcp.Description("Application name as it appears in the ops repo path."),
			mcp.Pattern(`^[a-z0-9][a-z0-9-]*$`),
		),
		mcp.WithString("env",
			mcp.Required(),
			mcp.Description("Target deployment environment (e.g. dev, staging, prod)."),
			mcp.Pattern(`^[a-z0-9][a-z0-9-]*$`),
		),
		mcp.WithString("tag",
			mcp.Required(),
			mcp.Description("Docker image tag to deploy."),
			mcp.Pattern(`^[a-zA-Z0-9_.-]+$`),
		),
		mcp.WithString("namespace",
			mcp.Description("Kubernetes namespace. Overrides DS_NAMESPACE env var."),
			mcp.Pattern(`^[a-z0-9][a-z0-9-]*$`),
		),
		mcp.WithBoolean("dryRun",
			mcp.Description("If true, compute and return affected paths without modifying any files."),
		),
	)
	s.AddTool(deployTool, h.HandleDeploy)

	// Register promote tool
	promoteTool := mcp.NewTool("promote",
		mcp.WithDescription("Promote an application by copying its current image tag from a source environment to a destination environment. Files are mutated but not committed or pushed — the commitDirective in the response instructs the calling LLM to run git add / commit / push."),
		mcp.WithString("app",
			mcp.Required(),
			mcp.Description("Application name as it appears in the ops repo path."),
			mcp.Pattern(`^[a-z0-9][a-z0-9-]*$`),
		),
		mcp.WithString("destEnv",
			mcp.Required(),
			mcp.Description("Target environment to promote INTO (e.g. prod)."),
			mcp.Pattern(`^[a-z0-9][a-z0-9-]*$`),
		),
		mcp.WithString("srcEnv",
			mcp.Description("Source environment to promote FROM. Overrides DS_SRC_ENV (default: staging)."),
			mcp.Pattern(`^[a-z0-9][a-z0-9-]*$`),
		),
		mcp.WithString("namespace",
			mcp.Description("Kubernetes namespace. Overrides DS_NAMESPACE env var."),
			mcp.Pattern(`^[a-z0-9][a-z0-9-]*$`),
		),
		mcp.WithBoolean("dryRun",
			mcp.Description("If true, read and return the current tag from srcEnv and affected paths without modifying any files."),
		),
	)
	s.AddTool(promoteTool, h.HandlePromote)

	// Register rollback tool
	rollbackTool := mcp.NewTool("rollback",
		mcp.WithDescription("Roll back an application in a given environment to the image tag it had before the most recent commit that touched its manifest. The previous tag is read from git history. If the rollback target commit changed more than the image tag line, a non-dry-run call is REFUSED with MULTI_LINE_CHANGE unless acknowledgeMultiLineChange=true; surface that commit to the user before acknowledging. Files are mutated but not committed or pushed — the commitDirective in the response instructs the calling LLM to run git add / commit / push."),
		mcp.WithString("app",
			mcp.Required(),
			mcp.Description("Application name as it appears in the ops repo path."),
			mcp.Pattern(`^[a-z0-9][a-z0-9-]*$`),
		),
		mcp.WithString("env",
			mcp.Required(),
			mcp.Description("Target deployment environment (e.g. dev, staging, prod)."),
			mcp.Pattern(`^[a-z0-9][a-z0-9-]*$`),
		),
		mcp.WithString("namespace",
			mcp.Description("Kubernetes namespace. Overrides DS_NAMESPACE env var."),
			mcp.Pattern(`^[a-z0-9][a-z0-9-]*$`),
		),
		mcp.WithBoolean("dryRun",
			mcp.Description("If true, resolve the previous tag and compute the change-set warning without modifying any files."),
		),
		mcp.WithBoolean("acknowledgeMultiLineChange",
			mcp.Description("Required to be true for a non-dry-run rollback when the target commit changed lines beyond the image tag; otherwise the call is refused with MULTI_LINE_CHANGE. Set it only after showing the operator that commit and getting their agreement — a tag-only revert of a wider commit leaves the old image running against new configuration."),
		),
	)
	s.AddTool(rollbackTool, h.HandleRollback)

	// Start stdio server (blocks until stdin closes or signal)
	if err := server.ServeStdio(s); err != nil {
		slog.Default().Error("server_error", "err", err)
		os.Exit(1)
	}
}
