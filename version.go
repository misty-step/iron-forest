package main

import "fmt"

// Build metadata is injected at link time with -ldflags -X. A plain `go build`
// leaves buildSHA at its default so this command stays honest: it reports the
// revision as unknown instead of fabricating one. buildDirty is a string so a
// linker line can set it to the exact verbatim "true"/"false" build decision.
var (
	buildSHA   = "unknown"
	buildTime  = ""
	buildDirty = "false"
)

// versionPayload is the machine-readable answer to "what revision is this?".
// Field names stay snake_case to match the rest of the CLI payload surface.
type versionPayload struct {
	BuildSHA   string `json:"build_sha"`
	CommitTime string `json:"commit_time,omitempty"`
	Dirty      bool   `json:"dirty"`
}

func runVersion(_ []string, _ cliFlags) cliOutcome {
	payload := versionPayload{
		BuildSHA:   buildSHA,
		CommitTime: buildTime,
		Dirty:      buildDirty == "true",
	}
	return cliOutcome{
		Exit:  exitOK,
		Data:  payload,
		Human: versionHuman(payload),
	}
}

func versionHuman(payload versionPayload) string {
	commit := payload.CommitTime
	if commit == "" {
		commit = "unknown"
	}
	return fmt.Sprintf("build_sha: %s\ncommit_time: %s\ndirty: %t",
		payload.BuildSHA, commit, payload.Dirty)
}
