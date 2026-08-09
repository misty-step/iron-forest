package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// managerSubject identifies the Manager's one judgement slot per pass. The
// lane can keep ReadyDepth assignments in flight across passes, but it never
// names an Item before the model has judged it. Manager Subjects carry only
// this key, the plan Revision, and the operator Label.
const managerSubject = "manager"

// managerFlow keeps up to the configured ready depth of unstarted assignments.
// It reads the open items, filters a candidate set deterministically (unblocked,
// unbranched, unstalled), asks one agent pass to pick one of them, and lays
// readyTag on that pick. It writes no code, branch, comment, or merge. The
// Builder loop is unchanged and selects the tag the Manager laid.
//
// The Manager never dispatches the Builder. Its only fact about the Builder is
// the repository. An item that carries readyTag but has no remote forest branch
// occupies one slot throughout an in-progress build because commitAndPush runs
// only after the agent and Gate. ReadyDepth bounds Manager decisions against
// Builder work, while each Manager pass promotes at most one candidate.
type managerFlow struct{}

func (managerFlow) Name() string { return "manager" }

func (managerFlow) Interval(cfg Config) time.Duration {
	return time.Duration(cfg.Flows.Manager.IntervalSec) * time.Second
}

func (managerFlow) Enabled(cfg Config) bool { return cfg.Flows.Manager.Enabled }

// Select returns a single manager subject wherever the slot is empty and there
// are candidates to judge, or where an assigned item must be reaped. It is a
// pure read: it lists items, reads branches, and consults the stalled brake,
// none of which write. The singleton Revision is a deterministic stamp over the
// candidate set, so the repeat-failure brake holds until the backlog moves.
func (managerFlow) Select(cfg Config, repoDir string) ([]Subject, error) {
	updateGate.RLock()
	defer updateGate.RUnlock()
	tk := trackerFor(cfg.Repo)
	items, err := validatedTrackerItems(tk)
	if err != nil {
		return nil, fmt.Errorf("items: %w", err)
	}
	branches, err := forestBranches(repoDir)
	if err != nil {
		return nil, err
	}
	retiring, err := retirementItemIDs(repoDir)
	if err != nil {
		return nil, err
	}
	plan, err := buildManagerPlan(cfg.Flows.Manager, repoDir, items, branches, retiring)
	if err != nil {
		return nil, err
	}
	if !plan.hasWork() {
		return nil, nil
	}
	if plan.braked && len(plan.reap) == 0 {
		// The only work left is the braked promote judgement; retrying it on
		// unchanged input is forbidden. A deterministic reap, in contrast, is
		// free and always surfaces the subject so Act can free the slot.
		return nil, nil
	}
	return []Subject{{
		Key:      managerSubject,
		Kind:     subjectManager,
		Revision: plan.revision,
		Label:    plan.label,
	}}, nil
}

