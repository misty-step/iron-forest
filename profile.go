package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// This file owns the harness profile: which directories feed it, what a
// repository layer may contribute, how one is materialized per Run, and the
// child's environment. A Run's evidence must describe exactly what its agent
// saw, so profiles are built per Run, never cached per declaration.

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
		if err := validateRepositoryProfileEntry(dir, relative, entry); err != nil {
			return err
		}
		if !entry.IsDir() {
			files = append(files, relative)
		}
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	slices.Sort(files)
	return files, nil
}

func validateRepositoryProfileEntry(dir, relative string, entry fs.DirEntry) error {
	switch {
	case filepath.Base(relative) == "auth.json":
		return fmt.Errorf("%w: %s", errProfileAuth, filepath.Join(dir, relative))
	case entry.IsDir():
		return nil
	case entry.Type()&os.ModeSymlink != 0:
		return fmt.Errorf("profile layer %s contains a symlink %s", dir, relative)
	case !entry.Type().IsRegular():
		return fmt.Errorf("profile layer %s contains a non-regular file %s", dir, relative)
	default:
		return nil
	}
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

const (
	maxProfileFiles         = 4096
	maxProfileFileBytes     = 16 << 20
	maxProfileBytes         = 64 << 20
	maxProfileManifestBytes = 512 << 10
)

type profileLayerKind uint8

const (
	operatorProfileLayer profileLayerKind = iota
	repositoryProfileLayer
)

type profileLayer struct {
	dir      string
	kind     profileLayerKind
	required bool
}

type profileBudget struct {
	files         int
	bytes         int64
	manifestBytes int
}

func (b *profileBudget) add(relative string, size int64) error {
	if size < 0 || size > maxProfileFileBytes {
		return fmt.Errorf("profile file %s exceeds %d bytes", relative, maxProfileFileBytes)
	}
	b.files++
	b.bytes += size
	b.manifestBytes += len(relative)*6 + 3
	if b.files > maxProfileFiles || b.bytes > maxProfileBytes || b.manifestBytes > maxProfileManifestBytes {
		return fmt.Errorf("profile exceeds limits: files=%d bytes=%d manifest=%d", b.files, b.bytes, b.manifestBytes)
	}
	return nil
}

func pruneProfileManifest(manifest map[string]struct{}, relative string) {
	prefix := relative + string(os.PathSeparator)
	for path := range manifest {
		if path == relative || strings.HasPrefix(path, prefix) {
			delete(manifest, path)
		}
	}
}

func openProfileDirectory(parent *os.Root, name string, mode os.FileMode) (*os.Root, error) {
	if err := parent.Mkdir(name, mode); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	info, err := parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("profile path %s is not a real directory", name)
	}
	return parent.OpenRoot(name)
}

func createRunProfileRoot(root, runID string) (*os.Root, string, error) {
	repository, err := os.OpenRoot(root)
	if err != nil {
		return nil, "", err
	}
	defer repository.Close()
	forest, err := openProfileDirectory(repository, workspaceName, 0o755)
	if err != nil {
		return nil, "", err
	}
	defer forest.Close()
	profiles, err := openProfileDirectory(forest, "profiles", 0o700)
	if err != nil {
		return nil, "", err
	}
	defer profiles.Close()
	if err := profiles.Mkdir(runID, 0o700); err != nil {
		return nil, "", fmt.Errorf("create Run profile: %w", err)
	}
	target, err := profiles.OpenRoot(runID)
	if err != nil {
		return nil, "", err
	}
	return target, runProfileDir(root, runID), nil
}

// materializeRunProfile builds the per-Run harness profile. The trusted base
// is copied first, then the shared repository layer, then the declaration's
// layer. Later files replace earlier paths.
func materializeRunProfile(ctx context.Context, root, runID string, declaration Declaration) (string, []string, error) {
	target, targetPath, err := createRunProfileRoot(root, runID)
	if err != nil {
		return "", nil, err
	}
	defer target.Close()
	layers := []profileLayer{}
	if declaration.BaseProfile != "" {
		layers = append(layers, profileLayer{
			dir:      declaration.BaseProfile,
			kind:     operatorProfileLayer,
			required: declaration.BaseProfileRequired,
		})
	}
	layers = append(layers,
		profileLayer{dir: sharedProfileDir(root), kind: repositoryProfileLayer},
		profileLayer{dir: declarationProfileDir(root, declaration.Name), kind: repositoryProfileLayer},
	)
	manifest := make(map[string]struct{})
	budget := profileBudget{}
	for _, layer := range layers {
		if err := copyProfileLayer(ctx, target, targetPath, layer, manifest, &budget); err != nil {
			return "", nil, err
		}
	}
	files := make([]string, 0, len(manifest))
	for name := range manifest {
		files = append(files, name)
	}
	slices.Sort(files)
	return targetPath, files, nil
}

