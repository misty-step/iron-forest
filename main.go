package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/misty-step/iron-forest/core"
)

func main() {
	recordPendingUpdate()
	os.Exit(run(os.Args[1:]))
}

func usage() {
	fmt.Fprintln(os.Stderr, `forest

  forest list                         print eligible tracker items
  forest agents                       list declared agents and digests
  forest stats [--json]              aggregate the run ledger
  forest serve [--flow <name>]... [--factory-dir <path>]
                                      run enabled flows
  forest run <flow> <subject>        run one selected subject
  forest show <sha>                  print verdict and checks notes
  forest version                     print the binary revision
  forest selfcheck                   verify config and agents offline
  forest watch [--interval 2s]       show the operator board`)
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}
	repoDir, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "forest:", err)
		return 1
	}
	api := NewCore(repoDir)
	switch args[0] {
	case "list":
		return cmdList(api)
	case "agents":
		return cmdAgents(api)
	case "stats":
		return cmdStats(api, args[1:])
	case "serve":
		cfg, code := repoConfig(repoDir)
		if code != 0 {
			return code
		}
		var names []string
		for i := 1; i < len(args); i++ {
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "forest: serve: expected a value after %q\n", args[i])
				return 2
			}
			switch args[i] {
			case "--flow":
				names = append(names, args[i+1])
			case "--factory-dir":
				factoryDir = args[i+1]
			default:
				fmt.Fprintf(os.Stderr, "forest: serve: expected --flow or --factory-dir, got %q\n", args[i])
				return 2
			}
			i++
		}
		return serve(cfg, repoDir, names)
	case "run":
		cfg, code := repoConfig(repoDir)
		if code != 0 {
			return code
		}
		if len(args) != 3 {
			usage()
			return 2
		}
		return runOnce(cfg, repoDir, args[1], args[2])
	case "show":
		if len(args) != 2 {
			usage()
			return 2
		}
		return cmdShow(api, args[1])
	case "version":
		fmt.Printf("forest %s\n", version)
		return 0
	case "selfcheck":
		return cmdSelfcheck(repoDir)
	case "watch":
		return cmdWatch(api, repoDir, args[1:])
	default:
		usage()
		return 2
	}
}

// repoConfig loads forest.yaml for the live-run commands that need it.
func repoConfig(repoDir string) (Config, int) {
	cfg, err := loadConfig(filepath.Join(repoDir, "forest.yaml"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "forest:", err)
		return Config{}, 1
	}
	return cfg, 0
}

func cmdList(api core.API) int {
	// Ask the lane, not the tracker: an operator needs to know what the Builder
	// will take, which is narrower than what the tracker calls eligible. The
	// core API returns exactly that backlog.
	items, err := api.Items()
	if err != nil {
		fmt.Fprintln(os.Stderr, "forest:", err)
		return 1
	}
	for _, it := range items {
		fmt.Printf("#%s\t%s\n", it.ID, it.Title)
	}
	return 0
}

func cmdAgents(api core.API) int {
	agents, err := api.Agents()
	if err != nil {
		fmt.Fprintln(os.Stderr, "forest:", err)
		return 1
	}
	if len(agents) == 0 {
		fmt.Println("forest: no agents declared under agents/")
		return 0
	}
	for _, a := range agents {
		if a.Err != "" {
			fmt.Fprintln(os.Stderr, "forest:", a.Err)
			continue
		}
		fmt.Printf("%s\tmodel=%s%s mode=%s def_sha=%s\n",
			a.Name, a.Model, variantSuffix(a.Variant), a.Mode, a.DefSHA)
		fmt.Printf("  %s\n", a.Description)
		var mcps []string
		for _, m := range a.Mcps {
			state := "off"
			if m.Enabled {
				state = "on"
			}
			mcps = append(mcps, fmt.Sprintf("%s(%s)", m.Name, state))
		}
		if len(mcps) > 0 {
			fmt.Printf("  mcp: %s\n", strings.Join(mcps, ", "))
		}
	}
	return 0
}

func cmdShow(api core.API, sha string) int {
	v, c, err := api.Notes(sha)
	if err != nil {
		// The core API tags which note subsystem failed so the operator keeps
		// the per-subsystem prefix the command always printed.
		prefix := "forest:"
		var se *core.StageError
		if errors.As(err, &se) {
			switch se.Stage {
			case core.StageFetch:
				prefix = "forest: notes:"
			case core.StageVerdict:
				prefix = "forest: verdict:"
			case core.StageChecks:
				prefix = "forest: checks:"
			}
		}
		fmt.Fprintln(os.Stderr, prefix, err)
		return 1
	}
	verdict := verdictNote{
		Verdict: v.Verdict, Notes: v.Notes, Reviewer: v.Reviewer,
		Model: v.Model, DefSHA: v.DefSHA, RunID: v.RunID, Time: v.Time,
	}
	// Always build a non-nil results slice so a raw checks note with no rows
	// (an absent, null, or explicitly empty results field) renders the same
	// empty array the show command always printed, never `null`.
	results := make([]checkResult, 0, len(c.Results))
	for _, r := range c.Results {
		results = append(results, checkResult{
			Name: r.Name, Code: r.Code, Seconds: r.Seconds, Output: r.Output,
		})
	}
	checks := checksNote{
		Status:  c.Status,
		Results: results,
		RunID:   c.RunID,
		Time:    c.Time,
	}
	value := struct {
		Verdict *verdictNote `json:"verdict,omitempty"`
		Checks  *checksNote  `json:"checks,omitempty"`
	}{
		Checks: &checks,
	}
	if v.Present {
		value.Verdict = &verdict
	}
	if !c.Present {
		value.Checks = nil
	}
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "forest: show:", err)
		return 1
	}
	fmt.Println(string(b))
	return 0
}
