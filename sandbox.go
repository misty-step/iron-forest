package main

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const sandboxWorkspace = "/workspace"

type childSandbox struct {
	bwrap string
	env   []string
	args  []string
}

// prepareChildSandbox builds the Linux namespace that contains an agent or a
// declared check. repoDir identifies the owning checkout; a Git workspace must
// belong to that checkout, while a non-Git Manager workspace must stay in the
// system temporary tree.
func prepareChildSandbox(repoDir, wtDir string, env []string) (childSandbox, error) {
	bwrap, err := systemExecutable("bwrap")
	if err != nil {
		return childSandbox{}, err
	}
	wtDir, err = resolveSandboxWorkspace(repoDir, wtDir)
	if err != nil {
		return childSandbox{}, err
	}
	home := childEnvValue(env, "HOME")
	if home == "" {
		return childSandbox{}, fmt.Errorf("sandbox: child HOME is empty")
	}
	for _, key := range []string{"XDG_CONFIG_HOME", "XDG_RUNTIME_DIR", "GOMODCACHE", "GOCACHE"} {
		path := childEnvValue(env, key)
		if path == "" {
			continue
		}
		if !sandboxPathWithin(home, path) {
			return childSandbox{}, fmt.Errorf("sandbox: %s must stay inside the private HOME", key)
		}
		if err := os.MkdirAll(path, 0o700); err != nil {
			return childSandbox{}, fmt.Errorf("sandbox: create %s: %w", key, err)
		}
	}

	args := []string{
		"--die-with-parent", "--new-session",
		"--unshare-user", "--unshare-pid", "--unshare-ipc", "--unshare-uts",
	}
	for _, path := range []string{"/usr", "/bin", "/sbin", "/lib", "/lib64"} {
		if _, err := os.Stat(path); err == nil {
			args = append(args, "--ro-bind", path, path)
		}
	}
	if _, err := os.Stat("/usr/local"); err == nil {
		args = append(args, "--tmpfs", "/usr/local")
	}
	args = append(args,
		"--dev", "/dev",
		"--proc", "/proc",
		"--tmpfs", "/tmp",
		"--tmpfs", "/var",
		"--dir", "/var/tmp",
		"--tmpfs", "/run",
		"--dir", "/run/user",
		"--dir", fmt.Sprintf("/run/user/%d", os.Getuid()),
		"--dir", "/etc",
	)
	for _, path := range []string{
		"/etc/alternatives",
		"/etc/ca-certificates",
		"/etc/gai.conf",
		"/etc/group",
		"/etc/hosts",
		"/etc/ld.so.cache",
		"/etc/localtime",
		"/etc/nsswitch.conf",
		"/etc/os-release",
		"/etc/passwd",
		"/etc/protocols",
		"/etc/services",
		"/etc/timezone",
	} {
		if _, err := os.Stat(path); err == nil {
			args = append(args, "--ro-bind", path, path)
		}
	}
	certificates, err := stageCertificateBundles(home)
	if err != nil {
		return childSandbox{}, err
	}
	args = append(args, certificates...)
	resolver, err := stageResolverConfig(home)
	if err != nil {
		return childSandbox{}, err
	}
	if resolver != "" {
		args = append(args, "--ro-bind", resolver, "/etc/resolv.conf")
	}

	args = append(args,
		"--bind", home, home,
		"--ro-bind", filepath.Join(home, "bin"), filepath.Join(home, "bin"),
	)
	miseData := childEnvValue(env, "MISE_DATA_DIR")
	miseSource, err := validateMiseDataDir(miseData)
	if err != nil {
		return childSandbox{}, err
	}
	miseMounts, err := stageToolchainData(home, "mise-data", miseSource, miseData, "installs", "shims")
	if err != nil {
		return childSandbox{}, err
	}
	args = append(args, miseMounts...)
	if rustup := childEnvValue(env, "RUSTUP_HOME"); rustup != "" {
		rustupSource, err := validateRustupHome(rustup)
		if err != nil {
			return childSandbox{}, err
		}
		if toolchain := rustupDefaultToolchain(rustupSource); toolchain != "" {
			env = append(env, "RUSTUP_TOOLCHAIN="+toolchain)
		}
		rustupMounts, err := stageToolchainData(home, "rustup-data", rustupSource, rustup, "toolchains")
		if err != nil {
			return childSandbox{}, err
		}
		args = append(args, rustupMounts...)
	}

	// Stage only executable files from explicit Host PATH directories. Sibling
	// files, including tool credentials, never cross the boundary.
	for i, path := range filepath.SplitList(childEnvValue(env, "PATH")) {
		path = filepath.Clean(path)
		if path == "." || path == "" || sandboxSystemPath(path) ||
			sandboxPathWithin(home, path) || sandboxPathWithin(miseData, path) {
			continue
		}
		if _, err := os.Stat(path); os.IsNotExist(err) {
			continue
		}
		stage, err := stageHostExecutables(home, i, path)
		if err != nil {
			return childSandbox{}, err
		}
		args = append(args,
			"--ro-bind", stage, stage,
			"--ro-bind", stage, path,
		)
	}
	for _, path := range []string{"/usr/bin/gh", "/bin/gh"} {
		if _, err := os.Stat(path); err == nil {
			args = append(args, "--ro-bind", filepath.Join(home, "bin", "gh"), path)
		}
	}

	args = append(args, "--bind", wtDir, sandboxWorkspace)
	args, err = appendSandboxGit(args, repoDir, wtDir, home)
	if err != nil {
		return childSandbox{}, err
	}
	args = append(args, "--chdir", sandboxWorkspace)
	return childSandbox{bwrap: bwrap, env: env, args: args}, nil
}

