package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// secretScanner names the external binary the checks require for generic
// high-entropy credential detection. It reads the local working tree only and is
// never run against a network service; --no-verification skips the check that
// would confirm a candidate with a remote service, and --no-update suppresses
// the self-update call.
const secretScanner = "trufflehog"

// defaultScanExcludes are paths the scanner always skips because they are the
// engine's own bookkeeping, not repository content: version-control internals
// and the factory workspace (per-Run worktrees, Ledger, lock). This is
// infrastructure, not a protected-path list, so it stays tiny and never extends
// to content.
var defaultScanExcludes = []string{".git", ".forest"}

type secretFinding struct {
	Path string
	Rule string
}

// secretsConfig is forest.secrets.yaml: the explicit exclusion path for
// legitimate fixtures. It must stay narrow and name content only; it is not a
// return of the protected-path list that docs/adr/0003 removed.
type secretsConfig struct {
	Exclude []string `yaml:"exclude"`
}

// scanEnv holds the two seams a scan needs and a test can substitute: where to
// find the external scanner binary, and how to run the generic scan once found.
var scanEnv = struct {
	lookPath   func(root, name string) (string, error)
	runGeneric func(ctx context.Context, bin, dir string, excludes []string) ([]secretFinding, error)
}{
	lookPath:   trustedExecutable,
	runGeneric: runTrufflehog,
}

// scanSecretsTree runs the generic secret scan over a working tree dir and
// returns every finding. It fails closed: a required generic scanner that is
// absent returns an error naming the tool rather than silently narrowing to
// nothing. Exclusions are loaded from forest.secrets.yaml at the scanned root.
func scanSecretsTree(dir string) ([]secretFinding, error) {
	return scanSecretsTreeRoot(context.Background(), dir, dir)
}

// scanSecretsTreeRoot runs the generic secret scan over working tree dir and
// returns every finding. trustRoot is the repository boundary used to resolve
// the external scanner: an executable inside trustRoot is refused. The Gate
// passes the managed checkout as trustRoot so a candidate worktree cannot
// supply the scanner; the CLI passes dir so the scanned tree itself is the
// boundary.
func scanSecretsTreeRoot(ctx context.Context, trustRoot, dir string) ([]secretFinding, error) {
	excludes := append([]string(nil), defaultScanExcludes...)
	if cfg, err := loadSecretsConfig(dir); err == nil {
		excludes = append(excludes, cfg.Exclude...)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return scanGeneric(ctx, trustRoot, dir, excludes)
}

// loadSecretsConfig reads forest.secrets.yaml from the scanned root, or returns
// an os.IsNotExist error when the file is absent. It decodes with
// KnownFields(true) so unknown keys fail rather than being silently ignored,
// and every exclude is validated before it reaches the scanner.
func loadSecretsConfig(dir string) (secretsConfig, error) {
	var cfg secretsConfig
	b, err := os.ReadFile(filepath.Join(dir, "forest.secrets.yaml"))
	if err != nil {
		return cfg, err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(b))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		// An empty or comment-only file is the same as an absent one: the
		// operator created the slot and has not filled it yet.
		if errors.Is(err, io.EOF) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("parse forest.secrets.yaml: %w", err)
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return cfg, fmt.Errorf("parse forest.secrets.yaml: %w", err)
		}
		return cfg, fmt.Errorf("parse forest.secrets.yaml: multiple YAML documents")
	}
	for _, pattern := range cfg.Exclude {
		if err := validateScanExclude(dir, pattern); err != nil {
			return cfg, fmt.Errorf("forest.secrets.yaml exclude %q: %w", pattern, err)
		}
	}
	return cfg, nil
}

// validateScanExclude keeps a configured exclusion narrow and unambiguous: it
// must be a relative path that names a file or directory inside the scanned
// tree, with no parent element and no glob metacharacters. The whole scanned
// tree is never a valid fixture exclusion, because it would suppress every
// finding.
func validateScanExclude(dir, pattern string) error {
	if strings.TrimSpace(pattern) == "" {
		return errors.New("must not be empty")
	}
	if filepath.IsAbs(pattern) {
		return errors.New("must be a relative path")
	}
	if hasParentPathElement(pattern) {
		return errors.New("must not contain a parent path element")
	}
	if strings.ContainsAny(pattern, "*?[") {
		return errors.New("must not contain glob metacharacters")
	}
	clean := filepath.Clean(filepath.FromSlash(pattern))
	if clean == "." || clean == string(filepath.Separator) {
		return errors.New("must not name the scanned tree root")
	}
	current := dir
	for _, part := range strings.Split(filepath.ToSlash(clean), "/") {
		current = filepath.Join(current, filepath.FromSlash(part))
		info, err := os.Lstat(current)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return errors.New("names a path that does not exist")
			}
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("must not be a symlink")
		}
	}
	return nil
}

// hasParentPathElement reports whether a slash-separated path contains a `..`
// element, even when filepath.Clean would later fold it away.
func hasParentPathElement(pattern string) bool {
	for _, part := range strings.Split(filepath.ToSlash(pattern), "/") {
		if part == ".." {
			return true
		}
	}
	return false
}

