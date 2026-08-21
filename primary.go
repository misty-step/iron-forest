package main

import (
	"context"
	"fmt"
	"strings"
)

const (
	primaryRefPrefix = "refs/heads/"

	// PrimarySourceConfig identifies a `forest.yaml primary:` override.
	PrimarySourceConfig = "config"
	// PrimarySourceRemote identifies the advertised remote HEAD symref.
	PrimarySourceRemote = "remote"
)

// validatePrimaryRef requires a full `refs/heads/<branch>` ref. The advertised
// remote value and the config override are held to the same shape, so every
// consumer can treat the resolved ref as a branch ref without decoding two
// spellings.
func validatePrimaryRef(ref string) error {
	branch, ok := strings.CutPrefix(ref, primaryRefPrefix)
	if !ok || branch == "" {
		return fmt.Errorf("primary must be a refs/heads/* ref, got %q", ref)
	}
	if strings.ContainsAny(branch, " \t\r\n\\~^:?*[") ||
		strings.Contains(branch, "..") ||
		strings.Contains(branch, "@{") ||
		strings.Contains(branch, "//") ||
		strings.HasPrefix(branch, "/") ||
		strings.HasSuffix(branch, "/") ||
		strings.HasSuffix(branch, ".") ||
		strings.HasSuffix(branch, ".lock") {
		return fmt.Errorf("primary ref %q is not a valid branch ref", ref)
	}
	return nil
}

// resolvePrimary returns the full primary branch ref and the source that
// supplied it. An explicit forest.yaml primary wins without contacting the
// remote; otherwise the ref is read from the remote HEAD advertisement and
// validated. It never consults the clone-time refs/remotes/origin/HEAD, because
// that symref is absent from bare fetches and can go stale when the remote
// default changes.
func resolvePrimary(ctx context.Context, root string, cfg Config) (string, string, error) {
	if override := strings.TrimSpace(cfg.Primary); override != "" {
		if err := validatePrimaryRef(override); err != nil {
			return "", "", err
		}
		return override, PrimarySourceConfig, nil
	}
	ref, err := remoteHeadSymref(ctx, root)
	if err != nil {
		return "", "", err
	}
	if err := validatePrimaryRef(ref); err != nil {
		return "", "", err
	}
	return ref, PrimarySourceRemote, nil
}

// resolvedPrimaryRef loads the checkout config and resolves it. The audit path
// already has checkout access but not a Config; this keeps the local forest.yaml
// primary override in force for snapshot enumeration.
func resolvedPrimaryRef(ctx context.Context, root string) (string, string, error) {
	cfg, err := loadConfig(configPath(root))
	if err != nil {
		return "", "", err
	}
	return resolvePrimary(ctx, root, cfg)
}

// remoteHeadSymref reads the advertised HEAD symref from `git ls-remote
// --symref origin HEAD`. The output carries the symref line followed by the
// peeled object line:
//
//	ref: refs/heads/main\tHEAD
//	<sha>\tHEAD
//
// A remote that advertises no such line must be refused rather than guessed.
func remoteHeadSymref(ctx context.Context, root string) (string, error) {
	output, err := gitOutput(ctx, root, "ls-remote", "--symref", "origin", "HEAD")
	if err != nil {
		return "", fmt.Errorf("read remote HEAD symref: %w", err)
	}
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		first, second, ok := strings.Cut(line, "\t")
		if !ok || second != "HEAD" {
			continue
		}
		ref, ok := strings.CutPrefix(strings.TrimSpace(first), "ref: ")
		if !ok {
			continue
		}
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}
		return ref, nil
	}
	return "", fmt.Errorf("remote HEAD symref is missing or malformed")
}