func sandboxPathWithin(root, path string) bool {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func stageCertificateBundles(home string) ([]string, error) {
	targets := []string{
		"/etc/ssl/certs/ca-certificates.crt",
		"/etc/ssl/cert.pem",
		"/etc/pki/tls/certs/ca-bundle.crt",
		"/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem",
	}
	allowedRoots := []string{
		"/etc/ssl/certs",
		"/etc/pki/tls/certs",
		"/etc/pki/ca-trust/extracted",
	}
	args := []string{
		"--dir", "/etc/ssl",
		"--dir", "/etc/ssl/certs",
		"--dir", "/etc/pki",
		"--dir", "/etc/pki/tls",
		"--dir", "/etc/pki/tls/certs",
		"--dir", "/etc/pki/ca-trust",
		"--dir", "/etc/pki/ca-trust/extracted",
		"--dir", "/etc/pki/ca-trust/extracted/pem",
	}
	found := 0
	for i, target := range targets {
		source, err := filepath.EvalSymlinks(target)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("sandbox: resolve CA bundle %q: %w", target, err)
		}
		allowed := false
		for _, root := range allowedRoots {
			allowed = allowed || sandboxPathWithin(root, source)
		}
		info, err := os.Stat(source)
		if !allowed || err != nil || !info.Mode().IsRegular() || info.Size() > 16<<20 {
			return nil, fmt.Errorf("sandbox: CA bundle %q has an invalid source", target)
		}
		body, err := os.ReadFile(source)
		if err != nil {
			return nil, fmt.Errorf("sandbox: read CA bundle %q: %w", target, err)
		}
		staged := filepath.Join(home, fmt.Sprintf("ca-bundle-%d.pem", i))
		if err := os.WriteFile(staged, body, 0o400); err != nil {
			return nil, fmt.Errorf("sandbox: stage CA bundle %q: %w", target, err)
		}
		args = append(args, "--ro-bind", staged, target)
		found++
	}
	if found == 0 {
		return nil, fmt.Errorf("sandbox: no supported system CA bundle found")
	}
	return args, nil
}

