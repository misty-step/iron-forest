package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Doctor result vocabulary. A check reports how it concluded, and `ok` reports
// whether that conclusion is the required healthy state.
//
//   - observed: a local presence/mode read supplied the answer directly.
//   - evidenced: a read-only external probe supplied the answer.
//   - unknown: no answer could be obtained; `reason` says why.
//
// Evidence and reason never contain credential values.
const (
	doctorObserved  = "observed"
	doctorEvidenced = "evidenced"
	doctorUnknown   = "unknown"

	// openRouterDefaultBase is the OpenRouter API base the doctor key probe uses.
	// OPENROUTER_API_BASE overrides it, which also lets tests point the probe at
	// a local server. The key travels only in the Authorization header.
	openRouterDefaultBase = "https://openrouter.ai/api/v1"

	// openRouterProbeTimeout bounds the read-only key probe so an unreachable
	// provider reports unknown instead of hanging the doctor command.
	openRouterProbeTimeout = 10 * time.Second
)

var (
	// errOpenRouterKeyRejected is the sentinel for a definitive, read-only
	// rejection: the endpoint answered and refused the key. It is distinguishable
	// from a transport failure, which is unknown rather than a diagnosed key fault.
	errOpenRouterKeyRejected = errors.New("openrouter rejected the key")

	// doctorProbeTimeout bounds each read-only gh subprocess probe so an
	// unreachable or hung tool reports unknown instead of hanging the doctor
	// command. It is a var so the timeout regression test can shrink it.
	doctorProbeTimeout = 10 * time.Second
)

type doctorPayload struct {
	Repo    string        `json:"repo"`
	Checks  []doctorCheck `json:"checks"`
	Healthy bool          `json:"healthy"`
}

