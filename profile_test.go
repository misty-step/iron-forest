package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeAgentFiles(t *testing.T, root, name, frontmatter, body, task string) {
	t.Helper()
	dir := filepath.Join(root, "agents", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent.md"), []byte("---\n"+frontmatter+"---\n"+body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "task.md"), []byte(task), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestModelChainResolvesDeclarationThenDefaultsThenBuiltIn(t *testing.T) {
	t.Setenv("FOREST_DEFAULTS", "")
	root := t.TempDir()
	writeAgentFiles(t, root, "builder", "thinking: low\n", "System rules\n", "Select one item.")
	declaration, err := loadDeclaration(root, "builder")
	if err != nil {
		t.Fatal(err)
	}
	if declaration.Model != defaultModel || declaration.ModelSource != "built-in" {
		t.Fatalf("empty chain: model=%q source=%q, want built-in %q", declaration.Model, declaration.ModelSource, defaultModel)
	}

	if err := os.WriteFile(filepath.Join(root, "forest.defaults.yaml"), []byte("model: defaults/model\nthinking: medium\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	declaration, err = loadDeclaration(root, "builder")
	if err != nil {
		t.Fatal(err)
	}
	if declaration.Model != "defaults/model" || declaration.ModelSource != "defaults" || declaration.Thinking != "low" {
		t.Fatalf("defaults layer: %#v", declaration)
	}

	writeAgentFiles(t, root, "builder", "model: declared/model\n", "System rules\n", "Select one item.")
	declaration, err = loadDeclaration(root, "builder")
	if err != nil {
		t.Fatal(err)
	}
	if declaration.Model != "declared/model" || declaration.ModelSource != "declaration" {
		t.Fatalf("declaration layer: %#v", declaration)
	}
}

func TestDefaultsFileIsOptionalAndFOREST_DEFAULTSWins(t *testing.T) {
	t.Setenv("FOREST_DEFAULTS", "")
	root := t.TempDir()
	defaults, source, err := loadDefaults(root)
	if err != nil || source != "" || defaults != (Defaults{}) {
		t.Fatalf("absent defaults: %#v source=%q err=%v", defaults, source, err)
	}
	checkout := filepath.Join(root, "forest.defaults.yaml")
	if err := os.WriteFile(checkout, []byte("model: checkout/model\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	override := filepath.Join(t.TempDir(), "host.yaml")
	if err := os.WriteFile(override, []byte("model: host/model\nprofile: profiles/base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FOREST_DEFAULTS", override)
	defaults, source, err = loadDefaults(root)
	if err != nil {
		t.Fatal(err)
	}
	if source != override || defaults.Model != "host/model" {
		t.Fatalf("FOREST_DEFAULTS: %#v source=%q", defaults, source)
	}
	if defaults.Profile != filepath.Join(root, "profiles/base") {
		t.Fatalf("relative profile=%q, want resolved against checkout", defaults.Profile)
	}
	t.Setenv("FOREST_DEFAULTS", "missing.yaml")
	if _, _, err := loadDefaults(root); err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing FOREST_DEFAULTS err=%v, want not exist", err)
	}
	t.Setenv("FOREST_DEFAULTS", "host.yaml")
	if err := os.WriteFile(filepath.Join(root, "host.yaml"), []byte("model: relative/model\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	defaults, source, err = loadDefaults(root)
	if err != nil || defaults.Model != "relative/model" || source != filepath.Join(root, "host.yaml") {
		t.Fatalf("relative FOREST_DEFAULTS: %#v source=%q err=%v", defaults, source, err)
	}
}

func TestEmptyOrCommentOnlyDefaultsAreZero(t *testing.T) {
	t.Setenv("FOREST_DEFAULTS", "")
	root := t.TempDir()
	path := filepath.Join(root, "forest.defaults.yaml")
	for _, body := range []string{"", "# comment only\n"} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		defaults, source, err := loadDefaults(root)
		if err != nil || defaults != (Defaults{}) || source != path {
			t.Fatalf("body=%q defaults=%#v source=%q err=%v", body, defaults, source, err)
		}
	}
}

func TestDeclarationEnvPreservesOpaqueValuesAndRejectsOwnedNames(t *testing.T) {
	root := t.TempDir()
	writeAgentFiles(t, root, "builder", "model: local\nenv:\n  REFERENCE: \"Vendor:Open.Router__\"\n  NOTE: \"  hello  \"\n", "System rules\n", "Select one item.")
	declaration, err := loadDeclaration(root, "builder")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"REFERENCE": "Vendor:Open.Router__", "NOTE": "  hello  "}
	if !reflect.DeepEqual(declaration.Env, want) {
		t.Fatalf("env=%v, want %v", declaration.Env, want)
	}
	if !reflect.DeepEqual(declaration.EnvKeys, []string{"NOTE", "REFERENCE"}) {
		t.Fatalf("env keys=%v", declaration.EnvKeys)
	}
	encoded, err := json.Marshal(declaration)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "hello") || strings.Contains(string(encoded), "Vendor:") {
		t.Fatalf("JSON leaked env values: %s", encoded)
	}

	writeAgentFiles(t, root, "builder", "model: local\nenv:\n  PATH: /tmp\n", "System rules\n", "Select one item.")
	if _, err := loadDeclaration(root, "builder"); err == nil || !strings.Contains(err.Error(), "Kernel owns") {
		t.Fatalf("owned env err=%v, want Kernel owns", err)
	}

	writeAgentFiles(t, root, "builder", "model: local\nenv:\n  NOTE: one\n  NOTE: two\n", "System rules\n", "Select one item.")
	if _, err := loadDeclaration(root, "builder"); err == nil || !strings.Contains(err.Error(), "declared twice") {
		t.Fatalf("duplicate env err=%v, want declared twice", err)
	}
	writeAgentFiles(t, root, "builder", "model: local\nenv:\n", "System rules\n", "Select one item.")
	declaration, err = loadDeclaration(root, "builder")
	if err != nil || declaration.Env != nil {
		t.Fatalf("empty env=%v err=%v", declaration.Env, err)
	}
}

func TestRepositoryProfileRejectsAuthAndSymlinks(t *testing.T) {
	root := t.TempDir()
	writeAgentFiles(t, root, "builder", "model: local\n", "System rules\n", "Select one item.")
	layer := filepath.Join(root, "agents", "builder", "profile")
	if err := os.MkdirAll(layer, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(layer, "secrets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layer, "secrets", "auth.json"), []byte(`{"token":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadDeclaration(root, "builder"); err == nil || !errors.Is(err, errProfileAuth) {
		t.Fatalf("nested auth.json err=%v, want errProfileAuth", err)
	}
	if err := os.Remove(filepath.Join(layer, "secrets", "auth.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layer, "auth.json"), []byte(`{"token":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadDeclaration(root, "builder"); err == nil || !errors.Is(err, errProfileAuth) {
		t.Fatalf("auth.json err=%v, want errProfileAuth", err)
	}

	if err := os.Remove(filepath.Join(layer, "auth.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("skills.md", filepath.Join(layer, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := loadDeclaration(root, "builder"); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink err=%v, want symlink", err)
	}
	if err := os.RemoveAll(layer); err != nil {
		t.Fatal(err)
	}
	external := t.TempDir()
	if err := os.WriteFile(filepath.Join(external, "leak"), []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, layer); err != nil {
		t.Fatal(err)
	}
	if _, _, err := materializeRunProfile(context.Background(), root, "1-builder", Declaration{Name: "builder"}); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("materialized a symlinked repository root: %v", err)
	}
}

func TestMaterializeRunProfileOverlaysLayers(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(t.TempDir(), "base")
	for _, item := range []struct{ dir, name, body string }{
		{base, "auth.json", `{"token":"base"}`},
		{base, "skills/shared.md", "base skill\n"},
		{filepath.Join(root, "agents", "_shared", "profile"), "skills/shared.md", "shared skill\n"},
		{filepath.Join(root, "agents", "builder", "profile"), "skills/builder.md", "builder skill\n"},
		{base, "file-to-dir", "base file\n"},
		{base, "dir-to-file/old", "base child\n"},
		{filepath.Join(root, "agents", "builder", "profile"), "file-to-dir/new", "new child\n"},
		{filepath.Join(root, "agents", "builder", "profile"), "dir-to-file", "new file\n"},
	} {
		if err := os.MkdirAll(filepath.Join(item.dir, filepath.Dir(item.name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(item.dir, item.name), []byte(item.body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	declaration := Declaration{Name: "builder", BaseProfile: base, BaseProfileRequired: true}
	target, files, err := materializeRunProfile(context.Background(), root, "1-builder", declaration)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(files, []string{"auth.json", "dir-to-file", "file-to-dir/new", "skills/builder.md", "skills/shared.md"}) {
		t.Fatalf("manifest=%v", files)
	}
	shared, err := os.ReadFile(filepath.Join(target, "skills/shared.md"))
	if err != nil || string(shared) != "shared skill\n" {
		t.Fatalf("shared overlay=%q err=%v", shared, err)
	}
	auth, err := os.ReadFile(filepath.Join(target, "auth.json"))
	if err != nil || string(auth) != `{"token":"base"}` {
		t.Fatalf("base auth=%q err=%v", auth, err)
	}
	for name, want := range map[string]string{"dir-to-file": "new file\n", "file-to-dir/new": "new child\n"} {
		got, err := os.ReadFile(filepath.Join(target, name))
		if err != nil || string(got) != want {
			t.Fatalf("collision %s=%q err=%v", name, got, err)
		}
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, runProfileDir(root, "2-builder")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := materializeRunProfile(context.Background(), root, "2-builder", declaration); err == nil || !strings.Contains(err.Error(), "create Run profile") {
		t.Fatalf("reused a pre-existing Run profile target: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "auth.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("profile escaped through target symlink: %v", err)
	}
}

func TestMaterializeRejectsAFileValuedBaseProfile(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(base, []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := materializeRunProfile(context.Background(), root, "1-builder", Declaration{Name: "builder", BaseProfile: base, BaseProfileRequired: true})
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("file base err=%v, want not a directory", err)
	}
}

func TestMaterializeRejectsAMissingExplicitOperatorProfile(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(t.TempDir(), "gone")
	_, _, err := materializeRunProfile(context.Background(), root, "1-builder", Declaration{Name: "builder", BaseProfile: missing, BaseProfileRequired: true})
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing explicit profile err=%v, want not exist", err)
	}
}

func TestMaterializeRejectsAnOversizedProfileFile(t *testing.T) {
	root, base := t.TempDir(), t.TempDir()
	file, err := os.Create(filepath.Join(base, "oversized"))
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maxProfileFileBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	_, _, err = materializeRunProfile(context.Background(), root, "1-builder", Declaration{Name: "builder", BaseProfile: base, BaseProfileRequired: true})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized profile err=%v", err)
	}
}

func TestMaterializeFollowsASymlinkedOperatorProfile(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(t.TempDir(), "real")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "auth.json"), []byte(`{"ok":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	externalFile := filepath.Join(t.TempDir(), "tool")
	if err := os.WriteFile(externalFile, []byte("tool body"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(externalFile, filepath.Join(real, "tool")); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	target, files, err := materializeRunProfile(context.Background(), root, "1-builder", Declaration{Name: "builder", BaseProfile: link, BaseProfileRequired: true})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(files, []string{"auth.json", "tool"}) {
		t.Fatalf("symlink profile files=%v", files)
	}
	for name, want := range map[string]string{"auth.json": `{"ok":true}`, "tool": "tool body"} {
		got, err := os.ReadFile(filepath.Join(target, name))
		if err != nil || string(got) != want {
			t.Fatalf("symlink profile %s=%q err=%v", name, got, err)
		}
	}
	info, err := os.Lstat(filepath.Join(target, "tool"))
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("materialized tool mode=%v err=%v", info, err)
	}
	if err := os.WriteFile(externalFile, []byte("changed"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(target, "tool")); err != nil || string(got) != "tool body" {
		t.Fatalf("private tool changed with host target: %q err=%v", got, err)
	}
}

func TestOperatorProfileRejectsNestedTarget(t *testing.T) {
	root := t.TempDir()
	_, _, err := materializeRunProfile(context.Background(), root, "2-builder", Declaration{Name: "builder", BaseProfile: forestPath(root, "profiles"), BaseProfileRequired: true})
	if err == nil || !strings.Contains(err.Error(), "contains the Run profile") {
		t.Fatalf("nested target err=%v", err)
	}
}

func TestRunEnvironmentSetsProfileAndDeclaredKeys(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", "/home/operator")
	t.Setenv("FOREST_RUN_ID", "stale-run")
	t.Setenv("PI_CODING_AGENT_DIR", "/stale-profile")
	declaration := Declaration{Name: "builder", Env: map[string]string{"NOTE": "hello", "REFERENCE": "provider:opaque"}}
	environment, err := runEnvironment(root, "Iron Forest Builder", "builder@forest.invalid", "1-builder", "/tmp/profile", declaration)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, entry := range environment {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			t.Fatalf("malformed env %q", entry)
		}
		got[key] = append(got[key], value)
	}
	for key, want := range map[string]string{
		"FOREST_RUN_ID":       "1-builder",
		"PI_CODING_AGENT_DIR": "/tmp/profile",
		"GIT_AUTHOR_NAME":     "Iron Forest Builder",
		"HOME":                "/home/operator",
		"NOTE":                "hello",
		"REFERENCE":           "provider:opaque",
	} {
		if !reflect.DeepEqual(got[key], []string{want}) {
			t.Fatalf("env %s=%q, want exactly %q", key, got[key], want)
		}
	}
}

func TestRunEvidenceLineOmitsEnvValues(t *testing.T) {
	line := runEvidenceLine(
		RunRecord{RunID: "1-builder"},
		Declaration{Name: "builder", Model: "local", ModelSource: "declaration", Env: map[string]string{"SECRET": "never"}},
		[]string{"skills/builder.md"},
	)
	if strings.Contains(line, "never") {
		t.Fatalf("evidence leaked an env value: %s", line)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(line), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["type"] != "forest.run" || payload["model_source"] != "declaration" {
		t.Fatalf("evidence=%v", payload)
	}
	env, _ := payload["env"].([]any)
	if len(env) != 1 || env[0] != "SECRET" {
		t.Fatalf("evidence env keys=%v, want [SECRET]", env)
	}
}

func TestRunnerCollectsProfileAndWritesEvidence(t *testing.T) {
	root, _ := testClone(t)
	layer := filepath.Join(root, "agents", "builder", "profile", "skills")
	if err := os.MkdirAll(layer, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layer, "seen.md"), []byte("builder skill\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	seen := filepath.Join(state, "seen")
	script := `#!/bin/sh
set -eu
printf '%s\n' "$PI_CODING_AGENT_DIR" > "$SEEN"
cat "$PI_CODING_AGENT_DIR/skills/seen.md" >> "$SEEN"
printf '%s\n' '{"type":"turn_end","message":{"usage":{"input":1,"output":1}}}'
`
	omp := filepath.Join(state, "omp")
	if err := os.WriteFile(omp, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SEEN", seen)
	runner := NewRunner(root)
	runner.PiPath = omp
	declaration := Declaration{Name: "builder", Model: "local", ModelSource: "declaration", TaskPrompt: "x"}
	record, err := runner.Run(context.Background(), declaration, 10)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(seen)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(got, []byte("builder skill\n")) {
		t.Fatalf("child did not see profile: %q", got)
	}
	if _, err := os.Stat(runProfileDir(root, record.RunID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Run profile survived collection: %v", err)
	}
	log, err := os.ReadFile(forestPath(root, "runs", record.RunID+".log"))
	if err != nil {
		t.Fatal(err)
	}
	first, _, _ := strings.Cut(string(log), "\n")
	if !strings.Contains(first, `"type":"forest.run"`) || !strings.Contains(first, "skills/seen.md") {
		t.Fatalf("log missing evidence: %q", first)
	}
}

func TestRunnerRefreshesOAuthBeforeCopyingThePrivateProfile(t *testing.T) {
	root, _ := testClone(t)
	base, state := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "auth.json"), []byte(`{"openai-codex":{"type":"oauth"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(state, "refresh")
	script := `#!/bin/sh
set -eu
if [ "${1-}" = auth ]; then
	test "$2" = print-bearer-token
	test -z "${FOREST_RUN_ID+x}${GIT_AUTHOR_NAME+x}"
	test "$6" = 70s
	printf '%s' '{"openai-codex":{"type":"oauth","access":"refreshed"}}' > "$PI_CODING_AGENT_DIR/auth.json"
	printf refreshed > "$MARKER"
	printf token
	exit 0
fi
test "$(cat "$PI_CODING_AGENT_DIR/auth.json")" = '{"openai-codex":{"type":"oauth","access":"refreshed"}}'
printf '%s\n' '{"type":"turn_end","message":{"usage":{"input":1}}}'
`
	pi := filepath.Join(state, "pi")
	if err := os.WriteFile(pi, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MARKER", marker)
	t.Setenv("FOREST_RUN_ID", "stale")
	t.Setenv("GIT_AUTHOR_NAME", "stale")
	runner := NewRunner(root)
	runner.PiPath = pi
	declaration := Declaration{
		Name:                "builder",
		Model:               "openai-codex/gpt-5.6-sol",
		TaskPrompt:          "x",
		BaseProfile:         base,
		BaseProfileRequired: true,
	}
	if record, err := runner.Run(context.Background(), declaration, 10); err != nil || record.Exit != 0 {
		t.Fatalf("Run record=%#v err=%v", record, err)
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "refreshed" {
		t.Fatalf("OAuth refresh marker=%q err=%v", got, err)
	}
}

func TestShippedProfilesComposeSharedAndRoleResources(t *testing.T) {
	source, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for _, name := range []string{"_shared", "builder", "verifier", "fixer"} {
		src := filepath.Join(source, "agents", name, "profile")
		if _, err := os.Stat(src); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(root, "agents", name, "profile")
		if err := os.CopyFS(dst, os.DirFS(src)); err != nil {
			t.Fatal(err)
		}
	}

	profiles := make(map[string]string)
	for index, name := range []string{"builder", "verifier", "fixer"} {
		profile, _, err := materializeRunProfile(context.Background(), root, fmt.Sprintf("%d-%s", index+1, name), Declaration{Name: name})
		if err != nil {
			t.Fatal(err)
		}
		profiles[name] = profile
		for _, skill := range []string{"systematic-debugging", "verify-claim"} {
			if _, err := os.Stat(filepath.Join(profile, "skills", skill, "SKILL.md")); err != nil {
				t.Fatalf("%s profile missing shared skill %s: %v", name, skill, err)
			}
		}
		agents, err := os.ReadFile(filepath.Join(profile, "AGENTS.md"))
		if err != nil || !bytes.Contains(agents, []byte("# "+strings.ToUpper(name[:1])+name[1:]+" engineering")) {
			t.Fatalf("%s AGENTS.md=%q err=%v", name, agents, err)
		}
	}

	for _, skill := range []string{"thermo-nuclear-review", "thermo-nuclear-code-quality-review"} {
		if _, err := os.Stat(filepath.Join(profiles["verifier"], "skills", skill, "SKILL.md")); err != nil {
			t.Fatalf("Verifier profile missing %s: %v", skill, err)
		}
		for _, name := range []string{"builder", "fixer"} {
			if _, err := os.Stat(filepath.Join(profiles[name], "skills", skill, "SKILL.md")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%s profile received Verifier skill %s: %v", name, skill, err)
			}
		}
	}
}