func stageResolverConfig(home string) (string, error) {
	source, err := filepath.EvalSymlinks("/etc/resolv.conf")
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("sandbox: resolve /etc/resolv.conf: %w", err)
	}
	allowed := source == "/etc/resolv.conf"
	for _, root := range []string{"/run/systemd/resolve", "/run/NetworkManager", "/run/resolvconf"} {
		allowed = allowed || sandboxPathWithin(root, source)
	}
	if !allowed {
		return "", fmt.Errorf("sandbox: resolver source %q is outside system resolver roots", source)
	}
	info, err := os.Stat(source)
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("sandbox: resolver source %q is not a regular file", source)
	}
	body, err := os.ReadFile(source)
	if err != nil {
		return "", fmt.Errorf("sandbox: read resolver configuration: %w", err)
	}
	dest := filepath.Join(home, "resolv.conf")
	if err := os.WriteFile(dest, body, 0o444); err != nil {
		return "", fmt.Errorf("sandbox: stage resolver configuration: %w", err)
	}
	return dest, nil
}

func resolveSandboxWorkspace(repoDir, wtDir string) (string, error) {
	info, err := os.Lstat(wtDir)
	if err != nil {
		return "", fmt.Errorf("sandbox: inspect workspace %q: %w", wtDir, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("sandbox: workspace %q must be a real directory", wtDir)
	}
	workspace, err := validateSandboxMountDir("workspace", wtDir)
	if err != nil {
		return "", err
	}
	gitEntry := filepath.Join(workspace, ".git")
	if _, err := os.Lstat(gitEntry); os.IsNotExist(err) {
		tempRoot, resolveErr := filepath.EvalSymlinks(os.TempDir())
		if resolveErr != nil || !sandboxPathWithin(tempRoot, workspace) {
			return "", fmt.Errorf("sandbox: non-Git workspace %q is outside the system temporary tree", workspace)
		}
		return workspace, nil
	} else if err != nil {
		return "", fmt.Errorf("sandbox: inspect workspace Git entry: %w", err)
	}

	repo, err := filepath.Abs(repoDir)
	if err == nil {
		repo, err = filepath.EvalSymlinks(repo)
	}
	if err != nil {
		return "", fmt.Errorf("sandbox: resolve owning checkout %q: %w", repoDir, err)
	}
	if workspace == repo {
		return workspace, nil
	}
	worktreeRoot, err := filepath.EvalSymlinks(filepath.Join(repo, WorkspaceDir, "worktrees"))
	if err != nil || !sandboxPathWithin(worktreeRoot, workspace) {
		return "", fmt.Errorf("sandbox: Git workspace %q is outside the owning checkout worktree root", workspace)
	}
	return workspace, nil
}

func validateSandboxMountDir(label, path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("sandbox: %s path %q is not absolute", label, path)
	}
	source, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("sandbox: resolve %s path %q: %w", label, path, err)
	}
	info, err := os.Stat(source)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("sandbox: %s path %q is not a directory", label, path)
	}
	for _, root := range []string{"/", "/home", "/root", "/run", "/proc", "/dev", "/tmp", "/var", "/etc"} {
		if source == root || root != "/" && sandboxPathWithin(root, source) &&
			(root == "/run" || root == "/proc" || root == "/dev" || root == "/etc") {
			return "", fmt.Errorf("sandbox: %s path %q overlaps protected Host state", label, path)
		}
	}
	if operatorHome, err := os.UserHomeDir(); err == nil && source == filepath.Clean(operatorHome) {
		return "", fmt.Errorf("sandbox: %s path %q is the operator HOME", label, path)
	}
	return source, nil
}

