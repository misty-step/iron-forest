package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// This file owns the harness profile: which directories feed it, what a
// repository layer may contribute, how one is materialized per Run, the child's
// environment, and how the directory is collected. A Run's evidence must
// describe exactly what its agent saw, so profiles are built per Run, never
// cached per declaration.

// runProfileDir is where one Run's harness profile lives while the Run is live.
// trustedExecutable resolves a tool to a path outside the repository. Symlinks
// are followed to decide trust, because the target is what actually runs, but the
// caller's own path is returned to execute. A version-manager shim dispatches on
// its own name, so running the resolved target would run the manager instead of
// the tool.
func trustedExecutable(root, name string) (string, error) {
	path := name
	if !strings.ContainsRune(path, os.PathSeparator) {
		found, err := exec.LookPath(path)
		if err != nil {
			return "", err
		}
		path = found
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved := absolute
	if value, err := filepath.EvalSymlinks(absolute); err == nil {
		resolved = value
	}
	inside, err := pathInside(root, resolved)
	if err != nil {
		return "", err
	}
	if inside {
		return "", fmt.Errorf("refuse repository executable %s", resolved)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("%s is not executable", resolved)
	}
	return absolute, nil
}

func trustedPath(root string) (string, error) {
	entries := make([]string, 0)
	for _, entry := range filepath.SplitList(os.Getenv("PATH")) {
		if entry == "" {
			continue
		}
		absolute, err := filepath.Abs(entry)
		if err != nil {
			return "", err
		}
		// Resolve to decide trust, keep the caller's entry to hand to the child.
		// A shim directory reached through a symlink must stay a shim directory,
		// or the agent's own tools break the way the harness did.
		resolved := absolute
		if value, err := filepath.EvalSymlinks(absolute); err == nil {
			resolved = value
		}
		inside, err := pathInside(root, resolved)
		if err != nil {
			return "", err
		}
		if !inside {
			entries = append(entries, absolute)
		}
	}
	if len(entries) == 0 {
		return "", fmt.Errorf("PATH has no trusted directories")
	}
	return strings.Join(entries, string(os.PathListSeparator)), nil
}

func pathInside(root, path string) (bool, error) {
	rootPath, err := filepath.Abs(root)
	if err != nil {
		return false, err
	}
	if value, err := filepath.EvalSymlinks(rootPath); err == nil {
		rootPath = value
	}
	relative, err := filepath.Rel(rootPath, path)
	if err != nil {
		return false, err
	}
	return relative == "." || relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator)), nil
}

func runProfileDir(root, runID string) string {
	return forestPath(root, "profiles", runID)
}

// declarationProfileDir is one declaration's repository profile layer.
func declarationProfileDir(root, name string) string {
	return filepath.Join(declarationDir(root, name), "profile")
}

// sharedProfileDir is the repository layer every declaration shares.

// operatorProfile is the trusted base layer. An explicit defaults.Profile
// wins. Otherwise the host Pi profile is used, so an upgraded factory keeps
// the credentials pi already stored under ~/.pi/agent.
func operatorProfile(defaults Defaults) string {
	if defaults.Profile != "" {
		return defaults.Profile
	}
	if dir := strings.TrimSpace(os.Getenv("PI_CODING_AGENT_DIR")); dir != "" {
		return dir
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".pi", "agent")
	}
	return ""
}
func sharedProfileDir(root string) string {
	return filepath.Join(root, "agents", "_shared", "profile")
}

// errProfileAuth is rejected content: credentials never enter a repository
// layer. They live in the operator's base profile, or as mint references in
// declared environment.
var errProfileAuth = errors.New("a repository profile layer must not contain auth.json")

// scanProfileLayer validates one repository profile layer and lists its files.
// Layers are repository content, so they are checked rather than trusted: no
// credentials, no symlinks, and nothing but regular files and directories.
// Returns nil files when the layer does not exist, which is the common case.
func scanProfileLayer(dir string) ([]string, error) {
	info, err := os.Stat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("profile layer %s is not a directory", dir)
	}
	var files []string
	walkErr := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if filepath.Base(relative) == "auth.json" {
			return fmt.Errorf("%w: %s", errProfileAuth, filepath.Join(dir, relative))
		}
		switch {
		case entry.IsDir():
			return nil
		case entry.Type()&os.ModeSymlink != 0:
			return fmt.Errorf("profile layer %s contains symlink %s", dir, relative)
		case !entry.Type().IsRegular():
			return fmt.Errorf("profile layer %s contains a non-regular file %s", dir, relative)
		}
		files = append(files, relative)
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	slices.Sort(files)
	return files, nil
}

// declarationProfileFiles validates this declaration's own layer and the shared
// layer, so loading a declaration rejects bad content before any Run starts.
func declarationProfileFiles(root, name string) ([]string, error) {
	files, err := scanProfileLayer(declarationProfileDir(root, name))
	if err != nil {
		return nil, fmt.Errorf("agent %s: %w", name, err)
	}
	if _, err := scanProfileLayer(sharedProfileDir(root)); err != nil {
		return nil, fmt.Errorf("agent %s: %w", name, err)
	}
	return files, nil
}

