package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func usage() {
	fmt.Fprintf(os.Stderr, `forest — the iron-forest piranha tank

  forest list              print the current backlog
  forest once <issue>      chew a single issue end to end
  forest chew              poll the backlog forever, one item at a time
`)
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
	case "once":
		if len(args) < 2 {
			usage()
			return 2
		}
		n, err := strconv.Atoi(args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, "forest: issue number required")
			return 2
		}
		it, err := getIssue(cfg.Repo, n)
		if err != nil {
			fmt.Fprintln(os.Stderr, "forest:", err)
			return 1
		}
		return chewOne(cfg, repoDir, it)
	case "chew":
		return chewLoop(cfg, repoDir)
	default:
		usage()
		return 2
	}
}

func cmdList(cfg Config) int {
	items, err := backlog(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "forest:", err)
		return 1
	}
	for _, it := range items {
		fmt.Printf("#%d\t%s\n", it.Number, it.Title)
	}
	return 0
}

func chewLoop(cfg Config, repoDir string) int {
	fmt.Printf("forest: chewing %s every %ds\n", cfg.Repo, cfg.PollIntervalSec)
	for {
		items, err := backlog(cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "forest: backlog: %v\n", err)
		} else {
			for _, it := range items {
				fmt.Printf("forest: chewing #%d %s\n", it.Number, it.Title)
				if code := chewOne(cfg, repoDir, it); code != 0 {
					fmt.Fprintf(os.Stderr, "forest: #%d failed\n", it.Number)
				}
				time.Sleep(2 * time.Second) // breathe between items
			}
			if len(items) == 0 {
				fmt.Printf("forest: backlog empty, sleeping %ds\n", cfg.PollIntervalSec)
			}
		}
		time.Sleep(time.Duration(cfg.PollIntervalSec) * time.Second)
	}
}

// chewOne runs the whole pipeline for one issue: claim, worktree, agent,
// gate, publish, record. Each step that mutates external state is
// deterministic code.
func chewOne(cfg Config, repoDir string, it issue) int {
	runID := fmt.Sprintf("%s-%d", time.Now().UTC().Format("20060102T150405Z"), it.Number)
	workspace := filepath.Join(repoDir, ".forest")
	_ = ensureLabels(cfg.Repo)

	record := runRecord{Time: time.Now().UTC().Format(time.RFC3339), RunID: runID, Issue: it.Number}

	if err := claimIssue(cfg.Repo, it.Number); err != nil {
		record.Status = "claim_failed"
		record.Error = err.Error()
		_ = appendRun(workspace, record)
		fmt.Fprintf(os.Stderr, "forest: claim #%d: %v\n", it.Number, err)
		return 1
	}

	wtDir, branch, baseSHA, err := createWorktree(repoDir, workspace, it)
	if err != nil {
		record.Status = "worktree_failed"
		record.Error = err.Error()
		_ = appendRun(workspace, record)
		_ = failIssue(cfg.Repo, it.Number, "worktree: "+err.Error())
		fmt.Fprintf(os.Stderr, "forest: worktree #%d: %v\n", it.Number, err)
		return 1
	}
	defer removeWorktree(repoDir, wtDir)
	record.Branch = branch

	prompt, err := buildPrompt(filepath.Join(repoDir, cfg.Agent.SystemPrompt), it)
	if err != nil {
		record.Status = "prompt_failed"
		record.Error = err.Error()
		_ = appendRun(workspace, record)
		_ = failIssue(cfg.Repo, it.Number, "prompt: "+err.Error())
		fmt.Fprintf(os.Stderr, "forest: prompt #%d: %v\n", it.Number, err)
		return 1
	}

	tracePath := filepath.Join(workspace, "runs", runID+".jsonl")
	stats, runErr := runAgent(wtDir, prompt, cfg.Agent.Model, tracePath, 20*time.Minute)
	record.TokensIn = stats.tokensIn
	record.TokOut = stats.tokensOut
	record.CostUSD = price(stats, cfg)
	if runErr != nil {
		record.Status = "agent_failed"
		record.Error = runErr.Error()
		_ = appendRun(workspace, record)
		_ = failIssue(cfg.Repo, it.Number, "agent: "+runErr.Error())
		fmt.Fprintf(os.Stderr, "forest: agent #%d: %v\n", it.Number, runErr)
		return 1
	}

	changed, rep, gerr := gate(wtDir, baseSHA, cfg.Agent.Protected)
	if gerr != nil {
		record.Status = "gate_failed"
		record.Error = gerr.Error()
		_ = appendRun(workspace, record)
		_ = failIssue(cfg.Repo, it.Number, "gate: "+gerr.Error())
		fmt.Fprintf(os.Stderr, "forest: gate #%d: %v\n", it.Number, gerr)
		return 1
	}

	if err := commitAndPush(repoDir, wtDir, branch, it); err != nil {
		record.Status = "publish_failed"
		record.Error = err.Error()
		_ = appendRun(workspace, record)
		_ = failIssue(cfg.Repo, it.Number, "publish: "+err.Error())
		fmt.Fprintf(os.Stderr, "forest: publish #%d: %v\n", it.Number, err)
		return 1
	}

	// the pull request body lists exactly what is in the pushed commit, so
	// report.json (kept out of the tree) never appears as a changed file.
	committed, err := gitStrings(wtDir, "diff", "--name-only", baseSHA, "HEAD")
	if err != nil {
		committed = changed
	}

	prURL, err := openPR(cfg.Repo, branch, "forest: "+it.Title, prBody(it, rep, committed))
	if err != nil {
		record.Status = "pr_failed"
		record.Error = err.Error()
		_ = appendRun(workspace, record)
		_ = failIssue(cfg.Repo, it.Number, "pr: "+err.Error())
		fmt.Fprintf(os.Stderr, "forest: pr #%d: %v\n", it.Number, err)
		return 1
	}
	record.PRURL = prURL
	record.Status = "done"
	_ = appendRun(workspace, record)
	if err := closeIssue(cfg.Repo, it.Number); err != nil {
		fmt.Fprintf(os.Stderr, "forest: close #%d: %v (PR is open)\n", it.Number, err)
	}
	fmt.Printf("forest: #%d done: %s\n", it.Number, prURL)
	return 0
}

// buildPrompt composes the agent's system prompt with the issue text.
func buildPrompt(promptPath string, it issue) (string, error) {
	b, err := os.ReadFile(promptPath)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	sb.Write(b)
	sb.WriteString("\n\n## The issue to implement\n\n")
	sb.WriteString(fmt.Sprintf("Issue #%d: %s\n\n%s\n", it.Number, it.Title, it.Body))
	return sb.String(), nil
}

// prBody builds the pull request description from the agent's report.
func prBody(it issue, rep report, changed []string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Autogenerated by iron-forest `forest` from issue #%d — %s.\n\n", it.Number, it.Title)
	sb.WriteString(rep.Summary)
	sb.WriteString("\n\nChanged files:\n")
	for _, f := range changed {
		fmt.Fprintf(&sb, "- %s\n", f)
	}
	if strings.TrimSpace(rep.Notes) != "" && !strings.EqualFold(strings.TrimSpace(rep.Notes), "none") {
		fmt.Fprintf(&sb, "\nNotes: %s\n", rep.Notes)
	}
	fmt.Fprintf(&sb, "\nFixes #%d\n", it.Number)
	return sb.String()
}
