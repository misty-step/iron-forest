package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// providerMechanism names which credential source a run resolves. selfcheck
// reports the active one and fails loudly when none resolves, so an operator
// always knows which route an agent run will authenticate through.
type providerMechanism int

const (
	mechNone providerMechanism = iota
	// mechEnv resolves an OpenRouter key from the environment variable named by
	// openRouterKeyVar. It is the first-install path for an operator who has no
	// Mint route: set the variable and a run authenticates directly.
	mechEnv
	// mechMint resolves a credential through Mint marker placed in the opencode
	// configuration (an apiKey whose value begins with "__mint."). Mint is
	// internal infrastructure; this is the existing path, preserved unchanged.
	mechMint
	// mechConfig resolves a credential from an opencode configuration file that
	// carries no Mint marker (a real apiKey or an {env:...} reference the
	// operator wrote by hand). It keeps an operator-declared config working.
	mechConfig
)

// String renders the mechanism for selfcheck's report.
func (m providerMechanism) String() string {
	switch m {
	case mechEnv:
		return "env"
	case mechMint:
		return "mint"
	case mechConfig:
		return "config"
	default:
		return "none"
	}
}

const (
	// openRouterKeyVar is the environment variable a stranger supplies an
	// OpenRouter key through. The value is never written to disk and never
	// logged: the staged configuration references it with {env:OPENROUTER_API_KEY}
	// and the run's child environment carries the value for opencode to read, so
	// the credential never lands in a rendered declaration, a trace, a note, a
	// Ledger row, or a log line.
	openRouterKeyVar = "OPENROUTER_API_KEY"

	// providerBaseURLOverride is an optional broker base-URL override for the
	// env mechanism. A host that fronts OpenRouter with its own credential
	// broker proxy (for example Mint) points this at that proxy, so the broker
	// becomes nothing more than an optional base-URL override while the key
	// still travels by environment variable.
	providerBaseURLOverride = "FOREST_PROVIDER_BASE_URL"

	// defaultOpenRouterBaseURL is the direct OpenRouter endpoint used when no
	// base-URL override is configured.
	defaultOpenRouterBaseURL = "https://openrouter.ai/api/v1"
)

// mintMarkerPrefix is the leading marker of a Mint-resolved apiKey. A value
// beginning with it is not a credential byte but a broker token: opencode sends
// it to the broker's proxy address, and the broker resolves the real key.
const mintMarkerPrefix = "__mint."

// openCodeProvider describes one provider block of the opencode configuration.
type openCodeProvider struct {
	Options struct {
		APIKey  string `json:"apiKey"`
		BaseURL string `json:"baseURL"`
	} `json:"options"`
}

// openCodeConfig is the shape of opencode.json that this program reads and
// writes. Unknown fields are not modelled here because they are copied through
// verbatim in the Mint path rather than reconstructed.
type openCodeConfig struct {
	Provider map[string]openCodeProvider `json:"provider"`
}

// resolveProvider returns the active credential mechanism for a run and the
// opencode.json bytes to stage under the run config root. The environment-key
// mechanism wins when openRouterKeyVar is set, so an operator outside Misty
// Step who supplies an OpenRouter key gets a direct route even when a Mint
// config ships with the factory. Otherwise an opencode configuration file that
// resolves (a Mint broker marker, or a hand-written config) is preserved as the
// operator configured it. When nothing resolves, ok is false and the caller
// decides whether that is an error.
func resolveProvider(repoDir string) (mech providerMechanism, cfg []byte, ok bool) {
	if os.Getenv(openRouterKeyVar) != "" {
		return mechEnv, envProviderConfig(), true
	}
	b, found := providerConfigBytes(repoDir)
	if !found {
		return mechNone, nil, false
	}
	if hasMintMarker(b) {
		return mechMint, b, true
	}
	return mechConfig, b, true
}

// envProviderConfig builds the env-mechanism opencode.json. The apiKey is the
// {env:...} reference, never the key value, and the baseURL is the optional
// broker override when set, else the direct OpenRouter endpoint.
func envProviderConfig() []byte {
	base := os.Getenv(providerBaseURLOverride)
	if base == "" {
		base = defaultOpenRouterBaseURL
	}
	cfg := openCodeConfig{Provider: map[string]openCodeProvider{
		"openrouter": {Options: struct {
			APIKey  string `json:"apiKey"`
			BaseURL string `json:"baseURL"`
		}{APIKey: "{env:" + openRouterKeyVar + "}", BaseURL: base}},
	}}
	b, err := json.Marshal(cfg)
	if err != nil {
		// Marshalling a fixed shape cannot fail; a nil slice stages nothing.
		return nil
	}
	return b
}

// providerConfigBytes reads the provider configuration a real run actually
// uses: the factory project's own .opencode/opencode.json, falling back to the
// operator's global opencode config when the factory ships none. It returns the
// bytes and true when a file resolves.
func providerConfigBytes(repoDir string) ([]byte, bool) {
	for _, src := range []string{projectProviderConfigPath(repoDir), openCodeProviderConfigPath()} {
		if src == "" {
			continue
		}
		b, err := os.ReadFile(src)
		if err == nil {
			return b, true
		}
	}
	return nil, false
}

// hasMintMarker reports whether an opencode.json carries a Mint broker marker:
// any provider's apiKey whose value begins with the Mint marker prefix. A
// configuration with one resolves through Mint; without one it is either a
// hand-written config or an env-mechanism reference.
func hasMintMarker(b []byte) bool {
	var cfg openCodeConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return false
	}
	for _, p := range cfg.Provider {
		if strings.HasPrefix(p.Options.APIKey, mintMarkerPrefix) {
			return true
		}
	}
	return false
}

// providerMechanismReport renders a one-line statement of the active credential
// mechanism, or an error when none resolves.
func providerMechanismReport(repoDir string) (string, error) {
	mech, _, ok := resolveProvider(repoDir)
	if !ok {
		return "", fmt.Errorf("no provider credential resolves: set %s or configure an opencode provider route", openRouterKeyVar)
	}
	return fmt.Sprintf("provider: %s", mech), nil
}