// scanSecretsCheckError turns a trusted scan's result into a single error for
// the review/verdict check surface. It keeps the same redaction as the
// `scan-secrets` CLI: findings name the path and rule, never a candidate value.
func scanSecretsCheckError(findings []secretFinding, err error) error {
	if err != nil {
		return err
	}
	if len(findings) == 0 {
		return nil
	}
	var b strings.Builder
	for _, f := range findings {
		fmt.Fprintf(&b, "\n%s in %s", f.Rule, oneLine(f.Path))
	}
	return fmt.Errorf("leaked credential material in the worktree%s", b.String())
}

// scanGeneric runs the external generic scanner over the working tree. If the
// scanner binary is absent the check fails closed, naming the tool, rather than
// silently skipping the scan. The exclusions are handed to the scanner too, so a
// legitimate fixture on the list cannot fail.
func scanGeneric(ctx context.Context, trustRoot, dir string, excludes []string) ([]secretFinding, error) {
	bin, err := scanEnv.lookPath(trustRoot, secretScanner)
	if err != nil {
		return nil, fmt.Errorf("%s not found on PATH; failing closed instead of skipping the generic high-entropy scan: %w", secretScanner, err)
	}
	return scanEnv.runGeneric(ctx, bin, dir, excludes)
}

// runTrufflehog runs the trufflehog filesystem detector against the local
// working tree dir and parses its JSON findings. It never contacts a network
// service: the scan reads only local files, --no-verification disables the
// confirmation call that is the tool's only network request, and --no-update
// suppresses the self-update check.
//
// The exclusion path is a file trufflehog reads as a list of regular
// expressions, so every exclusion is written as an escaped, anchored rule for
// its exact path rather than a raw name that would match unrelated paths.
// The scan runs through processGroupOutput so a cancelled or timed-out Verdict
// terminates the scanner's process group.
func runTrufflehog(ctx context.Context, bin, dir string, excludes []string) ([]secretFinding, error) {
	excludeFile, err := writeExcludePaths(dir, excludes)
	if err != nil {
		return nil, err
	}
	defer os.Remove(excludeFile)
	args := []string{"filesystem", "--no-update", "--no-verification", "--json", "--directory", dir}
	if excludeFile != "" {
		args = append(args, "--exclude-paths", excludeFile)
	}
	cmd := exec.Command(bin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := processGroupOutput(ctx, cmd)
	if err != nil {
		return nil, fmt.Errorf("%s failed: %w: %s", secretScanner, err, strings.TrimSpace(stderr.String()))
	}
	// Findings are the only JSON on stdout; the tool's own progress logs go to
	// stderr and are never parsed as detections.
	var findings []secretFinding
	sc := bufio.NewScanner(bytes.NewReader(stdout))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var f struct {
			DetectorName   string `json:"DetectorName"`
			SourceMetadata struct {
				Data struct {
					Filesystem struct {
						File string `json:"file"`
					} `json:"Filesystem"`
				} `json:"Data"`
			} `json:"SourceMetadata"`
		}
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			return nil, fmt.Errorf("parse %s output: %w", secretScanner, err)
		}
		path := f.SourceMetadata.Data.Filesystem.File
		if path == "" {
			path = "<unknown>"
		}
		rule := f.DetectorName
		if rule == "" {
			rule = "generic-secret"
		}
		findings = append(findings, secretFinding{Path: path, Rule: rule})
	}

	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("parse %s output: %w", secretScanner, err)
	}
	return findings, nil
}

// writeExcludePaths writes the exclusion list to a temporary file trufflehog
// reads as its --exclude-paths (one regular expression per line) and returns the
// file's path. The caller removes it. An empty list writes no file and returns
// an empty path. Each entry is converted to an escaped, anchored exact-path rule
// so a short fixture name cannot overmatch unrelated credential files.
func writeExcludePaths(dir string, excludes []string) (string, error) {
	if len(excludes) == 0 {
		return "", nil
	}
	f, err := os.CreateTemp("", "forest-excludes-*.txt")
	if err != nil {
		return "", fmt.Errorf("write scanner exclusion file: %w", err)
	}
	path := f.Name()
	for _, exclude := range excludes {
		if _, err := f.WriteString(excludePathRule(dir, exclude) + "\n"); err != nil {
			f.Close()
			os.Remove(path)
			return "", fmt.Errorf("write scanner exclusion file: %w", err)
		}
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("close scanner exclusion file: %w", err)
	}
	return path, nil
}

// excludePathRule converts one literal repository-relative exclusion into a
// trufflehog --exclude-paths regular expression that matches exactly that path
// instead of every path that contains the name. It does not resolve symlinks:
// the rule must name the configured path, never the target a symlink points at.
func excludePathRule(dir, exclude string) string {
	full := filepath.Join(filepath.Clean(dir), filepath.Clean(exclude))
	return "^" + regexp.QuoteMeta(full) + "$"
}
