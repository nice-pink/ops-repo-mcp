package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// makeHandler creates a handler with test-friendly defaults (no real repo needed
// for pure validation tests).
func makeHandler(allowlist []string) *handler {
	cfg := serverConfig{
		OpsRepoPath:    "/tmp/test-ops-repo",
		EnvAllowlist:   allowlist,
		LockTimeout:    5 * time.Second,
		RunnerTimeout:  10 * time.Second,
		DSPathScheme:   "{base}/{namespace}/{app}/{env}",
		DSImageFileName: "deployment.yaml",
		DSSrcEnv:       "staging",
	}
	return newHandler(cfg)
}

// makeDeployReq builds a mcp.CallToolRequest for the deploy tool.
func makeDeployReq(args map[string]any) mcp.CallToolRequest {
	return mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "deploy",
			Arguments: args,
		},
	}
}

// makePromoteReq builds a mcp.CallToolRequest for the promote tool.
func makePromoteReq(args map[string]any) mcp.CallToolRequest {
	return mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "promote",
			Arguments: args,
		},
	}
}

// makeRollbackReq builds a mcp.CallToolRequest for the rollback tool.
func makeRollbackReq(args map[string]any) mcp.CallToolRequest {
	return mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "rollback",
			Arguments: args,
		},
	}
}

func parseErrorCode(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	if len(result.Content) == 0 {
		t.Fatal("empty result content")
	}
	tc, ok := result.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", result.Content[0])
	}
	var resp struct {
		ErrorCode string `json:"errorCode"`
	}
	if err := json.Unmarshal([]byte(tc.Text), &resp); err != nil {
		t.Fatalf("failed to parse response JSON: %v\n%s", err, tc.Text)
	}
	return resp.ErrorCode
}

// TestHandlerValidation_Pattern tests that pattern validation catches bad app/env/tag values.
func TestHandlerValidation_Pattern(t *testing.T) {
	h := makeHandler(nil)
	ctx := context.Background()

	cases := []struct {
		name string
		args map[string]any
	}{
		{"app_uppercase", map[string]any{"app": "MyApp", "env": "dev", "tag": "v1"}},
		{"app_starts_with_dash", map[string]any{"app": "-myapp", "env": "dev", "tag": "v1"}},
		{"env_space", map[string]any{"app": "myapp", "env": "dev env", "tag": "v1"}},
		{"tag_space", map[string]any{"app": "myapp", "env": "dev", "tag": "tag with space"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := h.HandleDeploy(ctx, makeDeployReq(tc.args))
			if err != nil {
				t.Fatalf("handler returned Go error: %v", err)
			}
			code := parseErrorCode(t, result)
			if code != codeInvalidInput {
				t.Errorf("expected %s, got %s", codeInvalidInput, code)
			}
		})
	}
}

// TestHandlerValidation_Required tests that missing required fields return INVALID_INPUT.
func TestHandlerValidation_Required(t *testing.T) {
	h := makeHandler(nil)
	ctx := context.Background()

	// Deploy missing tag
	result, err := h.HandleDeploy(ctx, makeDeployReq(map[string]any{
		"app": "myapp",
		"env": "dev",
	}))
	if err != nil {
		t.Fatalf("handler returned Go error: %v", err)
	}
	if code := parseErrorCode(t, result); code != codeInvalidInput {
		t.Errorf("deploy missing tag: expected %s, got %s", codeInvalidInput, code)
	}

	// Promote missing destEnv
	result2, err2 := h.HandlePromote(ctx, makePromoteReq(map[string]any{
		"app": "myapp",
	}))
	if err2 != nil {
		t.Fatalf("handler returned Go error: %v", err2)
	}
	if code := parseErrorCode(t, result2); code != codeInvalidInput {
		t.Errorf("promote missing destEnv: expected %s, got %s", codeInvalidInput, code)
	}
}

// TestHandlerValidation_AdditionalProperties tests that unknown fields return INVALID_INPUT.
func TestHandlerValidation_AdditionalProperties(t *testing.T) {
	h := makeHandler(nil)
	ctx := context.Background()

	result, err := h.HandleDeploy(ctx, makeDeployReq(map[string]any{
		"app":           "myapp",
		"env":           "dev",
		"tag":           "v1",
		"unknownField":  "surprise",
	}))
	if err != nil {
		t.Fatalf("handler returned Go error: %v", err)
	}
	code := parseErrorCode(t, result)
	if code != codeInvalidInput {
		t.Errorf("expected %s for unknown field, got %s", codeInvalidInput, code)
	}
}

// TestSameEnvReject tests that promote with srcEnv==destEnv returns SAME_ENV.
func TestSameEnvReject(t *testing.T) {
	h := makeHandler(nil)
	ctx := context.Background()

	result, err := h.HandlePromote(ctx, makePromoteReq(map[string]any{
		"app":     "myapp",
		"destEnv": "staging",
		"srcEnv":  "staging",
	}))
	if err != nil {
		t.Fatalf("handler returned Go error: %v", err)
	}
	code := parseErrorCode(t, result)
	if code != codeSameEnv {
		t.Errorf("expected %s, got %s", codeSameEnv, code)
	}
}

