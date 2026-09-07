package main

import (
	"fmt"
	"strings"
)

// commitDirective is the machine-readable instruction for the calling LLM to
// perform git add / commit / push after a successful mutation.
type commitDirective struct {
	Action                 string   `json:"action"`
	FilesToStage           []string `json:"filesToStage"`
	SuggestedCommitMessage string   `json:"suggestedCommitMessage"`
	GitCommands            []string `json:"gitCommands"`
}

// recoveryHint tells the LLM how to detect, discard, or complete a prior
// partially-committed mutation. It is null in dry-run responses.
type recoveryHint struct {
	DetectCommand   string `json:"detectCommand"`
	DiscardCommand  string `json:"discardCommand"`
	CompleteCommand string `json:"completeCommand"`
}

// deploySuccessResponse is the JSON payload returned on a successful deploy call.
type deploySuccessResponse struct {
	Success         bool             `json:"success"`
	DryRun          bool             `json:"dryRun"`
	App             string           `json:"app"`
	Env             string           `json:"env"`
	Tag             string           `json:"tag"`
	OpsRepoPath     string           `json:"opsRepoPath"`
	CommitDirective commitDirective  `json:"commitDirective"`
	RecoveryHint    *recoveryHint    `json:"recoveryHint,omitempty"`
	RunnerOutput    string           `json:"runnerOutput"`
}

// rollbackSuccessResponse is the JSON payload returned on a successful rollback call.
// MultiLineChange / NonTagLineChanges / Warning warn the caller that the commit
// being reverted touched more than just the image tag, so a simple tag revert
// may not restore the prior intent of the manifest.
type rollbackSuccessResponse struct {
	Success           bool            `json:"success"`
	DryRun            bool            `json:"dryRun"`
	App               string          `json:"app"`
	Env               string          `json:"env"`
	CurrentTag        string          `json:"currentTag"`
	PreviousTag       string          `json:"previousTag"`
	LastCommit        string          `json:"lastCommit"`
	LastCommitMessage string          `json:"lastCommitMessage"`
	ParentCommit      string          `json:"parentCommit"`
	MultiLineChange   bool            `json:"multiLineChange"`
	NonTagLineChanges int             `json:"nonTagLineChanges"`
	Warning           string          `json:"warning,omitempty"`
	OpsRepoPath       string          `json:"opsRepoPath"`
	CommitDirective   commitDirective `json:"commitDirective"`
	RecoveryHint      *recoveryHint   `json:"recoveryHint,omitempty"`
	RunnerOutput      string          `json:"runnerOutput"`
}

// promoteSuccessResponse is the JSON payload returned on a successful promote call.
type promoteSuccessResponse struct {
	Success         bool             `json:"success"`
	DryRun          bool             `json:"dryRun"`
	App             string           `json:"app"`
	SrcEnv          string           `json:"srcEnv"`
	DestEnv         string           `json:"destEnv"`
	ResolvedTag     string           `json:"resolvedTag"`
	OpsRepoPath     string           `json:"opsRepoPath"`
	CommitDirective commitDirective  `json:"commitDirective"`
	RecoveryHint    *recoveryHint    `json:"recoveryHint,omitempty"`
	RunnerOutput    string           `json:"runnerOutput"`
}

// errorResponse is the JSON payload returned on any tool error.
type errorResponse struct {
	Success      bool   `json:"success"`
	ErrorCode    string `json:"errorCode"`
	ErrorMessage string `json:"errorMessage"`
	RecoveryHint string `json:"recoveryHint,omitempty"`
	RunnerOutput string `json:"runnerOutput"`
}

// buildDeployCommitDirective constructs the commitDirective and recoveryHint for deploy.
func buildDeployCommitDirective(app, env, tag, opsRepoPath string, filesToStage []string, dryRun bool) (commitDirective, *recoveryHint) {
	if dryRun {
		return commitDirective{
			Action:                 "dry-run-preview",
			FilesToStage:           filesToStage,
			SuggestedCommitMessage: fmt.Sprintf("Deploy %s(%s) version: %s", app, env, tag),
			GitCommands:            []string{}, // empty array (not null) per schema
		}, nil
	}

	msg := fmt.Sprintf("Deploy %s(%s) version: %s", app, env, tag)
	cmds := buildGitCommands(opsRepoPath, filesToStage, msg)

	cd := commitDirective{
		Action:                 "git-commit-and-push",
		FilesToStage:           filesToStage,
		SuggestedCommitMessage: msg,
		GitCommands:            cmds,
	}

	rh := buildRecoveryHint(opsRepoPath, filesToStage, msg)
	return cd, rh
}

