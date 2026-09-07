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
// -ldflags "-X main.serverVersion=<version>" by .github/workflows/release-mcp-server.yml.
// It must stay a var: -X cannot patch a const.
var serverVersion = "0.1.0"

func main() {
	// Handle --version flag before anything else (so stdout contains only JSON)
	for _, arg := range os.Args[1:] {
		if arg == "--version" || arg == "-version" || arg == "version" {
			versionJSON, _ := json.Marshal(map[string]string{
				"name":            serverName,
				"version":         serverVersion,
				"protocolVersion": "2024-11-05",
			})
			fmt.Printf("%s\n", versionJSON)
			return
		}
	}

	// Load and validate config from env vars (exits on CONFIG_ERROR or REPO_NOT_FOUND)
	cfg := loadConfig()

	// Configure slog to write to stderr
	logHandler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel})
	slog.SetDefault(slog.New(logHandler))

	// Startup logs
	slog.Default().Info("server_start",
		"opsRepo", cfg.OpsRepoPath,
		"envAllowlist", cfg.EnvAllowlist,
		"runnerTimeoutS", cfg.RunnerTimeout.Seconds(),
		"lockTimeoutS", cfg.LockTimeout.Seconds(),
	)
	if len(cfg.EnvAllowlist) == 0 {
		slog.Default().Warn("no_env_allowlist", "msg", "MCP_ENV_ALLOWLIST is empty; all environment values are accepted")
	}

	// Build the handler
	h := newHandler(cfg)

	// Create MCP server
	s := server.NewMCPServer(
		serverName,
		serverVersion,
		server.WithToolCapabilities(false),
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
		mcp.WithDescription("Roll back an application in a given environment to the image tag it had before the most recent commit that touched its manifest. The previous tag is read from git history. If the rollback target commit changed more than the image tag line, the response sets multiLineChange=true and includes a warning — the calling LLM SHOULD surface this to the user before committing. Files are mutated but not committed or pushed — the commitDirective in the response instructs the calling LLM to run git add / commit / push."),
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
	)
	s.AddTool(rollbackTool, h.HandleRollback)

	// Start stdio server (blocks until stdin closes or signal)
	if err := server.ServeStdio(s); err != nil {
		slog.Default().Error("server_error", "err", err)
		os.Exit(1)
	}
}