func validateMiseDataDir(path string) (string, error) {
	source, err := validateSandboxMountDir("MISE_DATA_DIR", path)
	if err != nil {
		return "", err
	}
	if base := filepath.Base(source); base != "mise" && base != ".mise" {
		return "", fmt.Errorf("sandbox: MISE_DATA_DIR %q is not a mise data root", path)
	}
	if info, err := os.Stat(filepath.Join(source, "installs")); err != nil || !info.IsDir() {
		return "", fmt.Errorf("sandbox: MISE_DATA_DIR %q has no installs directory", path)
	}
	return source, nil
}

func validateRustupHome(path string) (string, error) {
	source, err := validateSandboxMountDir("RUSTUP_HOME", path)
	if err != nil {
		return "", err
	}
	if base := filepath.Base(source); base != "rustup" && base != ".rustup" {
		return "", fmt.Errorf("sandbox: RUSTUP_HOME %q is not a rustup data root", path)
	}
	if info, err := os.Stat(filepath.Join(source, "toolchains")); err != nil || !info.IsDir() {
		return "", fmt.Errorf("sandbox: RUSTUP_HOME %q has no toolchains directory", path)
	}
	return source, nil
}

func rustupDefaultToolchain(root string) string {
	rootFS, err := os.OpenRoot(root)
	if err != nil {
		return ""
	}
	defer rootFS.Close()
	body, err := rootFS.ReadFile("settings.toml")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(body), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "default_toolchain" {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if value == "" || value == "." || value == ".." || strings.IndexFunc(value, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' ||
				r >= '0' && r <= '9' || strings.ContainsRune("._-", r))
		}) >= 0 {
			return ""
		}
		if info, err := rootFS.Stat(filepath.Join("toolchains", value)); err == nil && info.IsDir() {
			return value
		}
		return ""
	}
	return ""
}

func stageToolchainData(
	home, stageName, source, target string,
	children ...string,
) ([]string, error) {
	stage := filepath.Join(home, stageName)
	if err := os.Mkdir(stage, 0o700); err != nil {
		return nil, fmt.Errorf("sandbox: create %s staging root: %w", stageName, err)
	}
	args := []string{
		"--ro-bind", stage, stage,
		"--ro-bind", stage, target,
	}
	for _, child := range children {
		sourceChild, err := filepath.EvalSymlinks(filepath.Join(source, child))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("sandbox: resolve %s/%s: %w", stageName, child, err)
		}
		if !sandboxPathWithin(filepath.Join(source, child), sourceChild) {
			return nil, fmt.Errorf("sandbox: %s/%s escapes its declared subtree", stageName, child)
		}
		if info, err := os.Stat(sourceChild); err != nil || !info.IsDir() {
			continue
		}
		if err := validateSandboxTreeLinks(stageName, source, sourceChild); err != nil {
			return nil, err
		}
		if err := os.Mkdir(filepath.Join(stage, child), 0o700); err != nil {
			return nil, fmt.Errorf("sandbox: stage %s/%s: %w", stageName, child, err)
		}
		args = append(args, "--ro-bind", sourceChild, filepath.Join(target, child))
	}
	return args, nil
}

func validateSandboxTreeLinks(label, root, tree string) error {
	return filepath.WalkDir(tree, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("sandbox: inspect %s path %q: %w", label, path, walkErr)
		}
		if entry.Type()&os.ModeSymlink == 0 {
			return nil
		}
		target, err := os.Readlink(path)
		if err != nil {
			return fmt.Errorf("sandbox: read %s symbolic link %q: %w", label, path, err)
		}
		candidate := target
		if !filepath.IsAbs(candidate) {
			candidate = filepath.Join(filepath.Dir(path), candidate)
		}
		if !sandboxPathWithin(root, candidate) && sandboxCrossMountPath(candidate) {
			return fmt.Errorf("sandbox: %s symbolic link %q reaches another sandbox mount", label, path)
		}
		resolved, err := filepath.EvalSymlinks(path)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("sandbox: resolve %s symbolic link %q: %w", label, path, err)
		}
		if !sandboxPathWithin(root, resolved) && sandboxCrossMountPath(resolved) {
			return fmt.Errorf("sandbox: %s symbolic link %q reaches another sandbox mount", label, path)
		}
		return nil
	})
}

