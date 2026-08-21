package main

import (
	"context"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestScopeValidate(t *testing.T) {
	tests := []struct {
		name    string
		scope   Scope
		wantErr string
	}{
		{name: "empty"},
		{name: "label", scope: Scope{Label: "forest:ready:alpha"}},
		{name: "subjects", scope: Scope{Subjects: []string{"4", "if-241"}}},
		{name: "branch prefix", scope: Scope{BranchPrefix: "forest/if-"}},
		{name: "two modes", scope: Scope{Label: "forest:ready", Subjects: []string{"4"}}, wantErr: "scope must set exactly one"},
		{name: "three modes", scope: Scope{Label: "forest:ready", Subjects: []string{"4"}, BranchPrefix: "forest/4"}, wantErr: "scope must set exactly one"},
		{name: "bad subject", scope: Scope{Subjects: []string{"bad id"}}, wantErr: "scope.subjects entry"},
		{name: "bad prefix", scope: Scope{BranchPrefix: "main"}, wantErr: "scope.branch_prefix"},
		{name: "bad label", scope: Scope{Label: "bad\nlabel"}, wantErr: "scope.label"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.scope.Validate()
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Validate() error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestParseScopeOverride(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    Scope
		wantErr string
	}{
		{name: "label", raw: "label=forest:ready:alpha", want: Scope{Label: "forest:ready:alpha"}},
		{name: "subjects", raw: "subjects=4,if-241", want: Scope{Subjects: []string{"4", "if-241"}}},
		{name: "branch prefix", raw: "branch_prefix=forest/if-", want: Scope{BranchPrefix: "forest/if-"}},
		{name: "empty", raw: "", wantErr: "scope override is empty"},
		{name: "missing separator", raw: "forest:ready", wantErr: "scope override"},
		{name: "empty value", raw: "label=", wantErr: "scope override"},
		{name: "unknown mode", raw: "tag=forest:ready", wantErr: "unknown mode"},
		{name: "bad subjects", raw: "subjects=4,bad@id", wantErr: "scope.subjects entry"},
		{name: "bad prefix", raw: "branch_prefix=main", wantErr: "scope.branch_prefix"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseScopeOverride(test.raw)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("parseScopeOverride(%q) error = %v, want substring %q", test.raw, err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseScopeOverride(%q) error = %v", test.raw, err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("parseScopeOverride(%q) = %#v, want %#v", test.raw, got, test.want)
			}
		})
	}
}

func configWithScope(scope string) string {
	return "repo: owner/name\n" + scope + "agents:\n  builder: {poll: \"forest poll builder\", interval: 5}\nchecks:\n  - {name: test, run: \"true\"}\n"
}

func TestDecodeConfigScope(t *testing.T) {
	cfg, err := decodeConfig([]byte(configWithScope("scope:\n  subjects: [\"4\", \"if-241\"]\n")), "forest.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Scope == nil || !reflect.DeepEqual(cfg.Scope.Subjects, []string{"4", "if-241"}) {
		t.Fatalf("scope=%#v, want subjects [4 if-241]", cfg.Scope)
	}

	cfg, err = decodeConfig([]byte(configWithScope("")), "forest.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Scope != nil {
		t.Fatalf("absent scope decoded as %#v, want nil", cfg.Scope)
	}

	tests := []struct {
		name string
		data string
		want string
	}{
		{name: "two modes", data: configWithScope("scope:\n  label: forest:ready\n  subjects: [4]\n"), want: "scope must set exactly one"},
		{name: "bad subject", data: configWithScope("scope:\n  subjects: [\"bad id\"]\n"), want: "scope.subjects entry"},
		{name: "bad prefix", data: configWithScope("scope:\n  branch_prefix: main\n"), want: "scope.branch_prefix"},
		{name: "bad label", data: configWithScope("scope:\n  label: \"bad\\nlabel\"\n"), want: "scope.label"},
		{name: "unknown scope field", data: configWithScope("scope:\n  labels: [forest:ready]\n"), want: "field labels not found in scope"},
		{name: "scope not mapping", data: configWithScope("scope: forest:ready\n"), want: "scope must be a YAML mapping"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := decodeConfig([]byte(test.data), "forest.yaml")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("decodeConfig() error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestBuilderPollScopeBySubjects(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name     string
		subjects []string
		issues   string
		takeable string
		mine     string
		agent    string
		want     int
	}{
		{name: "matching issue", subjects: []string{"4"}, issues: `[[{"number":4}]]`, want: 0},
		{name: "issue outside scope", subjects: []string{"5"}, issues: `[[{"number":4}]]`, want: 1},
		{name: "matching powder", subjects: []string{"if-241"}, issues: `[]`, takeable: `[{"id":"if-241"}]`, mine: `[]`, agent: "forest-owner-name", want: 0},
		{name: "powder outside scope", subjects: []string{"if-242"}, issues: `[]`, takeable: `[{"id":"if-241"}]`, mine: `[]`, agent: "forest-owner-name", want: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.agent == "" {
				t.Setenv("POWDER_AGENT", "")
				t.Setenv("POWDER_URL", "")
				t.Setenv("POWDER_API_BASE_URL", "")
			} else {
				t.Setenv("POWDER_AGENT", test.agent)
				t.Setenv("POWDER_URL", "https://powder.example")
			}
			p := &Poller{Root: t.TempDir(), Repo: "owner/name", Scope: Scope{Subjects: test.subjects}}
			p.Run = func(_ context.Context, name string, args ...string) ([]byte, error) {
				switch name {
				case "gh":
					return []byte(test.issues), nil
				case "powder":
					if slices.Contains(args, "--takeable") {
						return []byte(test.takeable), nil
					}
					if slices.Contains(args, "--mine") {
						return []byte(test.mine), nil
					}
					t.Fatalf("unexpected powder args: %v", args)
					return nil, nil
				case "git":
					return nil, nil
				default:
					t.Fatalf("unexpected tool %s", name)
					return nil, nil
				}
			}
			if got, _ := p.builder(ctx); got != test.want {
				t.Fatalf("poll exit=%d, want %d", got, test.want)
			}
		})
	}
}

func TestBuilderPollScopeByBranchPrefix(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name     string
		prefix   string
		issues   string
		takeable string
		mine     string
		agent    string
		want     int
	}{
		{name: "matching issue", prefix: "forest/4", issues: `[[{"number":4}]]`, want: 0},
		{name: "issue outside scope", prefix: "forest/5", issues: `[[{"number":4}]]`, want: 1},
		{name: "matching powder", prefix: "forest/if-", issues: `[]`, takeable: `[{"id":"if-241"}]`, mine: `[]`, agent: "forest-owner-name", want: 0},
		{name: "powder outside scope", prefix: "forest/ifx-", issues: `[]`, takeable: `[{"id":"if-241"}]`, mine: `[]`, agent: "forest-owner-name", want: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.agent == "" {
				t.Setenv("POWDER_AGENT", "")
				t.Setenv("POWDER_URL", "")
				t.Setenv("POWDER_API_BASE_URL", "")
			} else {
				t.Setenv("POWDER_AGENT", test.agent)
				t.Setenv("POWDER_URL", "https://powder.example")
			}
			p := &Poller{Root: t.TempDir(), Repo: "owner/name", Scope: Scope{BranchPrefix: test.prefix}}
			p.Run = func(_ context.Context, name string, args ...string) ([]byte, error) {
				switch name {
				case "gh":
					return []byte(test.issues), nil
				case "powder":
					if slices.Contains(args, "--takeable") {
						return []byte(test.takeable), nil
					}
					if slices.Contains(args, "--mine") {
						return []byte(test.mine), nil
					}
					t.Fatalf("unexpected powder args: %v", args)
					return nil, nil
				case "git":
					return nil, nil
				default:
					t.Fatalf("unexpected tool %s", name)
					return nil, nil
				}
			}
			if got, _ := p.builder(ctx); got != test.want {
				t.Fatalf("poll exit=%d, want %d", got, test.want)
			}
		})
	}
}

func TestBuilderPollScopeByLabel(t *testing.T) {
	t.Setenv("POWDER_AGENT", "")
	t.Setenv("POWDER_URL", "")
	t.Setenv("POWDER_API_BASE_URL", "")
	p := &Poller{Root: t.TempDir(), Repo: "owner/name", Scope: Scope{Label: "forest:ready:alpha"}}
	p.Run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		switch name {
		case "gh":
			if args[len(args)-1] != "repos/owner/name/issues?state=open&labels=forest%3Aready%3Aalpha&per_page=100" {
				t.Fatalf("label endpoint wrong: %v", args)
			}
			return []byte(`[[{"number":4}]]`), nil
		case "powder":
			t.Fatalf("label scope must not query powder: %v", args)
			return nil, nil
		case "git":
			return nil, nil
		default:
			t.Fatalf("unexpected tool %s", name)
			return nil, nil
		}
	}
	if got, _ := p.builder(context.Background()); got != 0 {
		t.Fatalf("poll exit=%d, want 0", got)
	}
}