// materializeRunProfile builds the per-Run harness profile. The operator's base
// profile (which may carry credentials) is copied first, then the shared
// repository layer, then the declaration's own layer; each later file replaces
// an earlier one. The returned manifest lists every file the profile holds, in
// sorted order, so the Run's evidence states exactly what the agent saw.
func materializeRunProfile(ctx context.Context, root, runID string, declaration Declaration, defaults Defaults) (string, []string, error) {
	target := runProfileDir(root, runID)
	type layer struct {
		dir      string
		trusted  bool
		required bool
	}
	layers := []layer{}
	if defaults.Profile != "" {
		layers = append(layers, layer{dir: defaults.Profile, trusted: true, required: true})
	} else if base := operatorProfile(defaults); base != "" {
		layers = append(layers, layer{dir: base, trusted: true})
	}
	layers = append(layers,
		layer{dir: sharedProfileDir(root)},
		layer{dir: declarationProfileDir(root, declaration.Name)},
	)
	manifest := make(map[string]struct{})
	for _, item := range layers {
		if err := ctx.Err(); err != nil {
			return "", nil, err
		}
		info, err := os.Stat(item.dir)
		if errors.Is(err, os.ErrNotExist) {
			if item.required {
				return "", nil, fmt.Errorf("read operator profile %s: %w", item.dir, err)
			}
			continue
		} else if err != nil {
			return "", nil, err
		}
		if !info.IsDir() {
			return "", nil, fmt.Errorf("profile layer %s is not a directory", item.dir)
		}
		source := item.dir
		if resolved, err := filepath.EvalSymlinks(item.dir); err == nil {
			source = resolved
		}
		if inside, err := pathInside(source, target); err != nil {
			return "", nil, err
		} else if inside {
			return "", nil, fmt.Errorf("profile layer %s contains the Run profile", item.dir)
		}
		walkErr := filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			relative, err := filepath.Rel(source, path)
			if err != nil {
				return err
			}
			if !item.trusted && filepath.Base(relative) == "auth.json" {
				return fmt.Errorf("%w: %s", errProfileAuth, filepath.Join(item.dir, relative))
			}
			destination := filepath.Join(target, relative)
			if entry.IsDir() {
				return os.MkdirAll(destination, 0o700)
			}
			if !entry.Type().IsRegular() {
				// The operator base may contain sockets or dangling links. Skip
				// them. A repository layer already failed at load for these.
				if item.trusted {
					return nil
				}
				return fmt.Errorf("profile layer %s contains a non-regular file %s", item.dir, relative)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
				return err
			}
			// Owner-only, and keep an execute bit the source already had.
			mode := os.FileMode(0o600)
			if info, err := entry.Info(); err == nil {
				mode |= info.Mode().Perm() & 0o111
			}
			if err := os.WriteFile(destination, data, mode); err != nil {
				return err
			}
			if err := os.Chmod(destination, mode); err != nil {
				return err
			}
			manifest[relative] = struct{}{}
			return nil
		})
		if walkErr != nil {
			return "", nil, walkErr
		}
	}
	if len(manifest) == 0 {
		// An empty profile still exists so the harness has a directory to write
		// its own defaults into.
		if err := os.MkdirAll(target, 0o700); err != nil {
			return "", nil, err
		}
	}
	files := make([]string, 0, len(manifest))
	for name := range manifest {
		files = append(files, name)
	}
	slices.Sort(files)
	return target, files, nil
}

// collectRunProfile removes a Run's profile. It is plain filesystem state, so
// collection is a plain removal, paired with worktree collection.
func collectRunProfile(root, runID string) error {
	err := os.RemoveAll(runProfileDir(root, runID))
	if err != nil {
		return fmt.Errorf("remove Run profile: %w", err)
	}
	return nil
}

// runEnvironment composes the child's environment: the inherited environment
// minus the variables the Kernel owns, the trusted PATH, the Run's Git identity
// and identity marker, the per-Run harness profile, and the declaration's
// declared environment.
func runEnvironment(root, name, email, runID, profileDir string, declaration Declaration) ([]string, error) {
	path, err := trustedPath(root)
	if err != nil {
		return nil, err
	}
	controlled := append([]string{"PI_CODING_AGENT_DIR="}, runnerControlledEnvPrefixes...)
	environment := make([]string, 0, len(os.Environ())+8)
	for _, value := range os.Environ() {
		keep := true
		for _, prefix := range controlled {
			if strings.HasPrefix(value, prefix) {
				keep = false
				break
			}
		}
		if keep {
			environment = append(environment, value)
		}
	}
	environment = append(environment,
		"PATH="+path,
		"GIT_AUTHOR_NAME="+name,
		"GIT_AUTHOR_EMAIL="+email,
		"GIT_COMMITTER_NAME="+name,
		"GIT_COMMITTER_EMAIL="+email,
		"FOREST_RUN_ID="+runID,
		"PI_CODING_AGENT_DIR="+profileDir,
	)
	keys := make([]string, 0, len(declaration.Env))
	for key := range declaration.Env {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		environment = append(environment, key+"="+declaration.Env[key])
	}
	return environment, nil
}
