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
  forest agents            list declared agents and their composition digest
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
	case "agents":
		return cmdAgents(repoDir)
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
	for _, n := range names {
		a, err := loadAgent(repoDir, n)
		if err != nil {
			fmt.Fprintf(os.Stderr, "forest: %v\n", err)
			continue
		}
		fmt.Printf("%s\tmodel=%s mode=%s steps=%d budget=%ds price=%.2f/%.2f def_sha=%s\n",
			n, a.Model, a.Mode, a.Steps, a.BudgetSec, a.PriceInUSDPerM, a.PriceOutUSDPerM, a.DefSHA)
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

// chewOne runs the whole workflow for one issue: claim, worktree, build,
// gate, review, corrective passes, publish, record. Everything that mutates
// external state is deterministic code; the agents only produce work in the
// worktree.
func chewOne(cfg Config, repoDir string, it issue) int {
	runID := fmt.Sprintf("%s-%d", time.Now().UTC().Format("20060102T150405Z"), it.Number)
	workspace := filepath.Join(repoDir, WorkspaceDir)
	_ = ensureLabels(cfg.Repo)

	buildAgent, err := loadAgent(repoDir, cfg.Workflow.Build)
	if err != nil {
		fmt.Fprintf(os.Stderr, "forest: build agent: %v\n", err)
		return 1
	}
	buildSchema := filepath.Join(repoDir, DefaultAgentsDir, cfg.Workflow.Build, "report.schema.json")

	record := runRecord{
		Time: time.Now().UTC().Format(time.RFC3339),
		RunID: runID, Issue: it.Number,
		Agent: buildAgent.Name, Model: buildAgent.Model, DefSHA: buildAgent.DefSHA,
	}

	// The review agent is optional; "" in forest.yaml disables review.
	var reviewAgent *Agent
	var reviewSchema string
	if cfg.Workflow.Review != "" {
		reviewAgent, err = loadAgent(repoDir, cfg.Workflow.Review)
		if err != nil {
			fmt.Fprintf(os.Stderr, "forest: review agent: %v\n", err)
			return 1
		}
		record.Agent += "," + reviewAgent.Name
		reviewSchema = filepath.Join(repoDir, DefaultAgentsDir, cfg.Workflow.Review, "report.schema.json")
	}

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
	record.BaseSHA = baseSHA

	trace := func(phase string) string {
		return filepath.Join(workspace, "runs", runID+"."+phase+".jsonl")
	}
	addStats := func(st runStats, a *Agent) {
		record.TokensIn += st.tokensIn
		record.TokOut += st.tokensOut
		record.CostUSD += price(st, a)
	}
	fail := func(status, label, msg string) int {
		record.Status = status
		record.Error = msg
		_ = appendRun(workspace, record)
		_ = failIssue(cfg.Repo, it.Number, label+": "+msg)
		fmt.Fprintf(os.Stderr, "forest: %s #%d: %s\n", label, it.Number, msg)
		return 1
	}

	// Phase 1: build.
	prompt, err := renderUserPrompt(buildAgent, issueData(it, ""))
	if err != nil {
		fmt.Fprintf(os.Stderr, "forest: prompt #%d: %v\n", it.Number, err)
		return 1
	}
	stats, runErr := runPhase(wtDir, buildAgent, prompt, trace("chew"),
		time.Duration(buildAgent.BudgetSec)*time.Second)
	addStats(stats, buildAgent)
	if runErr != nil {
		return fail("agent_failed", "agent", runErr.Error())
	}
	changed, rep, gerr := gate(wtDir, baseSHA, cfg.Protected, buildSchema)
	if gerr != nil {
		return fail("gate_failed", "gate", gerr.Error())
	}

	// Phase 2: review, with up to max_fix_iterations corrective build passes.
	verdict := "approve"
	reviewNotes := ""
	if reviewAgent != nil {
		verdict = ""
	}
	for fix := 0; fix <= cfg.Workflow.MaxFixIterations; fix++ {
		if reviewAgent == nil {
			break
		}
		// stage the working tree so the diff, including new files, is visible
		// to the reviewer as exactly what would be committed.
		_ = git(wtDir, "add", "-A")
		_ = git(wtDir, "reset", "-q", "--", "report.json", "review.json")
		diff, derr := gitOut(wtDir, "diff", "--cached", "--", ".",
			":(exclude)report.json", ":(exclude)review.json")
		if derr != nil {
			return fail("review_failed", "review", derr.Error())
		}
		rvPrompt, rerr := renderUserPrompt(reviewAgent, reviewData(it, rep, diff))
		if rerr != nil {
			fmt.Fprintf(os.Stderr, "forest: review prompt #%d: %v\n", it.Number, rerr)
			return 1
		}
		rvStats, rerr := runPhase(wtDir, reviewAgent, rvPrompt, trace("review"),
			time.Duration(reviewAgent.BudgetSec)*time.Second)
		addStats(rvStats, reviewAgent)
		if rerr != nil {
			return fail("review_failed", "review", rerr.Error())
		}
		rv, gerr := gateReview(wtDir, reviewSchema)
		if gerr != nil {
			return fail("review_failed", "review", gerr.Error())
		}
		record.ReviewVerdict = rv.Verdict
		verdict = rv.Verdict
		reviewNotes = rv.Notes
		if verdict == "approve" {
			break
		}
		if fix >= cfg.Workflow.MaxFixIterations {
			break
		}
		// One corrective pass: re-run the build agent with the feedback.
		fixPrompt, ferr := renderUserPrompt(buildAgent, issueData(it, reviewNotes))
		if ferr != nil {
			fmt.Fprintf(os.Stderr, "forest: fix prompt #%d: %v\n", it.Number, ferr)
			return 1
		}
		fstats, ferr := runPhase(wtDir, buildAgent, fixPrompt, trace(fmt.Sprintf("fix%d", fix)),
			time.Duration(buildAgent.BudgetSec)*time.Second)
		addStats(fstats, buildAgent)
		if ferr != nil {
			return fail("agent_failed", "fix", ferr.Error())
		}
		changed, rep, gerr = gate(wtDir, baseSHA, cfg.Protected, buildSchema)
		if gerr != nil {
			return fail("gate_failed", "gate", gerr.Error())
		}
	}

	if verdict != "approve" {
		// The review never approved. Park the item for a human: comment the
		// feedback and mark it failed so the loop does not retry forever.
		msg := "review requested changes: " + reviewNotes
		record.Status = "changes_requested"
		record.Error = msg
		_ = appendRun(workspace, record)
		_ = failIssue(cfg.Repo, it.Number, msg)
		fmt.Printf("forest: #%d changes requested: %s\n", it.Number, reviewNotes)
		return 0
	}

	if err := commitAndPush(repoDir, wtDir, branch, it); err != nil {
		return fail("publish_failed", "publish", err.Error())
	}

	// the pull request body lists exactly what is in the pushed commit, so the
	// run artifacts never appear as changed files.
	committed, err := gitStrings(wtDir, "diff", "--name-only", baseSHA, "HEAD")
	if err != nil {
		committed = changed
	}
	prURL, err := openPR(cfg.Repo, branch, "forest: "+it.Title, prBody(it, rep, committed))
	if err != nil {
		return fail("pr_failed", "pr", err.Error())
	}
	record.PRURL = prURL
	record.Status = "done"
	_ = appendRun(workspace, record)
	if err := closeIssue(cfg.Repo, it.Number); err != nil {
		fmt.Fprintf(os.Stderr, "forest: close #%d: %v (PR is open)\n", it.Number, err)
	}
	fmt.Printf("forest: #%d done: %s (review=%s)\n", it.Number, prURL, verdict)
	return 0
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