func TestPollCLIRejectsMalformedScopeOverride(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	code, _, stderr := captureCLIOutput(t, func() int { return runCLI([]string{"poll", "builder", "--scope", "bad"}) })
	if code != exitInvalidArg {
		t.Fatalf("code=%d, want %d (stderr=%q)", code, exitInvalidArg, stderr)
	}
	if !strings.Contains(stderr, "scope override") {
		t.Fatalf("stderr=%q, want scope override", stderr)
	}
}

func TestSelfcheckRejectsMalformedScope(t *testing.T) {
	tests := []struct {
		name  string
		scope string
		want  string
	}{
		{name: "two modes", scope: "scope:\n  label: forest:ready\n  subjects: [4]\n", want: "scope must set exactly one"},
		{name: "bad subject", scope: "scope:\n  subjects: [\"bad id\"]\n", want: "scope.subjects entry"},
		{name: "bad prefix", scope: "scope:\n  branch_prefix: main\n", want: "scope.branch_prefix"},
		{name: "bad label", scope: "scope:\n  label: \"bad\\nlabel\"\n", want: "scope.label"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeCLIConfig(t, root, "exit 1")
			config := "repo: owner/name\nprimary: refs/heads/master\n" + test.scope + "agents:\n  builder:\n    poll: exit 1\n    interval: 1\nchecks:\n  - name: test\n    run: \"true\"\n"
			if err := os.WriteFile(configPath(root), []byte(config), 0o644); err != nil {
				t.Fatal(err)
			}
			code, _, stderr := captureCLIOutput(t, func() int { return runCLI([]string{"selfcheck", "--root", root}) })
			if code != exitError {
				t.Fatalf("selfcheck code=%d, want %d (stderr=%q)", code, exitError, stderr)
			}
			if !strings.Contains(stderr, test.want) {
				t.Fatalf("selfcheck stderr=%q, want substring %q", stderr, test.want)
			}
		})
	}
}
