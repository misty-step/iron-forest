package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	cfg, err := loadConfig(filepath.Join(repoDir, "forest.yaml"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "forest:", err)
		return 1
	}
	switch args[0] {
	case "list":
		return cmdList(cfg)
	case "agents":
		return cmdAgents(repoDir)
	case "stats":
		return cmdStats(repoDir, args[1:])
	case "serve":
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
		return cmdShow(repoDir, args[1])
	case "version":
		fmt.Printf("forest %s\n", version)
		return 0
	case "selfcheck":
		return cmdSelfcheck(repoDir)
	case "watch":
		return cmdWatch(cfg, repoDir, args[1:])
	default:
		usage()
		return 2
	}
}

func cmdList(cfg Config) int {
	repoDir, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "forest:", err)
		return 1
	}
	// Ask the lane, not the tracker: an operator needs to know what the Builder
	// will take, which is narrower than what the tracker calls eligible.
	subjects, err := builderFlow{}.Select(cfg, repoDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "forest:", err)
		return 1
	}
	for _, s := range subjects {
		fmt.Printf("#%d\t%s\n", s.Issue, s.Item.Title)
	}
	return 0
}

func cmdAgents(repoDir string) int {
	names, err := discoverAgents(repoDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "forest:", err)
		return 1
	}
	if len(names) == 0 {
		fmt.Println("forest: no agents declared under agents/")
		return 0
	}
	for _, name := range names {
		a, err := loadAgent(repoDir, name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "forest: %v\n", err)
			continue
		}
		budget := "none"
		if a.BudgetSec > 0 {
			budget = fmt.Sprintf("%ds", a.BudgetSec)
		}
		fmt.Printf("%s\tmodel=%s%s mode=%s steps=%d budget=%s def_sha=%s\n",
			name, a.Model, variantSuffix(a), a.Mode, a.Steps, budget, a.DefSHA)
		fmt.Printf("  %s\n", a.Description)
		var mcps []string
		for _, m := range a.MCP {
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

func cmdShow(repoDir, sha string) int {
	if err := fetchNotes(repoDir); err != nil {
		fmt.Fprintln(os.Stderr, "forest: notes:", err)
		return 1
	}
	verdict, haveVerdict, err := readVerdict(repoDir, sha)
	if err != nil {
		fmt.Fprintln(os.Stderr, "forest: verdict:", err)
		return 1
	}
	checks, haveChecks, err := readChecks(repoDir, sha)
	if err != nil {
		fmt.Fprintln(os.Stderr, "forest: checks:", err)
		return 1
	}
	value := struct {
		Verdict *verdictNote `json:"verdict,omitempty"`
		Checks  *checksNote  `json:"checks,omitempty"`
	}{
		Checks: &checks,
	}
	if haveVerdict {
		value.Verdict = &verdict
	}
	if !haveChecks {
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
