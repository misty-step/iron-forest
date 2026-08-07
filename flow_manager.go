package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// managerMarker prefixes every comment the Manager lane writes, so a later pass
// can tell its own comment from a human's and stays silent on an unchanged item
// even if the durable judgement record is lost.
const managerMarker = "forest:manager:"

// managerSeenRef durably records, per item, the revision at which the Manager
// last judged it (an item's update stamp). An item judged at revision R is not
// judged again until R moves; the ref lives beside the stalled records so a
// pass never re-reads and re-comments on a backlog that has not changed.
const managerSeenRef = "refs/forest/manager/seen"

// managerFlow is the lane that owns the promotion queue. It reads the open
// items, asks an agent to judge their shape and declared blockers, and applies
// the promote tag to ready items up to the declared open level. It never
// creates a branch, writes code, or merges: promoting is its only effect, and
// the Builder selects promotions it has already laid down.
type managerFlow struct{}

func (managerFlow) Name() string { return "manager" }

func (managerFlow) Interval(cfg Config) time.Duration {
	return time.Duration(cfg.Flows.Manager.IntervalSec) * time.Second
}

func (managerFlow) Enabled(cfg Config) bool { return cfg.Flows.Manager.Enabled }

// Select returns a single subject covering the whole backlog when any open item
// needs a fresh judgement. Acting on one batch per pass is what lets the lane
// enforce the level across several items at once instead of one at a time.
func (managerFlow) Select(cfg Config, repoDir string) ([]Subject, error) {
	updateGate.RLock()
	defer updateGate.RUnlock()
	items, err := trackerFor(cfg.Repo).ListOpen()
	if err != nil {
		return nil, fmt.Errorf("items: %w", err)
	}
	cands, err := managerCandidates(cfg.Flows.Manager, repoDir, items)
	if err != nil {
		return nil, err
	}
	if len(cands) == 0 {
		return nil, nil
	}
	return []Subject{{
		Key:   "manager-backlog",
		Kind:  "items",
		Label: fmt.Sprintf("open backlog (%d to judge)", len(cands)),
	}}, nil
}

func (managerFlow) Act(cfg Config, repoDir string, s Subject, runID string) (Outcome, error) {
	items, err := trackerFor(cfg.Repo).ListOpen()
	if err != nil {
		return Outcome{Status: "item_failed"}, fmt.Errorf("items: %w", err)
	}
	branches, err := forestBranches(repoDir)
	if err != nil {
		return Outcome{}, fmt.Errorf("branches: %w", err)
	}
	cands, err := managerCandidates(cfg.Flows.Manager, repoDir, items)
	if err != nil {
		return Outcome{}, fmt.Errorf("candidates: %w", err)
	}
	a, err := loadAgent(repoDir, cfg.Flows.Manager.Agent)
	if err != nil {
		return Outcome{Status: "agent_failed"}, fmt.Errorf("agent: %w", err)
	}

	workspace := workspaceDir(repoDir)
	wtDir, baseSHA, err := createManagerWorktree(repoDir, workspace)
	if err != nil {
		return Outcome{Status: "worktree_failed", Agent: a.Name, Model: a.Model, DefSHA: a.DefSHA}, fmt.Errorf("worktree: %w", err)
	}
	defer func() {
		removeWorktree(repoDir, wtDir)
		untrackWorktree(wtDir)
	}()
	out := Outcome{Agent: a.Name, Model: a.Model, DefSHA: a.DefSHA, BaseSHA: baseSHA}
	prompt, err := renderManagerPrompt(a, cands)
	if err != nil {
		out.Status = "prompt_failed"
		return out, fmt.Errorf("prompt: %w", err)
	}
	trace := filepath.Join(workspace, "runs", runID+".manager.jsonl")
	stats, err := runPhase(wtDir, a, prompt, trace)
	out.TokIn, out.TokOut = stats.tokensIn, stats.tokensOut
	if err != nil {
		out.Status = "agent_failed"
		return out, fmt.Errorf("agent: %w", err)
	}
	rep, err := gateManagerReport(wtDir, filepath.Join(repoDir, DefaultAgentsDir, a.Name, "report.schema.json"), cands)
	if err != nil {
		out.Status = "gate_failed"
		return out, fmt.Errorf("gate: %w", err)
	}
	if _, err := applyManager(cfg.Flows.Manager, trackerFor(cfg.Repo), items, branches, rep); err != nil {
		out.Status = "tracker_failed"
		return out, fmt.Errorf("tracker: %w", err)
	}
	if err := recordManagerSeen(repoDir, cands); err != nil {
		out.Status = "notes_failed"
		return out, fmt.Errorf("notes: %w", err)
	}
	out.Status = "done"
	return out, nil
}

