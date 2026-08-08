package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// secretScanner names the external binary the gate requires for generic
// high-entropy credential detection. It is deliberately the same tool the
// managed misty-step repositories already run in their own pre-push hooks, so a
// factory push and a managed push fail on the same detector. It is never run
// against a network service; it reads the local working tree only.
const secretScanner = "trufflehog"

// reMintMarker matches the factory's own credential marker shape,
// `__mint.<alias>__` (for example `__mint.openrouter.ironforest__`), that the
// factory writes as an API key alias in rendered declarations. A marker is not a
// live credential byte, but the alias plus the proxy host is infrastructure
// detail that must not land in a repository the operator does not control.
var reMintMarker = regexp.MustCompile(`__mint\.[A-Za-z0-9._-]+__`)

// reMintProxy matches the Mint proxy host the factory routes provider traffic
// through (for example `mint.tail5f5eb4.ts.net`). Combined with a marker name it
// names the operator's provider route, so a rendered declaration that reaches a
// worktree must be caught before publication.
var reMintProxy = regexp.MustCompile(`(?i)mint[a-z0-9.-]*\.ts\.net`)

// defaultScanExcludes are paths the scanner always skips because they are its
// own bookkeeping, not repository content: version-control internals and the
// factory workspace (per-run worktrees, ledger, traces). This is infrastructure,
// not a protected-path list, so it stays tiny and never covers content.
var defaultScanExcludes = []string{".git", ".forest"}

// secretFinding is one place in a working tree that carries leaked credential
// material: the file, which rule matched, and the matched value.
type secretFinding struct {
	Path  string
	Rule  string
	Match string
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
	lookPath   func(string) (string, error)
	runGeneric func(bin, dir string, excludes []string) ([]secretFinding, error)
}{
	lookPath:   exec.LookPath,
	runGeneric: runTrufflehog,
}

// scanSecretsTree runs the full secret scan over a working tree dir: the
// built-in content rules first, then the generic high-entropy scanner unless
// noGeneric is set. Both the `scan-secrets` command and the pre-publication gate
// go through this one function so they can never drift. It fails closed: a
// finding, or a required generic scanner that is absent, returns an error.
// Exclusions are loaded from forest.secrets.yaml at the scanned root.
func scanSecretsTree(dir string, noGeneric bool) ([]secretFinding, error) {
	excludes := append([]string(nil), defaultScanExcludes...)
	if cfg, err := loadSecretsConfig(dir); err == nil {
		excludes = append(excludes, cfg.Exclude...)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	findings, err := scanSecretsContent(dir, excludes)
	if err != nil {
		return nil, err
	}
	// Only reach for the generic scanner when the content rules came back clean:
	// a found leak already fails the gate, and a clean tree must then fail closed
	// rather than silently narrow itself to the two content rules when the
	// generic scanner is absent.
	if len(findings) == 0 && !noGeneric {
		generic, gerr := scanGeneric(dir, excludes)
		if gerr != nil {
			return nil, gerr
		}
		findings = append(findings, generic...)
	}
	return findings, nil
}

// scanSecretsBeforePublish runs the full secret scan over a working tree and
// returns an error when the tree carries leaked credential material, or when the
// required generic scanner is absent (fail closed). The builder and fixer call
// it on their worktree immediately before commitAndPush, so publication depends
// on a clean scan: a rendered declaration that reaches the worktree cannot
// survive `git add -A` and the push.
func scanSecretsBeforePublish(wtDir string) error {
	findings, err := scanSecretsTree(wtDir, false)
	if err != nil {
		return err
	}
	if len(findings) == 0 {
		return nil
	}
	var b strings.Builder
	for _, f := range findings {
		fmt.Fprintf(&b, "%s %q in %s\n", f.Rule, f.Match, f.Path)
	}
	return fmt.Errorf("leaked credential material in the working tree; refusing to publish:\n%s", strings.TrimSpace(b.String()))
}

// cmdScanSecrets scans the working tree under dir before a branch is published.
// It always runs the built-in content rules (Mint markers and the Mint proxy
// host) over the whole tree, untracked files included, and then the generic
// high-entropy scanner unless --no-generic is passed. A finding, or a missing
// generic scanner, fails the gate closed. The exclusion path comes from
// forest.secrets.yaml at the scanned root.
func cmdScanSecrets(args []string) int {
	noGeneric := false
	var dir string
	for _, a := range args {
		switch a {
		case "--no-generic":
			noGeneric = true
		case "":
			continue
		default:
			if dir != "" {
				fmt.Fprintf(os.Stderr, "forest scan-secrets: unexpected argument %q\n", a)
				return 2
			}
			dir = a
		}
	}
	if dir == "" {
		dir = "."
	}
	findings, err := scanSecretsTree(dir, noGeneric)
	if err != nil {
		fmt.Fprintln(os.Stderr, "forest scan-secrets:", err)
		return 1
	}
	if len(findings) == 0 {
		fmt.Println("forest scan-secrets: ok")
		return 0
	}
	for _, f := range findings {
		fmt.Fprintf(os.Stderr, "forest scan-secrets: %s %q in %s\n", f.Rule, f.Match, f.Path)
	}
	fmt.Fprintln(os.Stderr, "forest scan-secrets: leaked credential material in the working tree; refusing to publish")
	return 1
}

// loadSecretsConfig reads forest.secrets.yaml from the scanned root, or returns
// an os.IsNotExist error when the file is absent.
func loadSecretsConfig(dir string) (secretsConfig, error) {
	var cfg secretsConfig
	b, err := os.ReadFile(filepath.Join(dir, "forest.secrets.yaml"))
	if err != nil {
		return cfg, err
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("parse forest.secrets.yaml: %w", err)
	}
	return cfg, nil
}

// scanSecretsContent walks the working tree under dir, untracked files
// included, and applies the built-in content rules. It never consults the git
// index: a rendered declaration that an earlier step left on disk is caught
// even though it is untracked and .gitignore could not help it. Excluded paths
// are skipped whole.
func scanSecretsContent(dir string, excludes []string) ([]secretFinding, error) {
	var findings []secretFinding
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if excludedPath(rel, excludes) {
				return fs.SkipDir
			}
			return nil
		}
		if excludedPath(rel, excludes) {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if bytes.IndexByte(b, 0) >= 0 {
			return nil // binary; not text we render
		}
		s := string(b)
		for _, m := range reMintMarker.FindAllString(s, -1) {
			findings = append(findings, secretFinding{Path: rel, Rule: "mint-marker", Match: m})
		}
		for _, m := range reMintProxy.FindAllString(s, -1) {
			findings = append(findings, secretFinding{Path: rel, Rule: "mint-proxy", Match: m})
		}
		return nil
	})
	if err != nil {
		return findings, fmt.Errorf("scan %s: %w", dir, err)
	}
	return findings, nil
}

