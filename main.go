package main

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func usage() {
	fmt.Fprintf(os.Stderr, `forest — the iron-forest piranha tank

  forest list              print the current backlog
  forest agents            list declared agents and their composition digest
  forest stats [--json]    aggregate the run ledger
  forest once <issue>      chew a single issue end to end
  forest chew              poll: chew backlog AND watch open factory PRs
  forest reconcile         report orphaned PRs (closed issue, unmerged change)
  forest version           print the git sha this binary was built from
  forest selfcheck         offline smoke gate (config + agents load)
  forest watch [--interval 2s] [--live-gh]
                          live operator board over .forest/ + daemon
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
	case "stats":
		return cmdStats(repoDir, args[1:])
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
		lock, err := acquireSingletonLock(repoDir)
		if err != nil {
			fmt.Fprintln(os.Stderr, "forest:", err)
			return 1
		}
		defer lock.Close()
		it, err := getIssue(cfg.Repo, n)
		if err != nil {
			fmt.Fprintln(os.Stderr, "forest:", err)
			return 1
		}
		return chewOne(cfg, repoDir, it)
	case "chew":
		return chewLoop(cfg, repoDir)
	case "reconcile":
		return cmdReconcile(cfg, repoDir)
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
		budget := "none"
		if a.BudgetSec > 0 {
			budget = fmt.Sprintf("%ds", a.BudgetSec)
		}
		fmt.Printf("%s\tmodel=%s%s mode=%s steps=%d budget=%s price=%.2f/%.2f def_sha=%s\n",
			n, a.Model, variantSuffix(a), a.Mode, a.Steps, budget,
			a.PriceInUSDPerM, a.PriceOutUSDPerM, a.DefSHA)
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

// chewLoop is the single piranha loop: it chews the backlog and, in the same
// pass, watches every open factory PR (reaction loop) until it merges or
// stalls. One loop, one serializer.
func chewLoop(cfg Config, repoDir string) int {
	fmt.Printf("forest v%s: chewing %s every %ds\n", version, cfg.Repo, cfg.PollIntervalSec)
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	var drain int32
	go func() {
		<-sig // first signal: drain the in-flight agent, then stop at a pass boundary
		atomic.StoreInt32(&drain, 1)
		fmt.Fprintln(os.Stderr, "forest: draining, waiting for the in-flight agent")
		<-sig // second signal: the operator's only clock, since agents are unbounded
		fmt.Fprintln(os.Stderr, "forest: second signal, exiting now")
		os.Exit(1)
	}()
	lock, err := acquireSingletonLock(repoDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "forest:", err)
		return 1
	}
	defer lock.Close()
	for {
		if atomic.LoadInt32(&drain) == 1 {
			fmt.Fprintln(os.Stderr, "forest: draining, no new pass")
			return 0
		}
		runUpdateCheck(repoDir)
		if nc, err := loadConfig(filepath.Join(repoDir, "forest.yaml")); err == nil {
			cfg = nc
		}
		items, err := backlog(cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "forest: backlog: %v\n", err)
		} else {
			if len(items) == 0 {
				fmt.Printf("forest: backlog empty, sleeping %ds\n", cfg.PollIntervalSec)
			} else {
				// One item per pass. Chewing the whole backlog before watching a
				// single PR starves the merge gate for as long as the queue is,
				// and every later build then branches from a master missing the
				// work already approved and waiting in an unmerged PR.
				for _, it := range items {
					fmt.Printf("forest: chewing #%d %s\n", it.Number, it.Title)
					code := chewOne(cfg, repoDir, it)
					if code == codeUnclaimable {
						// Another worker owns it. Try the next candidate rather
						// than spending the pass on an item we cannot touch.
						continue
					}
					if code != 0 {
						fmt.Fprintf(os.Stderr, "forest: #%d failed\n", it.Number)
					}
					break
				}
			}
		}
		if prs, err := listOpenForestPRs(cfg.Repo); err != nil {
			fmt.Fprintf(os.Stderr, "forest: pr list: %v\n", err)
		} else {
			for _, n := range prs {
				watchPR(cfg, repoDir, n)
				time.Sleep(2 * time.Second)
			}
		}
		time.Sleep(time.Duration(cfg.PollIntervalSec) * time.Second)
	}
}

// codeUnclaimable is chewOne's answer when another worker already owns the item.
// It is not a failure: nothing was built, spent, or decided, so the pass moves
// to the next candidate instead of burning itself on an item it cannot touch.
const codeUnclaimable = 2

// chewOne runs the whole workflow for one new issue: claim, worktree, build,
// gate, review, publish, record. It opens the PR and parks the item; the
// reaction loop in chewLoop drives the PR from there.
func chewOne(cfg Config, repoDir string, it issue) int {
	runID := fmt.Sprintf("%s-%d", time.Now().UTC().Format("20060102T150405Z"), it.Number)
	workspace := filepath.Join(repoDir, WorkspaceDir)
	_ = ensureLabels(cfg.Repo)

	if _, err := loadAgent(repoDir, cfg.Workflow.Build); err != nil {
		fmt.Fprintf(os.Stderr, "forest: build agent: %v\n", err)
		return 1
	}
	id := identify(repoDir, cfg)
	record := runRecord{
		Time: nowRFC(), RunID: runID, Issue: it.Number,
		Agent: id.Agents, Model: id.Models, DefSHA: id.DefSHA,
	}

	if err := claimIssue(cfg.Repo, it.Number); err != nil {
		if errors.Is(err, errAlreadyClaimed) {
			// Not a run: nothing was built, spent, or decided. Recording it as a
			// failure buried the ledger under identical zero-cost rows and, while
			// this item stayed first in the backlog, starved every card behind it.
			fmt.Fprintf(os.Stderr, "forest: #%d claimed by another worker, skipping\n", it.Number)
			return codeUnclaimable
		}
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

	r, err := runPick(cfg, repoDir, wtDir, baseSHA, runID, it, "")
	record.CostUSD += r.Cost
	record.TokensIn += r.TokIn
	record.TokOut += r.TokOut
	record.ReviewVerdict = r.Verdict
	if err != nil {
		record.Status = failStatus(err)
		record.Error = err.Error()
		_ = appendRun(workspace, record)
		_ = failIssue(cfg.Repo, it.Number, err.Error())
		fmt.Fprintf(os.Stderr, "forest: #%d: %s\n", it.Number, err.Error())
		return 1
	}
	if r.Verdict != "approve" {
		// The owl never approved. Park the item for a human: comment the
		// feedback and mark it failed so the loop does not retry it forever.
		msg := "review requested changes: " + r.Notes
		record.Status = "changes_requested"
		record.Error = msg
		_ = appendRun(workspace, record)
		_ = failIssue(cfg.Repo, it.Number, msg)
		fmt.Printf("forest: #%d changes requested: %s\n", it.Number, r.Notes)
		return 0
	}

	if err := commitAndPush(repoDir, wtDir, branch, it); err != nil {
		record.Status = "publish_failed"
		record.Error = err.Error()
		_ = appendRun(workspace, record)
		_ = failIssue(cfg.Repo, it.Number, "publish: "+err.Error())
		fmt.Fprintf(os.Stderr, "forest: publish #%d: %v\n", it.Number, err)
		return 1
	}

	var committed []string
	raw, err := gitOut(wtDir, "diff", "--name-only", baseSHA, "HEAD")
	if err != nil {
		committed = r.Changed
	} else if raw != "" {
		committed = strings.Split(raw, "\n")
	}
	prURL, err := openPR(cfg.Repo, branch, "forest: "+it.Title, prBody(it, r.Rep, committed))
	if err != nil {
		record.Status = "pr_failed"
		record.Error = err.Error()
		_ = appendRun(workspace, record)
		_ = failIssue(cfg.Repo, it.Number, "pr: "+err.Error())
		fmt.Fprintf(os.Stderr, "forest: pr #%d: %v\n", it.Number, err)
		return 1
	}
	headSHA, _ := gitOut(wtDir, "rev-parse", "HEAD")
	prNum := prNumberFromURL(prURL)
	_ = appendPR(workspace, prState{
		Time: nowRFC(), PRURL: prURL, PR: prNum, Branch: branch, Issue: it.Number,
		State: "opened", Owl: r.Verdict, SHA: headSHA,
	})
	record.PRURL = prURL
	record.Status = "done"
	_ = appendRun(workspace, record)
	if err := closeIssue(cfg.Repo, it.Number); err != nil {
		fmt.Fprintf(os.Stderr, "forest: close #%d: %v (PR is open)\n", it.Number, err)
	}
	fmt.Printf("forest: #%d done: %s (review=%s)\n", it.Number, prURL, r.Verdict)
	return 0
}

// pickResult is one full build+gate+review pass over an item in a worktree.
type pickResult struct {
	Changed []string
	Rep     report
	Verdict string // owl verdict: approve | changes
	Notes   string
	TokIn   int64
	TokOut  int64
	Cost    float64
}

// runPick builds an item with the build agent, gates it, and runs the owl
// review loop (up to MaxFixIterations corrective passes). It is shared by
// chewOne (new items) and fixPR (reaction re-entries). Errors carry a stage
// prefix so callers can classify the run's status.
func runPick(cfg Config, repoDir, wtDir, baseSHA, runID string, it issue, feedback string) (pickResult, error) {
	var pr pickResult
	workspace := filepath.Join(repoDir, WorkspaceDir)
	buildAgent, err := loadAgent(repoDir, cfg.Workflow.Build)
	if err != nil {
		return pr, err
	}
	buildSchema := filepath.Join(repoDir, DefaultAgentsDir, cfg.Workflow.Build, "report.schema.json")
	trace := func(phase string) string {
		return filepath.Join(workspace, "runs", runID+"."+phase+".jsonl")
	}

	prompt, err := renderUserPrompt(buildAgent, issueData(it, feedback))
	if err != nil {
		return pr, fmt.Errorf("prompt: %w", err)
	}
	stats, err := runPhase(wtDir, buildAgent, prompt, trace("chew"),
		time.Duration(buildAgent.BudgetSec)*time.Second)
	if err != nil {
		return pr, fmt.Errorf("agent: %w", err)
	}
	pr.TokIn, pr.TokOut, pr.Cost = stats.tokensIn, stats.tokensOut, price(stats, buildAgent)
	pr.Changed, pr.Rep, err = gate(wtDir, baseSHA, cfg.Protected, buildSchema)
	if err != nil {
		return pr, fmt.Errorf("gate: %w", err)
	}
	if cfg.Workflow.Review == "" {
		pr.Verdict = "approve"
		return pr, nil
	}
	reviewAgent, err := loadAgent(repoDir, cfg.Workflow.Review)
	if err != nil {
		return pr, err
	}
	reviewSchema := filepath.Join(repoDir, DefaultAgentsDir, cfg.Workflow.Review, "report.schema.json")
	for fix := 0; fix <= cfg.Workflow.MaxFixIterations; fix++ {
		_ = git(wtDir, "add", "-A")
		_ = git(wtDir, "reset", "-q", "--", "report.json", "review.json")
		diff, derr := gitOut(wtDir, "diff", "--cached", "--", ".",
			":(exclude)report.json", ":(exclude)review.json")
		if derr != nil {
			return pr, fmt.Errorf("review: %w", derr)
		}
		rvPrompt, rerr := renderUserPrompt(reviewAgent, reviewData(it, pr.Rep, diff))
		if rerr != nil {
			return pr, fmt.Errorf("review: %w", rerr)
		}
		rvStats, rerr := runPhase(wtDir, reviewAgent, rvPrompt, trace(fmt.Sprintf("review%d", fix)),
			time.Duration(reviewAgent.BudgetSec)*time.Second)
		if rerr != nil {
			return pr, fmt.Errorf("review: %w", rerr)
		}
		pr.TokIn += rvStats.tokensIn
		pr.TokOut += rvStats.tokensOut
		pr.Cost += price(rvStats, reviewAgent)
		rv, gerr := gateReview(wtDir, reviewSchema)
		if gerr != nil {
			return pr, fmt.Errorf("review: %w", gerr)
		}
		pr.Verdict = rv.Verdict
		pr.Notes = rv.Notes
		if pr.Verdict == "approve" {
			break
		}
		if fix >= cfg.Workflow.MaxFixIterations {
			break
		}
		fixPrompt, ferr := renderUserPrompt(buildAgent, issueData(it, rv.Notes))
		if ferr != nil {
			return pr, fmt.Errorf("prompt: %w", ferr)
		}
		fstats, ferr := runPhase(wtDir, buildAgent, fixPrompt, trace(fmt.Sprintf("fix%d", fix)),
			time.Duration(buildAgent.BudgetSec)*time.Second)
		if ferr != nil {
			return pr, fmt.Errorf("agent: %w", ferr)
		}
		pr.TokIn += fstats.tokensIn
		pr.TokOut += fstats.tokensOut
		pr.Cost += price(fstats, buildAgent)
		pr.Changed, pr.Rep, gerr = gate(wtDir, baseSHA, cfg.Protected, buildSchema)
		if gerr != nil {
			return pr, fmt.Errorf("gate: %w", gerr)
		}
	}
	return pr, nil
}

// failStatus maps a stage-prefixed error to a ledger status name.
func failStatus(err error) string {
	switch s := err.Error(); {
	case strings.HasPrefix(s, "agent:"):
		return "agent_failed"
	case strings.HasPrefix(s, "gate:"):
		return "gate_failed"
	case strings.HasPrefix(s, "review:"):
		return "review_failed"
	case strings.HasPrefix(s, "prompt:"):
		return "prompt_failed"
	default:
		return "pick_failed"
	}
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