func sandboxCrossMountPath(path string) bool {
	if sandboxSystemPath(path) {
		return true
	}
	for _, root := range []string{"/etc", sandboxWorkspace, "/proc", "/dev"} {
		if sandboxPathWithin(root, path) {
			return true
		}
	}
	return false
}

func stageHostExecutables(home string, index int, path string) (string, error) {
	sourceDir, err := validateSandboxMountDir("FOREST_CHECK_PATH", path)
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(sourceDir)
	if err != nil {
		return "", fmt.Errorf("sandbox: read FOREST_CHECK_PATH entry %q: %w", path, err)
	}
	stage := filepath.Join(home, "host-bin", fmt.Sprintf("%d", index))
	if err := os.MkdirAll(stage, 0o700); err != nil {
		return "", fmt.Errorf("sandbox: stage FOREST_CHECK_PATH entry %q: %w", path, err)
	}
	copies := make(map[string]string)
	for _, entry := range entries {
		source, err := filepath.EvalSymlinks(filepath.Join(sourceDir, entry.Name()))
		if err != nil {
			continue
		}
		if !sandboxPathWithin(sourceDir, source) {
			return "", fmt.Errorf("sandbox: Host executable %q escapes FOREST_CHECK_PATH %q", entry.Name(), path)
		}
		info, err := os.Stat(source)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			continue
		}
		dest := filepath.Join(stage, entry.Name())
		if first := copies[source]; first != "" {
			if err := os.Link(first, dest); err == nil {
				continue
			}
		}
		if err := copySandboxExecutable(source, dest); err != nil {
			return "", fmt.Errorf("sandbox: stage Host executable %q: %w", source, err)
		}
		copies[source] = dest
	}
	return stage, nil
}

func copySandboxExecutable(source, dest string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o555)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func (s childSandbox) commandWithFiles(
	ctx context.Context,
	files []*os.File,
	name string,
	args ...string,
) (*exec.Cmd, error) {
	var target string
	switch name {
	case "sh":
		target = "/bin/sh"
	case "opencode":
		target = filepath.Join(childEnvValue(s.env, "HOME"), "bin", "opencode")
		if !sandboxPathWithin(childEnvValue(s.env, "HOME"), target) {
			return nil, fmt.Errorf("sandbox: opencode target escaped the private home")
		}
		if info, err := os.Stat(target); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return nil, fmt.Errorf("sandbox: staged opencode is unavailable")
		}
	default:
		return nil, fmt.Errorf("sandbox: unsupported command %q", name)
	}
	bwrapArgs := append([]string(nil), s.args...)
	if len(files) > 0 {
		if len(files) != 1 {
			return nil, fmt.Errorf("sandbox: one launch-status descriptor is required")
		}
		bwrapArgs = append(bwrapArgs, "--json-status-fd", "3")
	}
	bwrapArgs = append(bwrapArgs, "--", target)
	bwrapArgs = append(bwrapArgs, args...)
	cmd := exec.CommandContext(ctx, s.bwrap, bwrapArgs...)
	cmd.Env = s.env
	cmd.Dir = "/"
	cmd.ExtraFiles = files
	return cmd, nil
}