func renderManagerPrompt(a *Agent, items []Item) (string, error) {
	var b strings.Builder
	for _, it := range items {
		fmt.Fprintf(&b, "### %s %s (updated %s)\n", it.ID, it.Title, it.UpdatedAt)
		if len(it.Tags) > 0 {
			fmt.Fprintf(&b, "tags: %s\n", strings.Join(it.Tags, ", "))
		}
		if body := strings.TrimSpace(it.Body); body != "" {
			fmt.Fprintf(&b, "%s\n", body)
		}
		b.WriteString("\n")
	}
	return renderUserPrompt(a, map[string]any{"Items": strings.TrimSpace(b.String())})
}

// managerReport is the envelope the Manager agent must write: the items it
// promotes and the items it refuses, each with the specific thing it lacks.
// The controller enforces the level cap and applies tags itself; the agent
// only proposes.
type managerReport struct {
	Promote []string        `json:"promote"`
	Reject  []managerReject `json:"reject"`
}

type managerReject struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// gateManagerReport validates the Manager's report against the candidate set:
// every item it names, promote or reject, must be one the lane actually offered
// it. A report naming an unselected item is rejected. The manager's arrays are
// legitimately empty (nothing to promote), so the generic gate's "required
// array is empty" rule does not apply here; the schema file still documents the
// contract and the semantic rule that matters is enforced below.
func gateManagerReport(wtDir, schemaPath string, cands []Item) (managerReport, error) {
	var rep managerReport
	repFile := filepath.Join(wtDir, "report.json")
	raw, err := os.ReadFile(repFile)
	if err != nil {
		return rep, fmt.Errorf("report.json missing: %w", err)
	}
	// Validate the report against its declared schema, tolerating empty required
	// arrays: unlike a build's changed_files, a Manager pass legitimately
	// promotes or rejects nothing. Every other schema violation (a missing key,
	// a mistyped field) is a gate failure.
	if err := checkSchema(repFile, schemaPath); err != nil && !strings.Contains(err.Error(), "is empty") {
		return rep, err
	}
	if err := json.Unmarshal(raw, &rep); err != nil {
		return rep, fmt.Errorf("report.json is invalid JSON: %w", err)
	}
	selected := make(map[string]bool, len(cands))
	for _, it := range cands {
		selected[it.ID] = true
	}
	for _, id := range rep.Promote {
		if id != "" && !selected[id] {
			return rep, fmt.Errorf("report promotes unselected item %q", id)
		}
	}
	for _, r := range rep.Reject {
		if r.ID != "" && !selected[r.ID] {
			return rep, fmt.Errorf("report rejects unselected item %q", r.ID)
		}
	}
	return rep, nil
}

// applyManager enforces the level cap and applies the report's decisions to
// the tracker: promote tagged items are added with SetTags, and refused items
// receive at most one comment naming what they lack. Every effect is a tag or a
// comment; the lane never writes code or moves branches.
func applyManager(cfg ManagerFlowCfg, tk Tracker, items []Item, branches []string, rep managerReport) (int, error) {
	open := make(map[string]Item, len(items))
	for _, it := range items {
		open[it.ID] = it
	}
	covered := make(map[string]bool, len(branches))
	for _, branch := range branches {
		covered[itemIDFromBranch(branch)] = true
	}
	// The open level counts items already promoted without a branch, so a fast
	// interval cannot stack promotions ahead of the Builder.
	inFlight := 0
	for _, it := range items {
		if it.hasTag(cfg.PromoteTag) && !covered[it.ID] {
			inFlight++
		}
	}
	promoted := 0
	for _, id := range rep.Promote {
		if id == "" {
			continue
		}
		it, ok := open[id]
		if !ok {
			return promoted, fmt.Errorf("promote names unknown item %q", id)
		}
		if hasExcludedTag(it, cfg.ExcludeTags) {
			continue
		}
		if len(openBlockers(it, open)) > 0 {
			continue
		}
		if it.hasTag(cfg.PromoteTag) {
			continue
		}
		if inFlight >= cfg.MaxOpenReady {
			break
		}
		if err := tk.SetTags(id, []string{cfg.PromoteTag}, nil); err != nil {
			return promoted, err
		}
		inFlight++
		promoted++
	}
	for _, r := range rep.Reject {
		if r.ID == "" {
			continue
		}
		it, ok := open[r.ID]
		if !ok {
			return promoted, fmt.Errorf("reject names unknown item %q", r.ID)
		}
		if managerAlreadyCommented(it) {
			continue
		}
		reason := strings.TrimSpace(r.Reason)
		if blockers := openBlockers(it, open); len(blockers) > 0 {
			reason = fmt.Sprintf("blocked by %s, which is open", formatBlockers(blockers))
		}
		if reason == "" {
			reason = "not promoted"
		}
		if err := tk.Comment(it.ID, managerMarker+" not promoted: "+reason); err != nil {
			return promoted, err
		}
	}
	return promoted, nil
}

