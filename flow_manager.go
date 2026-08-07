package main

import (
	"bytes"
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
	branches, err := forestBranches(repoDir)
	if err != nil {
		return nil, fmt.Errorf("branches: %w", err)
	}
	cands, err := managerCandidates(cfg.Flows.Manager, repoDir, items, branches)
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
	cands, err := managerCandidates(cfg.Flows.Manager, repoDir, items, branches)
	if err != nil {
		return Outcome{}, fmt.Errorf("candidates: %w", err)
	}
	a, err := loadAgent(repoDir, cfg.Flows.Manager.Agent)
	if err != nil {
		return Outcome{Status: "agent_failed"}, fmt.Errorf("agent: %w", err)
	}

	workspace := workspaceDir(repoDir)
	runDir, cleanup, err := createManagerRunDir(workspace)
	if err != nil {
		return Outcome{Status: "worktree_failed", Agent: a.Name, Model: a.Model, DefSHA: a.DefSHA}, fmt.Errorf("run dir: %w", err)
	}
	defer cleanup()
	out := Outcome{Agent: a.Name, Model: a.Model, DefSHA: a.DefSHA}
	prompt, err := renderManagerPrompt(a, cands)
	if err != nil {
		out.Status = "prompt_failed"
		return out, fmt.Errorf("prompt: %w", err)
	}
	trace := filepath.Join(workspace, "runs", runID+".manager.jsonl")
	stats, err := runPhase(runDir, a, prompt, trace)
	out.TokIn, out.TokOut = stats.tokensIn, stats.tokensOut
	if err != nil {
		out.Status = "agent_failed"
		return out, fmt.Errorf("agent: %w", err)
	}
	rep, err := gateManagerReport(runDir, cands)
	if err != nil {
		out.Status = "gate_failed"
		return out, fmt.Errorf("gate: %w", err)
	}
	if err := gateManagerRunDir(runDir); err != nil {
		out.Status = "gate_failed"
		return out, fmt.Errorf("gate: %w", err)
	}
	var judged map[string]bool
	_, judged, err = applyManager(cfg.Flows.Manager, trackerFor(cfg.Repo), items, branches, rep)
	if err != nil {
		out.Status = "tracker_failed"
		return out, fmt.Errorf("tracker: %w", err)
	}
	if err := recordManagerSeen(repoDir, trackerFor(cfg.Repo), cands, judged); err != nil {
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

// gateManagerReport validates the Manager's report against the candidate set
// and requires the agent to actually judge every item it was offered: each
// candidate must be decided exactly once, as either a promote or a reject entry,
// and duplicate or conflicting (promote and reject for the same id) entries are
// rejected. This closes the hole where `{"promote":[],"reject":[]}` — or any
// partial set — passed as a successful judgement, recorded no seen revisions,
// and made the daemon immediately rerun the lane instead of resolving (and
// commenting on) the backlog. A report naming an item the lane did not offer, a
// null field, or a reject entry missing its id or reason is also rejected. The
// controller applies only what survives this gate, never the agent directly.
func gateManagerReport(wtDir string, cands []Item) (managerReport, error) {
	var rep managerReport
	repFile := filepath.Join(wtDir, "report.json")
	raw, err := os.ReadFile(repFile)
	if err != nil {
		return rep, fmt.Errorf("report.json missing: %w", err)
	}
	if err := json.Unmarshal(raw, &rep); err != nil {
		return rep, fmt.Errorf("report.json is invalid JSON: %w", err)
	}
	// Inspect the raw fields so a null cannot hide as an empty judgement: Go
	// decodes `"promote": null` into a nil slice without error, which would let
	// a malformed report masquerade as "nothing to promote".
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return rep, fmt.Errorf("report.json is invalid JSON: %w", err)
	}
	for _, name := range []string{"promote", "reject"} {
		rawField, ok := fields[name]
		if !ok {
			return rep, fmt.Errorf("report.json missing required field %q", name)
		}
		if len(bytes.TrimSpace(rawField)) == 0 || string(bytes.TrimSpace(rawField)) == "null" {
			return rep, fmt.Errorf("report.json field %q must be an array, got null", name)
		}
	}
	selected := make(map[string]bool, len(cands))
	for _, it := range cands {
		selected[it.ID] = true
	}
	// decided tracks whether a candidate has already been resolved and how, so a
	// duplicate promote, a duplicate reject, and a promote+reject conflict on the
	// same id all fail, and an undecided candidate is caught at the end.
	decided := make(map[string]string, len(cands))
	seenPromote := make(map[string]bool)
	for _, id := range rep.Promote {
		if strings.TrimSpace(id) == "" {
			return rep, fmt.Errorf("report promote entry is empty")
		}
		if !selected[id] {
			return rep, fmt.Errorf("report promotes unselected item %q", id)
		}
		if seenPromote[id] {
			return rep, fmt.Errorf("report promotes item %q more than once", id)
		}
		if kind := decided[id]; kind != "" {
			return rep, fmt.Errorf("report promotes item %q already decided as %q", id, kind)
		}
		seenPromote[id] = true
		decided[id] = "promote"
	}
	seenReject := make(map[string]bool)
	for i, r := range rep.Reject {
		if strings.TrimSpace(r.ID) == "" {
			return rep, fmt.Errorf("report reject[%d] missing required field %q", i, "id")
		}
		if strings.TrimSpace(r.Reason) == "" {
			return rep, fmt.Errorf("report reject[%d] missing required field %q", i, "reason")
		}
		if !selected[r.ID] {
			return rep, fmt.Errorf("report rejects unselected item %q", r.ID)
		}
		if seenReject[r.ID] {
			return rep, fmt.Errorf("report rejects item %q more than once", r.ID)
		}
		if kind := decided[r.ID]; kind != "" {
			return rep, fmt.Errorf("report rejects item %q already decided as %q", r.ID, kind)
		}
		seenReject[r.ID] = true
		decided[r.ID] = "reject"
	}
	// Completeness: every candidate the lane offered must be decided exactly
	// once. An agent that cannot judge must fail the pass, not succeed silently.
	var missing []string
	for _, it := range cands {
		if _, ok := decided[it.ID]; !ok {
			missing = append(missing, it.ID)
		}
	}
	if len(missing) > 0 {
		return rep, fmt.Errorf("report leaves %d offered item(s) undecided: %s", len(missing), strings.Join(missing, ", "))
	}
	return rep, nil
}

// gateManagerRunDir enforces the lane's authority bound: the Manager writes
// exactly one file, its report. The run directory is not a git checkout, so
// there is no HEAD to move, no tracked file to modify, and no ref to push. The
// bound holds because the lane has no repository, not because a check watches
// one.
//
// An earlier version ran the agent in a detached worktree and proved innocence
// by snapshotting every local and remote ref before and after. That could not
// work: Builder, Verifier and Fixer run concurrently in this process, so a
// sibling lane's push or note during a multi-minute Manager run moved refs the
// Manager never touched and failed its gate. Removing the repository removes
// the question.
func gateManagerRunDir(runDir string) error {
	entries, err := os.ReadDir(runDir)
	if err != nil {
		return fmt.Errorf("cannot read manager run directory: %w", err)
	}
	for _, e := range entries {
		name := e.Name()
		// The harness renders the agent declaration here before the run.
		if name == ".opencode" || isRunArtifact(name) {
			continue
		}
		return fmt.Errorf("manager wrote unexpected file %q", name)
	}
	return nil
}

// applyManager enforces the level cap and applies the report's decisions to
// the tracker: promote tagged items are added with SetTags, and refused items
// receive at most one comment naming what they lack. Every effect is a tag or a
// comment; the lane never writes code or moves branches.
//
// judged names the candidates the pass actually resolved, so the caller records
// judgement only for those. An item deferred by the level cap, or one refused
// only because an open blocker sits on it, is not resolved: it must stay
// eligible for a later pass, because freeing the level or closing the blocker
// changes nothing about the item's own update stamp. Returning them in judged
// would mark them seen and never reconsider them.
func applyManager(cfg ManagerFlowCfg, tk Tracker, items []Item, branches []string, rep managerReport) (int, map[string]bool, error) {
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
	// judged tracks the candidates this pass actually resolved. Only those are
	// cached as seen; cap-deferred and open-blocked items are left untagged so a
	// later pass can reconsider them when capacity frees or the blocker closes.
	judged := make(map[string]bool)
	// commented tracks the ids this pass has already told why it will not
	// promote. The local tracker copy is stale on comments written during this
	// same call, so the marker scan alone would not stop an item named in both
	// promote (blocked) and reject from receiving two explanations.
	commented := make(map[string]bool)
	commentIfNeeded := func(it Item, reason string) error {
		if commented[it.ID] || managerAlreadyCommented(it) {
			return nil
		}
		if err := tk.Comment(it.ID, managerMarker+" not promoted: "+reason); err != nil {
			return err
		}
		commented[it.ID] = true
		return nil
	}
	for _, id := range rep.Promote {
		if id == "" {
			continue
		}
		it, ok := open[id]
		if !ok {
			return promoted, judged, fmt.Errorf("promote names unknown item %q", id)
		}
		if hasExcludedTag(it, cfg.ExcludeTags) {
			continue
		}
		if blockers := openBlockers(it, open); len(blockers) > 0 {
			// A promote entry for a blocked item is not silently dropped: the
			// reason must be recorded (and idempotently, so a second pass at the
			// same revision adds no further comment). The item stays unjudged
			// for promotion but the lane tells it what it lacks.
			reason := "blocked by " + formatBlockers(blockers) + ", which is open"
			if err := commentIfNeeded(it, reason); err != nil {
				return promoted, judged, err
			}
			continue
		}
		if it.hasTag(cfg.PromoteTag) {
			judged[id] = true
			continue
		}
		if inFlight >= cfg.MaxOpenReady {
			break
		}
		if err := tk.SetTags(id, []string{cfg.PromoteTag}, nil); err != nil {
			return promoted, judged, err
		}
		inFlight++
		promoted++
		judged[id] = true
	}
	for _, r := range rep.Reject {
		if r.ID == "" {
			continue
		}
		it, ok := open[r.ID]
		if !ok {
			return promoted, judged, fmt.Errorf("reject names unknown item %q", r.ID)
		}
		reason := strings.TrimSpace(r.Reason)
		if blockers := openBlockers(it, open); len(blockers) > 0 {
			// A reject caused solely by an open blocker is not a final verdict:
			// closing the blocker changes nothing about this item's own update
			// stamp, so it must stay eligible for a later pass. The reason is
			// still recorded, but the item is not marked judged.
			reason = fmt.Sprintf("blocked by %s, which is open", formatBlockers(blockers))
			if err := commentIfNeeded(it, reason); err != nil {
				return promoted, judged, err
			}
			continue
		}
		if reason == "" {
			reason = "not promoted"
		}
		if err := commentIfNeeded(it, reason); err != nil {
			return promoted, judged, err
		}
		judged[r.ID] = true
	}
	return promoted, judged, nil
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

// createManagerRunDir makes a throwaway empty directory for the Manager to run
// in. The lane reads its items from the Tracker and writes one report, so it
// needs no checkout: giving it a git worktree would hand a judgement-only agent
// a repository it has no business touching, and would make the lane's authority
// a thing to police instead of a thing it lacks.
func createManagerRunDir(workspace string) (string, func(), error) {
	base := filepath.Join(workspace, "runs")
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", nil, err
	}
	dir, err := os.MkdirTemp(base, "manager-")
	if err != nil {
		return "", nil, err
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

// managerCandidates returns the open items that need a fresh judgement now:
// every non-excluded item whose update stamp has moved since the lane last
// judged it. Already-judged items stay quiet until their revision moves.
//
// An item already promoted, or already covered by a branch, is settled: the
// queue decision for it is made and the lanes downstream own it. Offering it
// again invites the agent to reject work that is already moving, which would
// put a "not promoted" comment on a ready item.
func managerCandidates(cfg ManagerFlowCfg, repoDir string, items []Item, branches []string) ([]Item, error) {
	seen, err := readManagerSeen(repoDir)
	if err != nil {
		return nil, err
	}
	covered := make(map[string]bool, len(branches))
	for _, branch := range branches {
		covered[itemIDFromBranch(branch)] = true
	}
	var out []Item
	for _, it := range items {
		if hasExcludedTag(it, cfg.ExcludeTags) {
			continue
		}
		if it.hasTag(cfg.PromoteTag) || covered[it.ID] {
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
// same static backlog is not reread on the next interval. Only the candidates
// the pass actually resolved are recorded; an item deferred by the level cap or
// refused only because an open blocker exists stays untracked, so it is revisited
// when capacity frees or the blocker closes.
//
// The recorded revision is the item's post-effect update stamp, re-read from the
// tracker: GitHub bumps updatedAt on the label and comment mutations the pass
// just made, so storing the pre-effect stamp (as the pass saw it) would make the
// next selection rejudge the Manager's own changes instead of suppressing a
// static item. A re-read failure falls back to the stamp the pass saw rather
// than failing the whole run.
func recordManagerSeen(repoDir string, tk Tracker, cands []Item, judged map[string]bool) error {
	seen, err := readManagerSeen(repoDir)
	if err != nil {
		return err
	}
	for _, it := range cands {
		if !judged[it.ID] {
			continue
		}
		revision := it.UpdatedAt
		if fresh, gerr := tk.Get(it.ID); gerr == nil && fresh.UpdatedAt != "" {
			revision = fresh.UpdatedAt
		}
		seen[it.ID] = revision
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