// Act executes a Manager pass on the singleton subject: it reaps any dead
// assignment, then, when the slot is empty, runs the model to pick one candidate
// and lays readyTag on that pick. Reaping is a write, which is why it lives in
// Act and never in Select.
func (managerFlow) Act(cfg Config, repoDir string, s Subject, runID string) (Outcome, error) {
	tk := trackerFor(cfg.Repo)
	items, err := validatedTrackerItems(tk)
	if err != nil {
		return Outcome{Status: "item_failed"}, fmt.Errorf("items: %w", err)
	}
	branches, err := forestBranches(repoDir)
	if err != nil {
		return Outcome{Status: "branch_failed"}, fmt.Errorf("branches: %w", err)
	}
	retiring, err := retirementItemIDs(repoDir)
	if err != nil {
		return Outcome{Status: "branch_failed"}, fmt.Errorf("retirements: %w", err)
	}
	plan, err := buildManagerPlan(cfg.Flows.Manager, repoDir, items, branches, retiring)
	if err != nil {
		return Outcome{Status: "flow_failed"}, err
	}
	if s.Revision == "" || s.Revision != plan.revision {
		return Outcome{Status: "stale"}, nil
	}

	// Withdraw assigned items first. Each Tracker write shares the Item admission
	// key with the Builder and Verifier, then rechecks durable coverage after
	// taking it.
	reaped := 0
	for _, it := range plan.reap {
		changed, err := mutateManagerItem(cfg, repoDir, tk, it,
			nil, []string{readyTag}, "")
		if err != nil {
			return Outcome{Status: "tracker_failed"}, fmt.Errorf("reap item %s: %w", it.ID, err)
		}
		if changed {
			reaped++
		}
	}

	if reaped != len(plan.reap) {
		// The snapshot expected these assignments to leave the slot. Admission
		// or state changed before the Effect, so replan before judging a pick.
		return Outcome{Status: "stale"}, nil
	}

	if plan.braked {
		if reaped == 0 {
			return Outcome{Status: "skipped"}, nil
		}
		return Outcome{Status: "reaped"}, nil
	}
	if !plan.needModel {
		if reaped == 0 {
			return Outcome{Status: "skipped"}, nil
		}
		return Outcome{Status: "reaped"}, nil
	}

	a, err := loadAgent(repoDir, cfg.Flows.Manager.Agent)
	if err != nil {
		return Outcome{Status: "agent_failed"}, fmt.Errorf("agent: %w", err)
	}
	rep, stats, err := managerJudge(repoDir, plan.cands, a, runID)
	out := Outcome{
		Agent: a.Name, Model: a.Model, DefSHA: a.DefSHA,
	}
	// The common accounting path copies every measured token class a phase
	// returns — input, output, cache read, cache write, and reasoning — so the
	// Manager records all spend, not just the fresh input and output. Copying
	// only two fields here would be the discard defect the ledger exists to
	// prevent.
	out.addTokens(stats)
	if err != nil {
		// A mechanical prompt-delivery failure names itself prompt_failed rather
		// than agent_failed: the same prompt fails identically, so it must park
		// instead of spending a judge attempt it can never satisfy. A run that
		// exceeded its declared deadline is the same shape: it parks
		// (timeout_failed) instead of being re-judged.
		out.Status = "agent_failed"
		if isPromptDelivery(err) {
			out.Status = "prompt_failed"
		}
		if isRunTimeout(err) {
			out.Status = "timeout_failed"
		}
		return out, err
	}
	var picked Item
	for _, it := range plan.cands {
		if it.ID == rep.Pick {
			picked = it
			break
		}
	}
	if picked.ID == "" {
		out.Status = "refused"
		return out, fmt.Errorf("refused: pick %q is outside the candidate set", rep.Pick)
	}
	promoted, err := mutateManagerItem(cfg, repoDir, tk, picked, []string{readyTag}, nil, plan.revision)
	if err != nil {
		out.Status = "tracker_failed"
		return out, fmt.Errorf("promote item %s: %w", rep.Pick, err)
	}
	if !promoted {
		out.Status = "stale"
		return out, nil
	}
	out.Status = "done"
	return out, nil
}