func copyProfileLayer(ctx context.Context, target *os.Root, targetPath string, layer profileLayer, manifest map[string]struct{}, budget *profileBudget) error {
	source := layer.dir
	var info os.FileInfo
	var err error
	if layer.kind == operatorProfileLayer {
		info, err = os.Stat(source)
		if err == nil {
			source, err = filepath.EvalSymlinks(source)
		}
	} else {
		info, err = os.Lstat(source)
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("profile layer %s contains a symlink", layer.dir)
		}
	}
	if errors.Is(err, os.ErrNotExist) {
		if layer.required {
			return fmt.Errorf("read operator profile %s: %w", layer.dir, err)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("profile layer %s is not a directory", layer.dir)
	}
	if inside, err := pathInside(source, targetPath); err != nil {
		return err
	} else if inside {
		return fmt.Errorf("profile layer %s contains the Run profile", layer.dir)
	}
	sourceRoot, err := os.OpenRoot(source)
	if err != nil {
		return err
	}
	defer sourceRoot.Close()
	return fs.WalkDir(sourceRoot.FS(), ".", func(relative string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if layer.kind == operatorProfileLayer && (relative == "sessions" || strings.HasPrefix(relative, "sessions"+string(os.PathSeparator))) {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if relative == "." {
			return nil
		}
		if layer.kind == repositoryProfileLayer {
			if err := validateRepositoryProfileEntry(layer.dir, relative, entry); err != nil {
				return err
			}
		}
		if entry.IsDir() {
			existing, err := target.Lstat(relative)
			switch {
			case errors.Is(err, os.ErrNotExist):
			case err != nil:
				return err
			case existing.IsDir():
				return nil
			default:
				if err := target.RemoveAll(relative); err != nil {
					return err
				}
				pruneProfileManifest(manifest, relative)
			}
			return target.MkdirAll(relative, 0o700)
		}
		var file *os.File
		if entry.Type()&os.ModeSymlink != 0 {
			file, err = os.Open(filepath.Join(source, relative))
		} else {
			file, err = sourceRoot.Open(relative)
		}
		if err != nil {
			return err
		}
		fileInfo, statErr := file.Stat()
		if statErr != nil {
			return errors.Join(statErr, file.Close())
		}
		if !fileInfo.Mode().IsRegular() {
			closeErr := file.Close()
			if layer.kind == operatorProfileLayer {
				return closeErr
			}
			return errors.Join(fmt.Errorf("profile layer %s contains a non-regular file %s", layer.dir, relative), closeErr)
		}
		data, readErr := io.ReadAll(io.LimitReader(file, maxProfileFileBytes+1))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil {
			return errors.Join(readErr, closeErr)
		}
		if err := budget.add(relative, int64(len(data))); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := target.RemoveAll(relative); err != nil {
			return err
		}
		pruneProfileManifest(manifest, relative)
		mode := os.FileMode(0o600) | fileInfo.Mode().Perm()&0o111
		if err := target.WriteFile(relative, data, mode); err != nil {
			return err
		}
		if err := target.Chmod(relative, mode); err != nil {
			return err
		}
		manifest[relative] = struct{}{}
		return nil
	})
}

func childEnvironment() []string {
	environment := make([]string, 0, len(os.Environ()))
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		if key == "HOME" || !slices.Contains(blockedEnvNames, key) {
			environment = append(environment, value)
		}
	}
	return environment
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
	environment := childEnvironment()
	environment = append(environment,
		"PATH="+path,
		"GIT_AUTHOR_NAME="+name,
		"GIT_AUTHOR_EMAIL="+email,
		"GIT_COMMITTER_NAME="+name,
		"GIT_COMMITTER_EMAIL="+email,
		"FOREST_RUN_ID="+runID,
		"PI_CODING_AGENT_DIR="+profileDir,
	)
	for _, key := range envKeys(declaration.Env) {
		environment = append(environment, key+"="+declaration.Env[key])
	}
	return environment, nil
}
