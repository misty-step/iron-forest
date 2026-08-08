package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// managerSubject is the Manager lane's singleton subject key. The lane owns one
// slot in the ready queue, so it acts on at most one thing per pass and never
// names an item before the model has judged it.
const managerSubject = "manager"

// managerFlow is the lane that keeps exactly one unstarted assignment in the
// ready queue. It reads the open items, filters a candidate set deterministically
// (unblocked, unbranched, unstalled), asks one agent pass to pick one of them,
// and lays readyTag on that pick. It writes no code, branch, comment, or merge;
// the Builder loop is unchanged and simply selects the tag the Manager laid.
//
// The Manager never dispatches the Builder. Its only fact about the Builder is
// the repository: an item that carries readyTag but has no remote forest branch
// occupies the slot for the whole of an in-progress build, because commitAndPush
// runs only after the agent and the gate. Depth one therefore means one Manager
// decision per Builder run, bounding Manager spend against Builder spend.
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
	items, err := tk.ListOpen()
	if err != nil {
		return nil, fmt.Errorf("items: %w", err)
	}
	branches, err := forestBranches(repoDir)
	if err != nil {
		return nil, err
	}
	plan, err := buildManagerPlan(cfg.Flows.Manager, repoDir, items, branches)
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
		Kind:     "manager",
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
	items, err := tk.ListOpen()
	if err != nil {
		return Outcome{Status: "item_failed"}, fmt.Errorf("items: %w", err)
	}
	branches, err := forestBranches(repoDir)
	if err != nil {
		return Outcome{Status: "branch_failed"}, fmt.Errorf("branches: %w", err)
	}
	plan, err := buildManagerPlan(cfg.Flows.Manager, repoDir, items, branches)
	if err != nil {
		return Outcome{Status: "flow_failed"}, err
	}

	// Reap first: a dead assignment holds the slot only until its durable
	// failure is withdrawn, so one failed build never starves the Builder.
	for _, it := range plan.reap {
		if err := reapManagerItem(tk, it); err != nil {
			return Outcome{Status: "tracker_failed"}, fmt.Errorf("reap item %s: %w", it.ID, err)
		}
	}

	if plan.braked {
		// The promote judgement for this unchanged candidate set is braked, and
		// this pass exists only to reap: Select will not hand a braked,
		// reapless pass a subject. Do not retry the braked judgement. The reap
		// above has already freed the slot.
		if len(plan.reap) == 0 {
			return Outcome{Status: "skipped"}, nil
		}
		return Outcome{Status: "reaped"}, nil
	}
	if !plan.needModel {
		if len(plan.reap) == 0 {
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
	promoted, err := applyManagerPick(tk, plan.cands, rep.Pick)
	if err != nil {
		out.Status = "tracker_failed"
		return out, fmt.Errorf("promote item %s: %w", rep.Pick, err)
	}
	if !promoted {
		// The model named no candidate it was offered (a hallucination, or a
		// blocked item the hard filter already removed). Promotes nothing. It
		// records a "refused" status on the ledger and, because actOnSubject
		// sees a non-nil error, engages the repeat-failure brake: an unchanged
		// candidate set must not be re-judged every pass, so the lane waits for
		// the backlog to move instead of re-sending the same judgement forever.
		out.Status = "refused"
		return out, fmt.Errorf("refused: pick %q is outside the candidate set", rep.Pick)
	}
	out.Status = "done"
	return out, nil
}

// managerPlan is the deterministic filter and slot accounting behind one Manager
// pass. It is a pure computation over the open items and branches; the only lane
// judgement it leaves to the model is ranking the returned candidates.
type managerPlan struct {
	key       string
	revision  string
	label     string
	needModel bool
	braked    bool   // the promote judgement on the current candidate set is braked
	reap      []Item // assigned items to withdraw
	cands     []Item // unblocked candidates the model may judge
}

// hasWork reports whether the plan produced anything for Act to do.
func (p managerPlan) hasWork() bool {
	return len(p.reap) > 0 || p.needModel
}

// buildManagerPlan builds the plan for one pass. An assigned item (open, ready
// and unbranched) is reaped on any durable failure: it is stalled on the builder
// flow, it carries forest:failed, or a blocker reopened. A closed item needs no
// reap: it leaves ListOpen, so the slot frees and nothing can build it. Everything
// else that is open, unbranched, unexcluded, unstalled, and unblocked is a
// candidate. The slot holds readyDepth healthy assigned items; only an empty slot
// calls the model.
func buildManagerPlan(cfg ManagerFlowCfg, repoDir string, items []Item, branches []string) (managerPlan, error) {
	covered := make(map[string]bool, len(branches))
	for _, branch := range branches {
		covered[itemIDFromBranch(branch)] = true
	}
	open := make(map[string]Item, len(items))
	for _, it := range items {
		open[it.ID] = it
	}
	plan := managerPlan{key: managerSubject}
	var reap []Item
	healthyAssigned := 0
	// Slot accounting and reaping run across the open, ready, branchless
	// assignments. A branch owns a ready item's slot and queue position
	// downstream, so a covered item is left alone; an open one is counted healthy
	// or withdrawn on a durable fact. Closed items never appear here.
	for _, it := range items {
		if !it.hasTag(readyTag) {
			continue
		}
		if covered[it.ID] {
			continue
		}
		withdraw, err := managerWithdraw(repoDir, it, open)
		if err != nil {
			return managerPlan{}, err
		}
		if withdraw {
			reap = append(reap, it)
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
		if hasExcludedTag(it, cfg.ExcludeTags) {
			continue
		}
		stalled, err := stalledOn(repoDir, "builder", "item-"+it.ID, it.UpdatedAt)
		if err != nil {
			return managerPlan{}, err
		}
		if stalled {
			continue
		}
		if len(openBlockers(it, open)) > 0 {
			// Blockers closed is a hard filter, never a suggestion: an item whose
			// Blocked by references an open item is not offered to the model at all.
			continue
		}
		cands = append(cands, it)
	}
	depth := cfg.ReadyDepth
	if depth < 1 {
		depth = 1
	}
	needsModel := healthyAssigned < depth && len(cands) > 0
	plan.needModel = needsModel
	plan.reap = reap
	plan.cands = cands
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
		parked, err := parkedOn(repoDir, "manager", managerSubject, plan.revision)
		if err != nil {
			return managerPlan{}, err
		}
		plan.braked = braked || parked
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
	if len(openBlockers(it, open)) > 0 {
		return true, nil
	}
	stalled, err := stalledOn(repoDir, "builder", "item-"+it.ID, it.UpdatedAt)
	if err != nil {
		return false, err
	}
	return stalled, nil
}

// itemSetStamp is a deterministic revision over a set of items: the count plus
// the newest update stamp. A constant or empty stamp would break the brake, so
// the Manager's Revision is always this stamp, and the brake releases the moment
// the set it judged changes.
func itemSetStamp(items []Item) string {
	newest := ""
	for _, it := range items {
		if it.UpdatedAt > newest {
			newest = it.UpdatedAt
		}
	}
	return fmt.Sprintf("%d/%s", len(items), newest)
}

// inCandidateSet reports whether id is one of the offered candidates.
func inCandidateSet(cands []Item, id string) bool {
	for _, c := range cands {
		if c.ID == id {
			return true
		}
	}
	return false
}

// applyManagerPick lays readyTag on exactly one validated candidate pick. It
// returns false, with no effect, when the pick is not one of the offered
// candidates, so a hallucinated or blocked id promotes nothing and records a
// refusal instead of failing the pass.
func applyManagerPick(tk Tracker, cands []Item, pick string) (bool, error) {
	if !inCandidateSet(cands, pick) {
		return false, nil
	}
	if err := tk.SetTags(pick, []string{readyTag}, nil); err != nil {
		return false, err
	}
	return true, nil
}

// reapManagerItem withdraws one dead assignment: it removes readyTag and adds
// forest:failed. Without this a single failed build occupies the slot forever
// and the Builder starves.
func reapManagerItem(tk Tracker, it Item) error {
	return tk.SetTags(it.ID, []string{failedLabel}, []string{readyTag})
}

// managerReport is the one file the Manager's agent writes. It names a single
// pick from the candidate set plus a short reason. The controller applies the
// tag; the agent only proposes.
type managerReport struct {
	Pick   string `json:"pick"`
	Reason string `json:"reason"`
}

// managerJudge runs the one judgement pass and reads its report, returning the
// run's token accounting so Act records every Manager model invocation on the
// ledger. It is a var so the refusal and brake tests can stub the model without
// driving a real agent, matching how trackerFor and ghJSON are injectable.
var managerJudge = runManagerJudge

// runManagerJudge runs the model in a throwaway directory, asks it to pick one
// candidate, and reads its report. The run directory is not a checkout, so the
// lane has no repository to touch; the Manager's whole effect on the world is
// the tags Act applies afterward.
func runManagerJudge(repoDir string, cands []Item, a *Agent, runID string) (managerReport, runStats, error) {
	runDir, cleanup, err := createManagerRunDir()
	if err != nil {
		return managerReport{}, runStats{}, err
	}
	defer cleanup()

	prompt, err := renderManagerPrompt(a, cands)
	if err != nil {
		return managerReport{}, runStats{}, err
	}
	trace := filepath.Join(workspaceDir(repoDir), "runs", runID+".manager.jsonl")
	stats, err := runPhase(repoDir, runDir, a, prompt, trace)
	if err != nil {
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
	b.WriteString("Choose exactly one item to promote next. Return it in report.json as {\"pick\": \"<id>\", \"reason\": \"<short reason>\"}.\n\n")
	b.WriteString("Candidates:\n")
	for _, it := range cands {
		content := it.Body
		if comments := renderComments(it.Comments); comments != "" {
			content += "\n\n## Item comments\n" + comments
		}
		tags := strings.Join(it.Tags, ", ")
		if tags == "" {
			tags = "(none)"
		}
		fmt.Fprintf(&b, "- %s %s (updated %s, tags %s)\n%s\n", it.ID, it.Title, it.UpdatedAt, tags, strings.TrimSpace(content))
	}
	return renderUserPrompt(a, map[string]any{
		"Task":  b.String(),
		"Items": strings.TrimSpace(b.String()),
	})
}

// readManagerReportFile reads the agent's report and enforces its structure: the
// file must exist, parse as JSON, and name a non-empty pick with a reason. It
// deliberately does not check membership in the candidate set: an out-of-set pick
// is a refusal that records on the ledger, not a pass failure that would drive the
// brake.
func readManagerReportFile(wtDir string) (managerReport, error) {
	var rep managerReport
	raw, err := os.ReadFile(filepath.Join(wtDir, "report.json"))
	if err != nil {
		return rep, fmt.Errorf("report.json missing: %w", err)
	}
	if err := json.Unmarshal(raw, &rep); err != nil {
		return rep, fmt.Errorf("report.json is invalid JSON: %w", err)
	}
	rep.Pick = strings.TrimSpace(rep.Pick)
	if rep.Pick == "" {
		return rep, fmt.Errorf("report.json must name one candidate (field \"pick\")")
	}
	if strings.TrimSpace(rep.Reason) == "" {
		return rep, fmt.Errorf("report.json must give a reason for the pick (field \"reason\")")
	}
	return rep, nil
}

// createManagerRunDir makes a throwaway empty directory for the Manager's agent
// to run in. It lives outside the checkout, in system temp, following #174's
// pattern for the agent config root: with bash allowed the agent's cwd is not
// under any enclosing git checkout, so a lane that claims no repository
// authority cannot discover and modify the managed repository or its refs. The
// lane reads the Tracker and writes one report; it needs no repository at all.
func createManagerRunDir() (string, func(), error) {
	dir, err := os.MkdirTemp("", "forest-manager-")
	if err != nil {
		return "", nil, err
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

// openBlockers returns the Blocked by references of an item that name items
// still open on the tracker. A promoted item's blockers must be closed: ordering
// emerges from declared dependencies, so the Manager never lifts an item ahead
// of one of its open dependencies.
func openBlockers(it Item, open map[string]Item) []string {
	var blockers []string
	for _, ref := range blockedRefs(it.Body) {
		if _, ok := open[ref]; ok {
			blockers = append(blockers, ref)
		}
	}
	return blockers
}

// blockedRefs extracts the issue references a `Blocked by:` prose line lists.
func blockedRefs(body string) []string {
	var refs []string
	seen := make(map[string]bool)
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(strings.ToLower(line), "blocked by") {
			continue
		}
		if i := strings.IndexByte(line, ':'); i >= 0 {
			line = line[i+1:]
		}
		for _, tok := range strings.FieldsFunc(line, func(r rune) bool {
			return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' ||
				r >= 'A' && r <= 'Z' || r == '_' || r == '-')
		}) {
			ref := strings.TrimPrefix(tok, "#")
			if ref == "" || seen[ref] {
				continue
			}
			seen[ref] = true
			refs = append(refs, ref)
		}
	}
	return refs
}