// appendSandboxGit exposes only the owning checkout's read-only history and
// diff metadata. It masks configuration, hooks, and sibling worktree
// administration so no checkout credential or Host-local hook reaches a child.
func appendSandboxGit(args []string, repoDir, wtDir, home string) ([]string, error) {
	gitEntry := filepath.Join(wtDir, ".git")
	entryInfo, err := os.Lstat(gitEntry)
	if os.IsNotExist(err) {
		return args, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sandbox: inspect worktree Git entry: %w", err)
	}
	if entryInfo.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("sandbox: worktree Git entry must not be a symbolic link")
	}

	expectedCommon, err := sandboxGitOut(repoDir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return nil, fmt.Errorf("sandbox: resolve owning Git common directory: %w", err)
	}
	expectedCommon, err = validateSandboxMountDir("owning Git common directory", expectedCommon)
	if err != nil {
		return nil, err
	}
	common, err := sandboxGitOut(wtDir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return nil, fmt.Errorf("sandbox: resolve workspace Git common directory: %w", err)
	}
	common, err = validateSandboxMountDir("Git common directory", common)
	if err != nil || filepath.Base(common) != ".git" {
		return nil, fmt.Errorf("sandbox: refuse unexpected Git common directory %q", common)
	}
	if common != expectedCommon {
		return nil, fmt.Errorf("sandbox: Git common directory %q does not belong to owning checkout %q", common, repoDir)
	}
	gitDir, err := sandboxGitOut(wtDir, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return nil, fmt.Errorf("sandbox: resolve Git directory: %w", err)
	}
	gitDir, err = validateSandboxMountDir("Git directory", gitDir)
	if err != nil {
		return nil, err
	}
	if gitDir != common && !sandboxPathWithin(filepath.Join(common, "worktrees"), gitDir) {
		return nil, fmt.Errorf("sandbox: Git directory %q is outside the managed common directory", gitDir)
	}
	if err := rejectSandboxTreeSymlinks("Git common directory", common); err != nil {
		return nil, err
	}

	view, mounts, err := stageSandboxGitView(home, common, gitDir)
	if err != nil {
		return nil, err
	}
	args = append(args, "--ro-bind", view, view, "--ro-bind", view, common)
	for _, mount := range mounts {
		args = append(args, "--ro-bind", mount.source, filepath.Join(common, mount.relative))
	}
	if entryInfo.IsDir() {
		alias := filepath.Join(sandboxWorkspace, ".git")
		args = append(args, "--ro-bind", view, alias)
		for _, mount := range mounts {
			args = append(args, "--ro-bind", mount.source, filepath.Join(alias, mount.relative))
		}
	} else if entryInfo.Mode().IsRegular() {
		args = append(args, "--ro-bind", gitEntry, filepath.Join(sandboxWorkspace, ".git"))
	} else {
		return nil, fmt.Errorf("sandbox: worktree Git entry is not regular")
	}
	return args, nil
}

type sandboxGitMount struct {
	source   string
	relative string
}