// managerAlreadyCommented reports whether the lane has already told this item
// why it was not promoted, so repeated passes at an unchanged revision add no
// further comment even without the durable judgement record.
func managerAlreadyCommented(it Item) bool {
	for _, c := range it.Comments {
		if strings.Contains(c.Body, managerMarker) {
			return true
		}
	}
	return false
}

// openBlockers returns the `Blocked by:` references, drawn from an item's body,
// that name items still open on the tracker. A promoted item's blockers must be
// closed: ordering emerges from declared dependencies plus judgement, not from a
// priority field.
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

func formatBlockers(ids []string) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = "#" + id
	}
	return strings.Join(parts, ", ")
}

// createManagerWorktree makes a throwaway, detached worktree at the remote tip
// for the Manager to run in. Unlike the build lanes the Manager owns no branch:
// the worktree is detached at origin/master, so no local ref is created or
// deleted and nothing is ever pushed from this lane.
func createManagerWorktree(repo, workspace string) (wtDir, baseSHA string, err error) {
	wtDir = filepath.Join(workspace, "worktrees", "manager")
	trackWorktree(wtDir)
	defer func() {
		if err != nil {
			untrackWorktree(wtDir)
		}
	}()
	_ = os.RemoveAll(wtDir)
	_ = git(repo, "worktree", "prune")
	if err := git(repo, "fetch", "origin", "master"); err != nil {
		return "", "", fmt.Errorf("fetch origin/master: %w", err)
	}
	baseSHA, err = gitOut(repo, "rev-parse", "origin/master")
	if err != nil {
		return "", "", fmt.Errorf("origin/master: %w", err)
	}
	if err := git(repo, "worktree", "add", "--detach", wtDir, baseSHA); err != nil {
		return "", "", fmt.Errorf("worktree add: %w", err)
	}
	return wtDir, baseSHA, nil
}

// managerCandidates returns the open items that need a fresh judgement now:
// every non-excluded item whose update stamp has moved since the lane last
// judged it. Already-judged items stay quiet until their revision moves.
func managerCandidates(cfg ManagerFlowCfg, repoDir string, items []Item) ([]Item, error) {
	seen, err := readManagerSeen(repoDir)
	if err != nil {
		return nil, err
	}
	var out []Item
	for _, it := range items {
		if hasExcludedTag(it, cfg.ExcludeTags) {
			continue
		}
		if seen[it.ID] == it.UpdatedAt {
			continue
		}
		out = append(out, it)
	}
	return out, nil
}

func readManagerSeen(repoDir string) (map[string]string, error) {
	_, body, err := getBlobRef(repoDir, managerSeenRef)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]string)
	if strings.TrimSpace(body) == "" {
		return seen, nil
	}
	if err := json.Unmarshal([]byte(body), &seen); err != nil {
		return nil, fmt.Errorf("decode manager seen: %w", err)
	}
	return seen, nil
}

// recordManagerSeen writes the judged revisions for a completed pass, so the
// same static backlog is not reread on the next interval.
func recordManagerSeen(repoDir string, cands []Item) error {
	seen, err := readManagerSeen(repoDir)
	if err != nil {
		return err
	}
	for _, it := range cands {
		seen[it.ID] = it.UpdatedAt
	}
	payload, err := json.Marshal(seen)
	if err != nil {
		return fmt.Errorf("encode manager seen: %w", err)
	}
	sha, _, err := getBlobRef(repoDir, managerSeenRef)
	if err != nil {
		return err
	}
	if err := putBlobRef(repoDir, managerSeenRef, string(payload), sha); err != nil {
		return fmt.Errorf("record manager seen: %w", err)
	}
	return nil
}