// mutateManagerItem shares the canonical Item claim with code-producing Flows.
// It rechecks branches, retirements, and the current plan under exactly one
// admission immediately before the tag Effect.
func mutateManagerItem(cfg Config, repoDir string, tk Tracker, it Item, add, remove []string, expectedRevision string) (bool, error) {
	release, err := claimAdmission(repoDir, cfg.Repo, "manager", Subject{
		Key: "item-" + it.ID, Kind: subjectItem, ID: it.ID, Revision: it.UpdatedAt,
	})
	if errors.Is(err, errAdmissionHeld) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer release()

	items, err := validatedTrackerItems(tk)
	if err != nil {
		return false, err
	}
	branches, err := forestBranches(repoDir)
	if err != nil {
		return false, err
	}
	retiring, err := retirementItemIDs(repoDir)
	if err != nil {
		return false, err
	}
	plan, err := buildManagerPlan(cfg.Flows.Manager, repoDir, items, branches, retiring)
	if err != nil {
		return false, err
	}
	if expectedRevision != "" {
		// A judged promotion must still match the plan the model saw: same
		// Revision, a judgement still wanted, no new withdrawal, and the pick
		// still a candidate.
		if plan.revision != expectedRevision || !plan.needModel || len(plan.reap) != 0 {
			return false, nil
		}
		for _, fresh := range plan.cands {
			if fresh.ID == it.ID {
				if err := tk.SetTags(it.ID, add, remove); err != nil {
					return false, err
				}
				return true, nil
			}
		}
		return false, nil
	}
	for _, fresh := range plan.reap {
		if fresh.ID != it.ID || fresh.UpdatedAt != it.UpdatedAt {
			continue
		}
		add = nil
		for _, failed := range plan.failed {
			if failed.ID == it.ID {
				add = []string{failedLabel}
				break
			}
		}
		if err := tk.SetTags(it.ID, add, remove); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

// managerPlan is the deterministic filter and slot accounting behind one Manager
// pass. It is a pure computation over the open items and branches; the only lane
// judgement it leaves to the model is ranking the returned candidates.
type managerPlan struct {
	revision  string
	label     string
	needModel bool
	braked    bool   // the promote judgement on the current candidate set is braked
	reap      []Item // assigned items to withdraw
	failed    []Item // withdrawn items that also receive forest:failed
	cands     []Item // unblocked candidates the model may judge
}

// hasWork reports whether the plan produced anything for Act to do.
func (p managerPlan) hasWork() bool {
	return len(p.reap) > 0 || p.needModel
}

// buildManagerPlan builds the plan for one pass. An assigned item (open, ready
// and unbranched) is withdrawn for a configured exclusion or a durable failure:
// it is stalled on the builder flow, it carries forest:failed, or a blocker
// reopened. A closed item needs no reap: it leaves ListOpen, so the slot frees
// and nothing can build it. Everything else that is open, unbranched, unexcluded,
// unstalled, and unblocked is a candidate. The slot holds readyDepth healthy
// assigned items; only an empty slot calls the model.
func buildManagerPlan(cfg ManagerFlowCfg, repoDir string, items []Item, branches, retiring []string) (managerPlan, error) {
	covered := make(map[string]bool, len(branches)+len(retiring))
	for _, branch := range branches {
		covered[itemIDFromBranch(branch)] = true
	}
	for _, id := range retiring {
		covered[id] = true
	}
	open := make(map[string]Item, len(items))
	for _, it := range items {
		open[it.ID] = it
	}
	plan := managerPlan{}
	var reap []Item
	var failed []Item
	healthyAssigned := 0
	// Slot accounting and reaping run across the open, ready, branchless
	// assignments. A branch owns a ready item's slot and queue position
	// downstream, so a covered item is left alone; an open one is counted healthy
	// or withdrawn on a durable fact or a configured exclusion. Closed items never
	// appear here.
	for _, it := range items {
		if !it.hasTag(readyTag) {
			continue
		}
		if covered[it.ID] {
			continue
		}
		excluded := hasExcludedLabel(it, cfg.ExcludeLabels)
		withdraw := excluded
		if !excluded {
			var err error
			withdraw, err = managerWithdraw(repoDir, it, open)
			if err != nil {
				return managerPlan{}, err
			}
		}
		if withdraw {
			reap = append(reap, it)
			if !excluded {
				failed = append(failed, it)
			}
		} else {
			healthyAssigned++
		}
	}

	var cands []Item
	// The model only ever judges the deterministic candidate set: open, not
	// assigned or branch-owned, unexcluded, unstalled, and unblocked.
	for _, it := range items {
		if covered[it.ID] {
			continue
		}
		if it.hasTag(readyTag) {
			// Already assigned; accounted above, never offered as a candidate.
			continue
		}
		if hasExcludedLabel(it, cfg.ExcludeLabels) {
			continue
		}
		stalled, err := stalledOn(repoDir, "builder", "item-"+it.ID, it.UpdatedAt)
		if err != nil {
			return managerPlan{}, err
		}
		if stalled {
			continue
		}
		if hasOpenBlocker(it, open) {
			// Blockers closed is a hard filter, never a suggestion: an item whose
			// Blocked by references an open item is not offered to the model at all.
			continue
		}
		cands = append(cands, it)
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].ID < cands[j].ID })
	sort.Slice(reap, func(i, j int) bool { return reap[i].ID < reap[j].ID })
	depth := cfg.ReadyDepth
	if depth < 1 {
		depth = 1
	}
	needsModel := healthyAssigned < depth && len(cands) > 0
	plan.needModel = needsModel
	plan.reap = reap
	plan.cands = cands
	plan.failed = failed
	if needsModel {
		plan.revision = itemSetStamp(cands)
		plan.label = fmt.Sprintf("manager: %d candidate(s), slot open", len(cands))
		// The promote judgement is braked when the same candidate set already
		// failed enough times: do not rejudge unchanged input. The stamp over
		// the candidate set means a single failure does not brake the lane
		// forever — the moment the backlog moves, the revision moves and the
		// Manager becomes eligible again. Reaping is deterministic and free, so
		// it is never gated by this brake.
		braked, err := stalledOn(repoDir, "manager", managerSubject, plan.revision)
		if err != nil {
			return managerPlan{}, err
		}
		plan.braked = braked
	} else {
		plan.revision = itemSetStamp(reap)
		if len(reap) > 0 {
			plan.label = fmt.Sprintf("manager: reaping %d dead assignment(s)", len(reap))
		} else {
			plan.label = "manager: slot occupied, nothing to do"
		}
	}
	return plan, nil
}

// managerWithdraw reports whether an assigned item is a dead assignment that
// must be reaped: it only ends on a durable fact, never on a change of mind.
// A promotion is withdrawn when the item is stalled on the builder flow, carries
// forest:failed, or has a Blocked by reference that reopened against an open
// item. Reaping removes readyTag and adds forest:failed so the slot frees and
// the Builder never starves on a dead promotion.
func managerWithdraw(repoDir string, it Item, open map[string]Item) (bool, error) {
	if it.hasTag(failedLabel) {
		return true, nil
	}
	if hasOpenBlocker(it, open) {
		return true, nil
	}
	stalled, err := stalledOn(repoDir, "builder", "item-"+it.ID, it.UpdatedAt)
	if err != nil {
		return false, err
	}
	return stalled, nil
}

