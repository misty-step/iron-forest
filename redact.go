package main

import "regexp"

// secretRedacted is the marker every credential-shaped token is replaced with
// before the surrounding text is published verbatim.
const secretRedacted = "[REDACTED]"

// secretShapedPatterns are the credential-shaped token shapes the factory never
// publishes verbatim. Each pattern matches one full token so its replacement
// leaves no prefix of the original secret behind (an oracle plants
// sk-AAAAAAAAAAAAAAAA and requires that no sk- substring survive).
var secretShapedPatterns = []*regexp.Regexp{
	// Vendor API keys and tokens: the sk- prefix family the storage grep names,
	// GitHub personal/clone/repo fine-grained tokens, Stripe live keys, Slack
	// tokens, and AWS access key ids.
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{10,}`),
	regexp.MustCompile(`(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{30,}`),
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`),
	regexp.MustCompile(`sk_live_[A-Za-z0-9]{16,}`),
	regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{8,}`),
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	// The Mint resolver marker, __mint.<alias>__, whose value is itself a
	// credential the daemon never holds as a key byte.
	regexp.MustCompile(`__mint\.[A-Za-z0-9._-]+__`),
	// The factory's own model proxy: a Tailscale host whose address must not be
	// published any more than the credential it demultiplexes.
	regexp.MustCompile(`[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?\.ts\.net(?::[0-9]{1,5})?`),
}

// redactSecretShaped replaces every credential-shaped token in s with a marker
// so no secret-shaped string can be published verbatim. Mutable text that does
// not match any pattern is returned unchanged.
func redactSecretShaped(s string) string {
	for _, re := range secretShapedPatterns {
		s = re.ReplaceAllString(s, secretRedacted)
	}
	return s
}

// secretShaped reports whether s carries a credential-shaped token. A flow
// treats text that needs redaction as untrusted: it must not be published at
// all, and the run is recorded as blocked for an operator.
func secretShaped(s string) bool {
	return redactSecretShaped(s) != s
}