// buildRollbackCommitDirective constructs the commitDirective and recoveryHint for rollback.
func buildRollbackCommitDirective(app, env, previousTag, opsRepoPath string, filesToStage []string, dryRun bool) (commitDirective, *recoveryHint) {
	msg := fmt.Sprintf("Rollback %s(%s) to version: %s", app, env, previousTag)
	if dryRun {
		return commitDirective{
			Action:                 "dry-run-preview",
			FilesToStage:           filesToStage,
			SuggestedCommitMessage: msg,
			GitCommands:            []string{},
		}, nil
	}

	cmds := buildGitCommands(opsRepoPath, filesToStage, msg)
	cd := commitDirective{
		Action:                 "git-commit-and-push",
		FilesToStage:           filesToStage,
		SuggestedCommitMessage: msg,
		GitCommands:            cmds,
	}
	rh := buildRecoveryHint(opsRepoPath, filesToStage, msg)
	return cd, rh
}

// buildPromoteCommitDirective constructs the commitDirective and recoveryHint for promote.
func buildPromoteCommitDirective(app, destEnv, resolvedTag, opsRepoPath string, filesToStage []string, dryRun bool) (commitDirective, *recoveryHint) {
	if dryRun {
		return commitDirective{
			Action:                 "dry-run-preview",
			FilesToStage:           filesToStage,
			SuggestedCommitMessage: fmt.Sprintf("Promote %s(%s) version: %s", app, destEnv, resolvedTag),
			GitCommands:            []string{}, // empty array (not null) per schema
		}, nil
	}

	msg := fmt.Sprintf("Promote %s(%s) version: %s", app, destEnv, resolvedTag)
	cmds := buildGitCommands(opsRepoPath, filesToStage, msg)

	cd := commitDirective{
		Action:                 "git-commit-and-push",
		FilesToStage:           filesToStage,
		SuggestedCommitMessage: msg,
		GitCommands:            cmds,
	}

	rh := buildRecoveryHint(opsRepoPath, filesToStage, msg)
	return cd, rh
}

func buildGitCommands(opsRepoPath string, filesToStage []string, msg string) []string {
	cmds := make([]string, 0, len(filesToStage)+2)
	for _, f := range filesToStage {
		cmds = append(cmds, fmt.Sprintf("git -C %s add -- %s", opsRepoPath, f))
	}
	cmds = append(cmds, fmt.Sprintf("git -C %s commit -m %q", opsRepoPath, msg))
	cmds = append(cmds, fmt.Sprintf("git -C %s push", opsRepoPath))
	return cmds
}

func buildRecoveryHint(opsRepoPath string, filesToStage []string, msg string) *recoveryHint {
	if len(filesToStage) == 0 {
		return nil
	}

	// detectCommand: git status for all staged files
	detectArgs := make([]string, len(filesToStage))
	for i, f := range filesToStage {
		detectArgs[i] = "-- " + f
	}
	detect := fmt.Sprintf("git -C %s status --short %s", opsRepoPath, strings.Join(detectArgs, " "))

	// discardCommand: git checkout for all staged files
	discardArgs := make([]string, len(filesToStage))
	for i, f := range filesToStage {
		discardArgs[i] = "-- " + f
	}
	discard := fmt.Sprintf("git -C %s checkout %s", opsRepoPath, strings.Join(discardArgs, " "))

	// completeCommand: add + commit + push
	addCmds := make([]string, len(filesToStage))
	for i, f := range filesToStage {
		addCmds[i] = fmt.Sprintf("git -C %s add -- %s", opsRepoPath, f)
	}
	complete := strings.Join(addCmds, " && ") +
		fmt.Sprintf(" && git -C %s commit -m %q && git -C %s push", opsRepoPath, msg, opsRepoPath)

	return &recoveryHint{
		DetectCommand:   detect,
		DiscardCommand:  discard,
		CompleteCommand: complete,
	}
}