// itemSetStamp is a deterministic Revision over every candidate identity and
// update stamp. It changes whenever the judged set changes.
func itemSetStamp(items []Item) string {
	parts := make([]string, len(items))
	for i, it := range items {
		parts[i] = fmt.Sprintf("%d:%s:%d:%s", len(it.ID), it.ID, len(it.UpdatedAt), it.UpdatedAt)
	}
	sort.Strings(parts)
	return blobSHA(strings.Join(parts, "\n"))
}

// managerReport is the one file the Manager's agent writes. It names one pick
// from the candidate set; the controller applies the tag.
type managerReport struct {
	Pick string `json:"pick"`
}

// managerJudge runs the one judgement pass and reads its report, returning the
// run's token accounting so Act records every Manager model invocation on the
// ledger. It is a var so the refusal and brake tests can stub the model without
// driving a real agent, matching how trackerFor and ghJSON are injectable.
var managerJudge = runManagerJudge

// runManagerJudge runs the model in an empty directory outside the checkout.
func runManagerJudge(repoDir string, cands []Item, a *Agent, runID string) (managerReport, runStats, error) {
	// Keep the Manager cwd outside the checkout so its agent cannot modify repository state.
	runDir, err := os.MkdirTemp("", "forest-manager-")
	if err != nil {
		return managerReport{}, runStats{}, err
	}
	defer func() { _ = os.RemoveAll(runDir) }()

	prompt, err := renderManagerPrompt(a, cands)
	if err != nil {
		return managerReport{}, runStats{}, err
	}
	trace := filepath.Join(workspaceDir(repoDir), "runs", runID+".manager.jsonl")
	stats, err := runPhase(repoDir, runDir, a, prompt, trace)
	if err != nil {
		return managerReport{}, stats, err
	}
	if err := checkSchema(filepath.Join(runDir, "report.json"),
		filepath.Join(a.Dir, "report.schema.json")); err != nil {
		return managerReport{}, stats, err
	}
	rep, err := readManagerReportFile(runDir)
	return rep, stats, err
}

// renderManagerPrompt renders the judgement request for one candidate set. The
// agent ranks the offered items and returns one, so the prompt is scoped to the
// filtered set and never to the whole backlog. Each candidate carries its body,
// tags, and comment thread so the model can rank by their content, not just by
// id and title.
func renderManagerPrompt(a *Agent, cands []Item) (string, error) {
	var b strings.Builder
	b.WriteString("Choose exactly one item to promote next. Return it in report.json as {\"pick\": \"<id>\"}.\n\n")
	b.WriteString("Candidates:\n")
	for _, it := range cands {
		content := it.Body
		if comments := renderComments(it.Comments); comments != "" {
			content += "\n\n## Item comments\n" + comments
		}
		tagList := append([]string(nil), it.Tags...)
		sort.Strings(tagList)
		tags := strings.Join(tagList, ", ")
		if tags == "" {
			tags = "(none)"
		}
		fmt.Fprintf(&b, "- %s %s (updated %s, tags %s)\n%s\n", it.ID, it.Title, it.UpdatedAt, tags, strings.TrimSpace(content))
	}
	return renderUserPrompt(a, map[string]any{"Task": b.String()})
}

// readManagerReportFile requires valid JSON with one non-empty pick. Candidate
// membership is a controller refusal, not a report parse failure.
func readManagerReportFile(wtDir string) (managerReport, error) {
	var rep managerReport
	raw, err := os.ReadFile(filepath.Join(wtDir, "report.json"))
	if err != nil {
		return rep, fmt.Errorf("report.json missing: %w", err)
	}
	if err := json.Unmarshal(raw, &rep); err != nil {
		return rep, fmt.Errorf("report.json is invalid JSON: %w", err)
	}
	if rep.Pick == "" {
		return rep, fmt.Errorf("report.json must name one candidate (field \"pick\")")
	}
	return rep, nil
}

// hasOpenBlocker reports whether a comma-separated Blocked by reference names
// an open Item. References preserve opaque identity bytes after an optional #.
func hasOpenBlocker(it Item, open map[string]Item) bool {
	for _, line := range strings.Split(it.Body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(strings.ToLower(line), "blocked by") {
			continue
		}
		line = strings.TrimSpace(line[len("blocked by"):])
		line = strings.TrimSpace(strings.TrimPrefix(line, ":"))
		for _, ref := range strings.Split(line, ",") {
			ref = strings.TrimSpace(ref)
			if _, ok := open[ref]; ok {
				return true
			}
			if strings.HasPrefix(ref, "#") {
				if _, ok := open[strings.TrimPrefix(ref, "#")]; ok {
					return true
				}
			}
		}
	}
	return false
}