// TestEnvNotInAllowlist tests that envs not in the allowlist return ENV_NOT_ALLOWED.
func TestEnvNotInAllowlist(t *testing.T) {
	h := makeHandler([]string{"dev", "staging", "prod"})
	ctx := context.Background()

	result, err := h.HandleDeploy(ctx, makeDeployReq(map[string]any{
		"app": "myapp",
		"env": "canary",
		"tag": "v1",
	}))
	if err != nil {
		t.Fatalf("handler returned Go error: %v", err)
	}
	code := parseErrorCode(t, result)
	if code != codeEnvNotAllowed {
		t.Errorf("expected %s, got %s", codeEnvNotAllowed, code)
	}
}

// TestPathEscapeViaHandler tests that a DS_PATH_SCHEME that would escape the ops
// repo root returns PATH_ESCAPE (requires the mock ops repo to exist on disk).
func TestPathEscapeViaHandler(t *testing.T) {
	// Set up a real temp dir so the handler can stat the ops repo.
	tmp := t.TempDir()
	h := &handler{
		cfg: serverConfig{
			OpsRepoPath:     tmp,
			EnvAllowlist:    nil,
			LockTimeout:     2 * time.Second,
			RunnerTimeout:   5 * time.Second,
			// Path scheme that would escape: ../../etc/{app}/{env}
			DSPathScheme:    "../../etc/{app}/{env}",
			DSImageFileName: "deployment.yaml",
			DSSrcEnv:        "staging",
			DSBase:          "",
		},
		repoMu: newTimedMu(),
	}
	ctx := context.Background()

	result, err := h.HandleDeploy(ctx, makeDeployReq(map[string]any{
		"app": "myapp",
		"env": "dev",
		"tag": "v1",
	}))
	if err != nil {
		t.Fatalf("handler returned Go error: %v", err)
	}
	code := parseErrorCode(t, result)
	if code != codePathEscape {
		t.Errorf("expected %s, got %s", codePathEscape, code)
	}
}

// TestEnvAllowlistPermitted tests that an env IN the allowlist is not rejected.
func TestEnvAllowlistPermitted(t *testing.T) {
	h := makeHandler([]string{"dev", "staging", "prod"})
	ctx := context.Background()

	// We only need to test that allowlist check passes; the deploy will fail at
	// the repo-open step since there's no real repo. The error code should be
	// something other than ENV_NOT_ALLOWED.
	result, err := h.HandleDeploy(ctx, makeDeployReq(map[string]any{
		"app": "myapp",
		"env": "dev",
		"tag": "v1",
	}))
	if err != nil {
		t.Fatalf("handler returned Go error: %v", err)
	}
	code := parseErrorCode(t, result)
	if code == codeEnvNotAllowed {
		t.Errorf("env 'dev' should be allowed but got ENV_NOT_ALLOWED")
	}
	// Should reach later failure (PULL_FAILED or LOCK_TIMEOUT or RUNNER_FAILED)
	if strings.Contains(code, "ENV_NOT_ALLOWED") {
		t.Errorf("unexpected ENV_NOT_ALLOWED for permitted env")
	}
}

// TestRollbackValidation exercises the input-validation paths for rollback.
// The full git-history path requires a real ops repo and is covered by the
// rollback-specific unit tests in rollback_test.go.
func TestRollbackValidation(t *testing.T) {
	h := makeHandler([]string{"dev", "staging", "prod"})
	ctx := context.Background()

	t.Run("missing_env", func(t *testing.T) {
		result, err := h.HandleRollback(ctx, makeRollbackReq(map[string]any{"app": "myapp"}))
		if err != nil {
			t.Fatalf("handler returned Go error: %v", err)
		}
		if code := parseErrorCode(t, result); code != codeInvalidInput {
			t.Errorf("expected %s, got %s", codeInvalidInput, code)
		}
	})

	t.Run("bad_app_pattern", func(t *testing.T) {
		result, err := h.HandleRollback(ctx, makeRollbackReq(map[string]any{"app": "MyApp", "env": "dev"}))
		if err != nil {
			t.Fatalf("handler returned Go error: %v", err)
		}
		if code := parseErrorCode(t, result); code != codeInvalidInput {
			t.Errorf("expected %s, got %s", codeInvalidInput, code)
		}
	})

	t.Run("unknown_field", func(t *testing.T) {
		result, err := h.HandleRollback(ctx, makeRollbackReq(map[string]any{
			"app": "myapp", "env": "dev", "tag": "v1",
		}))
		if err != nil {
			t.Fatalf("handler returned Go error: %v", err)
		}
		if code := parseErrorCode(t, result); code != codeInvalidInput {
			t.Errorf("expected %s for unknown 'tag' field, got %s", codeInvalidInput, code)
		}
	})

	t.Run("env_not_in_allowlist", func(t *testing.T) {
		result, err := h.HandleRollback(ctx, makeRollbackReq(map[string]any{
			"app": "myapp", "env": "canary",
		}))
		if err != nil {
			t.Fatalf("handler returned Go error: %v", err)
		}
		if code := parseErrorCode(t, result); code != codeEnvNotAllowed {
			t.Errorf("expected %s, got %s", codeEnvNotAllowed, code)
		}
	})
}