// excludedPath reports whether a slash-form relative path falls under one of the
// exclusions, matching the path itself or any subtree beneath a directory entry.
func excludedPath(rel string, excludes []string) bool {
	for _, e := range excludes {
		e = strings.TrimSuffix(filepath.ToSlash(e), "/")
		if rel == e || strings.HasPrefix(rel, e+"/") {
			return true
		}
	}
	return false
}

// scanGeneric runs the external generic scanner over the working tree. If the
// scanner binary is absent the gate fails closed, naming the tool, rather than
// silently narrowing the scan to the factory's own content rules. The exclusions
// the content walk already honours are handed to the generic scanner too, so a
// legitimate fixture on the list cannot fail the generic pass.
func scanGeneric(dir string, excludes []string) ([]secretFinding, error) {
	bin, err := scanEnv.lookPath(secretScanner)
	if err != nil {
		return nil, fmt.Errorf("%s not found on PATH; failing closed instead of skipping the generic high-entropy scan: %w", secretScanner, err)
	}
	return scanEnv.runGeneric(bin, dir, excludes)
}

// runTrufflehog runs the trufflehog filesystem detector against the local
// working tree dir and parses its JSON findings. It never contacts a network
// service: the scan reads only local files, --no-update requests trufflehog's
// offline mode (it suppresses the self-update check that is the one network call
// the tool makes on startup), and no verification stage is engaged. Findings
// whose reported path is empty cannot be placed, but a raw match is still
// reported so the leak is not silently lost.
//
// The exclusion path is a file trufflehog reads with gitignore-style patterns,
// so the same forest.secrets.yaml list that the content walk skips is written to
// a temporary file and passed with --exclude-paths.
func runTrufflehog(bin, dir string, excludes []string) ([]secretFinding, error) {
	excludeFile, err := writeExcludePaths(excludes)
	if err != nil {
		return nil, err
	}
	defer os.Remove(excludeFile)
	args := []string{"filesystem", "--no-update", "-d", dir, "--json"}
	if excludeFile != "" {
		args = append(args, "--exclude-paths", excludeFile)
	}
	cmd := exec.Command(bin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%s failed: %w: %s", secretScanner, err, strings.TrimSpace(string(out)))
	}
	var findings []secretFinding
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var f struct {
			DetectorName   string `json:"DetectorName"`
			Raw            string `json:"Raw"`
			SourceMetadata struct {
				Data struct {
					Filesystem struct {
						File string `json:"file"`
					} `json:"Filesystem"`
				} `json:"Data"`
			} `json:"SourceMetadata"`
		}
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			continue
		}
		path := f.SourceMetadata.Data.Filesystem.File
		if path == "" {
			path = "<unknown>"
		}
		rule := f.DetectorName
		if rule == "" {
			rule = "generic-secret"
		}
		findings = append(findings, secretFinding{Path: path, Rule: rule, Match: f.Raw})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("parse %s output: %w", secretScanner, err)
	}
	return findings, nil
}

// writeExcludePaths writes the exclusion list to a temporary file trufflehog
// reads as its --exclude-paths (gitignore-style patterns, one per line) and
// returns the file's path. The caller removes it. An empty list writes no file
// and returns an empty path, which is only reached when --no-generic tests stub
// the scanner; the generic command is not invoked then.
func writeExcludePaths(excludes []string) (string, error) {
	if len(excludes) == 0 {
		return "", nil
	}
	f, err := os.CreateTemp("", "forest-excludes-*.txt")
	if err != nil {
		return "", fmt.Errorf("write scanner exclusion file: %w", err)
	}
	path := f.Name()
	if _, err := f.WriteString(strings.Join(excludes, "\n") + "\n"); err != nil {
		f.Close()
		os.Remove(path)
		return "", fmt.Errorf("write scanner exclusion file: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("close scanner exclusion file: %w", err)
	}
	return path, nil
}