type doctorCheck struct {
	Name     string `json:"name"`
	Result   string `json:"result"`
	OK       bool   `json:"ok"`
	Evidence string `json:"evidence,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// runDoctor runs the non-mutating machine-operability checks. It reads local
// presence and mode, and performs only read-only auth, capability, key, and
// reachability probes. The forge check never writes remotely.
func runDoctor(_ []string, flags cliFlags) cliOutcome {
	cfg, err := loadConfig(configPath(flags.root))
	if err != nil {
		return failure(exitError, "%s", err)
	}
	checks := doctorChecks(flags.root, cfg.Repo)
	healthy := true
	for _, check := range checks {
		if !check.OK {
			healthy = false
		}
	}
	payload := doctorPayload{Repo: cfg.Repo, Checks: checks, Healthy: healthy}
	exit := exitOK
	if !healthy {
		exit = exitError
	}
	return cliOutcome{Exit: exit, Data: payload, Human: doctorHuman(payload)}
}

func doctorChecks(root, repo string) []doctorCheck {
	ctx := context.Background()
	checks := make([]doctorCheck, 0, 7)
	for _, tool := range []string{"mise", "go", "pi"} {
		checks = append(checks, doctorToolCheck(root, tool))
	}
	checks = append(checks,
		doctorGHAuthCheck(ctx, root),
		doctorCredentialFileCheck(root),
		doctorForgeCheck(ctx, root, repo),
		doctorOpenRouterKeyCheck(ctx, root),
	)
	return checks
}

func doctorToolCheck(root, name string) doctorCheck {
	path, err := trustedExecutable(root, name)
	if err != nil {
		return doctorCheck{Name: name, Result: doctorObserved, OK: false, Evidence: name + " not found on PATH"}
	}
	return doctorCheck{Name: name, Result: doctorObserved, OK: true, Evidence: "path=" + path}
}

func doctorGHAuthCheck(ctx context.Context, root string) doctorCheck {
	if _, err := trustedExecutable(root, "gh"); err != nil {
		return doctorCheck{Name: "gh_auth", Result: doctorUnknown, OK: false, Reason: "gh not found on PATH"}
	}
	stdout, stderr, err := doctorProbe(ctx, root, "gh", "auth", "status")
	evidence := oneLine(firstLine(stdout + "\n" + stderr))
	if evidence == "" && err != nil {
		evidence = "gh auth status failed"
	}
	if err != nil {
		return doctorCheck{Name: "gh_auth", Result: doctorEvidenced, OK: false, Evidence: evidence}
	}
	return doctorCheck{Name: "gh_auth", Result: doctorEvidenced, OK: true, Evidence: evidence}
}

func doctorCredentialFileCheck(root string) doctorCheck {
	path, err := serviceEnvPath(root)
	if err != nil {
		return doctorCheck{Name: "credential_file", Result: doctorUnknown, OK: false, Reason: err.Error()}
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return doctorCheck{Name: "credential_file", Result: doctorObserved, OK: false, Evidence: "missing " + path}
		}
		return doctorCheck{Name: "credential_file", Result: doctorUnknown, OK: false, Reason: fmt.Sprintf("read %s: %v", path, err)}
	}
	if !info.Mode().IsRegular() {
		return doctorCheck{Name: "credential_file", Result: doctorObserved, OK: false, Evidence: "not a regular file: " + path}
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		return doctorCheck{Name: "credential_file", Result: doctorObserved, OK: false, Evidence: fmt.Sprintf("%s mode=%s, want 0600", path, mode)}
	}
	return doctorCheck{Name: "credential_file", Result: doctorObserved, OK: true, Evidence: "mode=0600"}
}

func doctorForgeCheck(ctx context.Context, root, repo string) doctorCheck {
	if _, err := trustedExecutable(root, "gh"); err != nil {
		return doctorCheck{Name: "forge_capability", Result: doctorUnknown, OK: false, Reason: "gh not found on PATH"}
	}
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" {
		return doctorCheck{Name: "forge_capability", Result: doctorUnknown, OK: false, Reason: fmt.Sprintf("repo %q is not owner/name", repo)}
	}
	stdout, stderr, err := doctorProbe(ctx, root, "gh", "api", "repos/"+owner+"/"+name, "--jq", ".permissions.push")
	if err != nil {
		evidence := oneLine(firstLine(stdout + "\n" + stderr))
		if evidence == "" {
			evidence = "gh api capability probe failed"
		}
		return doctorCheck{Name: "forge_capability", Result: doctorEvidenced, OK: false, Evidence: evidence}
	}
	switch strings.TrimSpace(stdout) {
	case "true":
		return doctorCheck{Name: "forge_capability", Result: doctorEvidenced, OK: true, Evidence: "repo " + repo + " push permission true"}
	case "false":
		return doctorCheck{Name: "forge_capability", Result: doctorEvidenced, OK: false, Evidence: "repo " + repo + " push permission false"}
	default:
		return doctorCheck{Name: "forge_capability", Result: doctorUnknown, OK: false, Reason: fmt.Sprintf("unexpected push capability output %q", oneLine(strings.TrimSpace(stdout)))}
	}
}

func doctorOpenRouterKeyCheck(ctx context.Context, root string) doctorCheck {
	path, err := serviceEnvPath(root)
	if err != nil {
		return doctorCheck{Name: "openrouter_key", Result: doctorUnknown, OK: false, Reason: err.Error()}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return doctorCheck{Name: "openrouter_key", Result: doctorObserved, OK: false, Evidence: "missing " + path}
		}
		return doctorCheck{Name: "openrouter_key", Result: doctorUnknown, OK: false, Reason: fmt.Sprintf("read %s: %v", path, err)}
	}
	keys := envFileOpenRouterKeys(data)
	if len(keys) == 0 {
		return doctorCheck{Name: "openrouter_key", Result: doctorObserved, OK: false, Evidence: "no OpenRouter key is configured in " + path}
	}
	for _, value := range keys {
		err = probeOpenRouterKey(ctx, value)
		switch {
		case err == nil:
			continue
		case errors.Is(err, errOpenRouterKeyRejected):
			return doctorCheck{Name: "openrouter_key", Result: doctorEvidenced, OK: false, Evidence: "OpenRouter rejected the key"}
		default:
			return doctorCheck{Name: "openrouter_key", Result: doctorUnknown, OK: false, Reason: "OpenRouter key probe failed: " + oneLine(err.Error())}
		}
	}
	return doctorCheck{Name: "openrouter_key", Result: doctorEvidenced, OK: true, Evidence: "OpenRouter key valid"}
}

// probeOpenRouterKey performs a read-only key-info request against OpenRouter.
// The key travels only in the Authorization header, so transport errors cannot
// echo it. The response body is drained to a bounded discard and never parsed:
// the HTTP status is the only verdict a doctor check needs.
func probeOpenRouterKey(ctx context.Context, key string) error {
	base := strings.TrimSpace(os.Getenv("OPENROUTER_API_BASE"))
	if base == "" {
		base = openRouterDefaultBase
	}
	endpoint := strings.TrimRight(base, "/") + "/key"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+key)
	client := &http.Client{Timeout: openRouterProbeTimeout}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	switch response.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return errOpenRouterKeyRejected
	default:
		return fmt.Errorf("openrouter key probe returned HTTP %d", response.StatusCode)
	}
}

// doctorProbe runs one read-only external probe with both stdout and stderr
// captured. The caller is responsible for not emitting credential values from
// the returned text.
func doctorProbe(ctx context.Context, root, name string, args ...string) (string, string, error) {
	path, err := trustedExecutable(root, name)
	if err != nil {
		return "", "", err
	}
	probeCtx, cancel := context.WithTimeout(ctx, doctorProbeTimeout)
	defer cancel()
	command := exec.Command(path, args...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	stdout, err := processGroupOutput(probeCtx, command)
	return string(stdout), stderr.String(), err
}

// serviceEnvPath names the instance environment file the installer and the
// systemd unit use: %h/.config/iron-forest/<checkout-basename>.env.
func serviceEnvPath(root string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	name := filepath.Base(filepath.Clean(root))
	if name == "." || name == string(filepath.Separator) || name == "" {
		return "", fmt.Errorf("cannot derive instance name from checkout root %q", root)
	}
	return filepath.Join(home, ".config", "iron-forest", name+".env"), nil
}

// envFileOpenRouterKeys returns the distinct non-empty OpenRouter completion
// keys configured in a service environment file: OPENROUTER_API_KEY and any
// OPENROUTER_API_KEY_<ROLE> entries. Values are never logged by callers.
func envFileOpenRouterKeys(data []byte) []string {
	seen := make(map[string]struct{})
	var keys []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if name != "OPENROUTER_API_KEY" && !strings.HasPrefix(name, "OPENROUTER_API_KEY_") {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		if value == "" {
			continue
		}
		if _, dup := seen[value]; dup {
			continue
		}
		seen[value] = struct{}{}
		keys = append(keys, value)
	}
	return keys
}

func firstLine(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		text = text[:index]
	}
	return strings.TrimSpace(text)
}

func doctorHuman(payload doctorPayload) string {
	var human strings.Builder
	fmt.Fprintf(&human, "doctor: %s\n", doctorVerdict(payload.Healthy))
	fmt.Fprintf(&human, "repo: %s", oneLine(payload.Repo))
	for _, check := range payload.Checks {
		verdict := "ok"
		if !check.OK {
			verdict = "fail"
		}
		fmt.Fprintf(&human, "\n  %s %s %s", check.Name, check.Result, verdict)
		if check.Evidence != "" {
			fmt.Fprintf(&human, " %s", oneLine(check.Evidence))
		}
		if check.Reason != "" {
			fmt.Fprintf(&human, " %s", oneLine(check.Reason))
		}
	}
	return human.String()
}

func doctorVerdict(healthy bool) string {
	if healthy {
		return "ok"
	}
	return "fail"
}