func stageSandboxGitView(home, common, gitDir string) (string, []sandboxGitMount, error) {
	view := filepath.Join(home, "git")
	if err := os.Mkdir(view, 0o700); err != nil {
		return "", nil, fmt.Errorf("sandbox: create Git view: %w", err)
	}
	var mounts []sandboxGitMount
	seen := make(map[string]bool)
	add := func(relative string, directory bool) error {
		relative = filepath.Clean(relative)
		if relative == "." || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return fmt.Errorf("sandbox: invalid Git view path %q", relative)
		}
		if seen[relative] {
			return nil
		}
		source := filepath.Join(common, relative)
		info, err := os.Lstat(source)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("sandbox: inspect Git metadata %q: %w", source, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || info.IsDir() != directory ||
			(!directory && !info.Mode().IsRegular()) {
			return fmt.Errorf("sandbox: Git metadata %q has an unsafe type", source)
		}
		target := filepath.Join(view, relative)
		if directory {
			err = os.MkdirAll(target, 0o700)
		} else {
			err = os.MkdirAll(filepath.Dir(target), 0o700)
			if err == nil {
				err = os.WriteFile(target, nil, 0o600)
			}
		}
		if err != nil {
			return fmt.Errorf("sandbox: stage Git metadata %q: %w", relative, err)
		}
		seen[relative] = true
		mounts = append(mounts, sandboxGitMount{source: source, relative: relative})
		return nil
	}
	for _, relative := range []string{"objects", "refs", "logs"} {
		if err := add(relative, true); err != nil {
			return "", nil, err
		}
	}
	for _, relative := range []string{
		"HEAD", "index", "packed-refs", "shallow", "ORIG_HEAD", "MERGE_HEAD",
		"CHERRY_PICK_HEAD", "REVERT_HEAD", "BISECT_LOG", "info/exclude", "info/refs",
	} {
		if err := add(relative, false); err != nil {
			return "", nil, err
		}
	}
	configs := []string{"config", "config.worktree"}
	if gitDir != common {
		relative, err := filepath.Rel(common, gitDir)
		if err != nil || relative == "." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return "", nil, fmt.Errorf("sandbox: invalid linked-worktree Git directory %q", gitDir)
		}
		for _, name := range []string{
			"HEAD", "index", "commondir", "gitdir", "ORIG_HEAD", "MERGE_HEAD",
			"CHERRY_PICK_HEAD", "REVERT_HEAD", "BISECT_LOG",
		} {
			if err := add(filepath.Join(relative, name), false); err != nil {
				return "", nil, err
			}
		}
		if err := add(filepath.Join(relative, "logs"), true); err != nil {
			return "", nil, err
		}
		configs = append(configs, filepath.Join(relative, "config.worktree"))
	}
	for _, relative := range configs {
		target := filepath.Join(view, relative)
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return "", nil, fmt.Errorf("sandbox: stage Git config mask: %w", err)
		}
		if err := os.WriteFile(target, nil, 0o600); err != nil {
			return "", nil, fmt.Errorf("sandbox: stage Git config mask: %w", err)
		}
	}
	return view, mounts, nil
}

func rejectSandboxTreeSymlinks(label, tree string) error {
	return filepath.WalkDir(tree, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("sandbox: inspect %s path %q: %w", label, path, walkErr)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("sandbox: %s path %q must not be a symbolic link", label, path)
		}
		return nil
	})
}

func sandboxGitOut(repo string, args ...string) (string, error) {
	git, err := systemExecutable("git")
	if err != nil {
		return "", err
	}
	cmd := exec.Command(git, hostGitArgs(repo, args...)...)
	env := make([]string, 0, len(os.Environ())+3)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "GIT_") {
			continue
		}
		env = append(env, entry)
	}
	cmd.Env = append(env,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
	)
	out, err := runOutput(cmd)
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

func childEnvValue(env []string, key string) string {
	var found string
	for _, entry := range env {
		k, value, ok := strings.Cut(entry, "=")
		if ok && k == key {
			found = value
		}
	}
	return found
}

func systemExecutable(name string) (string, error) {
	for _, dir := range []string{"/usr/bin", "/bin", "/usr/sbin", "/sbin"} {
		path := filepath.Join(dir, name)
		if info, err := os.Stat(path); err == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0 {
			return path, nil
		}
	}
	return "", fmt.Errorf("locate %s in the system runtime", name)
}

func sandboxSystemPath(path string) bool {
	path = filepath.Clean(path)
	if path == "/usr/local" || strings.HasPrefix(path, "/usr/local"+string(filepath.Separator)) {
		return false
	}
	for _, root := range []string{"/usr", "/bin", "/sbin", "/lib", "/lib64"} {
		if path == root || strings.HasPrefix(path, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func verifyChildSandbox(repoDir string) error {
	env, cleanup, err := childEnvironment(nil)
	if err != nil {
		return err
	}
	defer cleanup()
	sandbox, err := prepareChildSandbox(repoDir, repoDir, env)
	if err != nil {
		return err
	}
	cmd, err := sandbox.commandWithFiles(context.Background(), nil, "sh", "-c", "true")
	if err != nil {
		return err
	}
	if out, err := runCombinedOutput(cmd); err != nil {
		return fmt.Errorf("start bwrap: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
