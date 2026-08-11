package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

type toolFunc func(context.Context, string, ...string) ([]byte, error)

type Poller struct {
	Root         string
	Repo         string
	Run          toolFunc
	ResolveTools bool
}

func NewPoller(root, repo string) *Poller {
	return &Poller{Root: root, Repo: repo, Run: realTool, ResolveTools: true}
}

func realTool(ctx context.Context, name string, args ...string) ([]byte, error) {
	return processGroupOutput(ctx, exec.Command(name, args...))
}

func (p *Poller) git(ctx context.Context, args ...string) ([]byte, error) {
	return p.run(ctx, "git", append([]string{"-C", p.Root}, args...)...)
}

func (p *Poller) gh(ctx context.Context, args ...string) ([]byte, error) {
	return p.run(ctx, "gh", args...)
}

func (p *Poller) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if p.ResolveTools {
		path, err := trustedExecutable(p.Root, name)
		if err != nil {
			return nil, err
		}
		name = path
	}
	return p.Run(ctx, name, args...)
}

type trackerIssue struct {
	Number      *int            `json:"number"`
	PullRequest json.RawMessage `json:"pull_request"`
}

func (p *Poller) readyIssues(ctx context.Context) ([]int, error) {
	if strings.TrimSpace(p.Repo) == "" {
		return nil, fmt.Errorf("repository is required")
	}
	endpoint := fmt.Sprintf("repos/%s/issues?state=open&labels=forest%%3Aready&per_page=100", p.Repo)
	output, err := p.gh(ctx, "api", "--paginate", "--slurp", endpoint)
	if err != nil {
		return nil, err
	}
	var pages [][]trackerIssue
	if err := json.Unmarshal(output, &pages); err != nil {
		return nil, fmt.Errorf("malformed issue output: %w", err)
	}
	if pages == nil {
		return nil, fmt.Errorf("malformed issue output")
	}
	issues := make([]int, 0)
	for _, pageIssues := range pages {
		for _, issue := range pageIssues {
			if issue.Number == nil || *issue.Number <= 0 {
				return nil, fmt.Errorf("malformed issue number")
			}
			pullRequest := strings.TrimSpace(string(issue.PullRequest))
			if pullRequest != "" && pullRequest != "null" {
				var object map[string]json.RawMessage
				if !strings.HasPrefix(pullRequest, "{") || json.Unmarshal(issue.PullRequest, &object) != nil || object == nil {
					return nil, fmt.Errorf("malformed pull request field")
				}
				continue
			}
			issues = append(issues, *issue.Number)
		}
	}
	sort.Ints(issues)
	return issues, nil
}

func (p *Poller) builder(ctx context.Context) int {
	issues, err := p.readyIssues(ctx)
	if err != nil {
		return 2
	}
	for _, issue := range issues {
		branches, err := p.git(ctx, "ls-remote", "--heads", "origin", fmt.Sprintf("refs/heads/forest/%d-*", issue))
		if err != nil {
			return 2
		}
		matches, err := parseBranchOutput(branches, issue)
		if err != nil {
			return 2
		}
		if len(matches) == 0 {
			return 0
		}
	}
	return 1
}

func parseBranchOutput(output []byte, issue int) ([]branchTip, error) {
	text := strings.TrimSpace(string(output))
	if text == "" {
		return nil, nil
	}
	branches := make([]branchTip, 0)
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || !isSHA(fields[0]) {
			return nil, fmt.Errorf("malformed branch ls-remote output")
		}
		ref := fields[1]
		if !strings.HasPrefix(ref, "refs/heads/") {
			return nil, fmt.Errorf("malformed branch ref")
		}
		branch := strings.TrimPrefix(ref, "refs/heads/")
		branchIssue := issue
		if branchIssue <= 0 {
			parts := strings.SplitN(strings.TrimPrefix(branch, "forest/"), "-", 2)
			parsedIssue, err := strconv.Atoi(parts[0])
			if err != nil || parsedIssue <= 0 {
				return nil, fmt.Errorf("malformed branch ref")
			}
			branchIssue = parsedIssue
		}
		if !validBranch(branch, branchIssue) {
			if issue > 0 {
				return nil, fmt.Errorf("branch does not match issue %d", issue)
			}
			return nil, fmt.Errorf("malformed branch ref")
		}
		branches = append(branches, branchTip{Name: branch, SHA: fields[0]})
	}
	return branches, nil
}

func (p *Poller) verifier(ctx context.Context) (code int) {
	branches, err := p.branchTips(ctx)
	if err != nil {
		return 2
	}
	if len(branches) == 0 {
		return 1
	}
	notes, err := p.fetchNotes(ctx)
	if err != nil {
		return 2
	}
	defer func() {
		if err := p.deleteNotes(notes); err != nil {
			code = 2
		}
	}()
	for _, tip := range branches {
		review, reviewErr := p.coordinationNote(ctx, notes.ReviewRequest.Ref, tip.SHA, "builder", "fixer")
		if reviewErr != nil {
			if errors.Is(reviewErr, pollEnumerationSkip) {
				return 1
			}
			if !isMissingNote(reviewErr) {
				return 2
			}
			continue
		}
		if err := validatePollReviewRequestBranch(review, tip.SHA, tip.Name); err != nil {
			return 2
		}
		verdict, verdictErr := p.coordinationNote(ctx, notes.Verdict.Ref, tip.SHA, "verifier")
		if verdictErr == nil {
			if _, err := decodeVerdict(verdict, tip.SHA); err != nil {
				return 2
			}
			continue
		}
		if errors.Is(verdictErr, pollEnumerationSkip) {
			return 1
		}
		if !isMissingNote(verdictErr) {
			return 2
		}
		if err := p.confirmSnapshot(ctx, tip, notes); err != nil {
			return 2
		}
		return 0
	}
	return 1
}

func (p *Poller) fixer(ctx context.Context) (code int) {
	branches, err := p.branchTips(ctx)
	if err != nil {
		return 2
	}
	if len(branches) == 0 {
		return 1
	}
	notes, err := p.fetchNotes(ctx)
	if err != nil {
		return 2
	}
	defer func() {
		if err := p.deleteNotes(notes); err != nil {
			code = 2
		}
	}()
	for _, tip := range branches {
		verdict, verdictErr := p.coordinationNote(ctx, notes.Verdict.Ref, tip.SHA, "verifier")
		if verdictErr != nil {
			if errors.Is(verdictErr, pollEnumerationSkip) {
				return 1
			}
			if !isMissingNote(verdictErr) {
				return 2
			}
			continue
		}
		parsed, err := decodeVerdict(verdict, tip.SHA)
		if err != nil {
			return 2
		}
		if parsed.Verdict == "changes" {
			review, reviewErr := p.coordinationNote(ctx, notes.ReviewRequest.Ref, tip.SHA, "builder", "fixer")
			if reviewErr != nil {
				if errors.Is(reviewErr, pollEnumerationSkip) {
					return 1
				}
				return 2
			}
			if err := validatePollReviewRequestBranch(review, tip.SHA, tip.Name); err != nil {
				return 2
			}
			if err := p.confirmSnapshot(ctx, tip, notes); err != nil {
				return 2
			}
			return 0
		}
	}
	return 1
}

type branchTip struct {
	Name string
	SHA  string
}

func (p *Poller) branchTips(ctx context.Context) ([]branchTip, error) {
	output, err := p.git(ctx, "ls-remote", "--heads", "origin", "refs/heads/forest/*")
	if err != nil {
		return nil, err
	}
	return parseBranchOutput(output, 0)
}

const pollNotesNamespace = "refs/notes/forest-poll"

type pollNoteState struct {
	Ref string
	OID string
}

type pollNoteSnapshot struct {
	ReviewRequest pollNoteState
	Checks        pollNoteState
	Verdict       pollNoteState
}

func (snapshot *pollNoteSnapshot) state(ref string) *pollNoteState {
	switch ref {
	case reviewRequestNoteRef:
		return &snapshot.ReviewRequest
	case checksNoteRef:
		return &snapshot.Checks
	case verdictNoteRef:
		return &snapshot.Verdict
	default:
		return nil
	}
}

func newPollNoteSnapshot() (pollNoteSnapshot, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return pollNoteSnapshot{}, err
	}
	base := fmt.Sprintf("%s/%x", pollNotesNamespace, id[:])
	return pollNoteSnapshot{
		ReviewRequest: pollNoteState{Ref: base + "/" + strings.TrimPrefix(reviewRequestNoteRef, "refs/notes/forest/")},
		Checks:        pollNoteState{Ref: base + "/" + strings.TrimPrefix(checksNoteRef, "refs/notes/forest/")},
		Verdict:       pollNoteState{Ref: base + "/" + strings.TrimPrefix(verdictNoteRef, "refs/notes/forest/")},
	}, nil
}

func (p *Poller) fetchNotes(ctx context.Context) (snapshot pollNoteSnapshot, err error) {
	snapshot, err = newPollNoteSnapshot()
	if err != nil {
		return snapshot, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, p.deleteNotes(snapshot))
		}
	}()

	refs := coordinationNoteRefs()
	args := make([]string, 0, 2+len(refs))
	args = append(args, "ls-remote", "origin")
	args = append(args, refs...)
	output, err := p.git(ctx, args...)
	if err != nil {
		return snapshot, err
	}
	text := strings.TrimSpace(string(output))
	if text != "" {
		for _, line := range strings.Split(text, "\n") {
			fields := strings.Fields(line)
			if len(fields) != 2 || !isSHA(fields[0]) {
				return snapshot, fmt.Errorf("malformed notes ls-remote output")
			}
			state := snapshot.state(fields[1])
			if state == nil {
				return snapshot, fmt.Errorf("malformed notes ls-remote output")
			}
			if state.OID != "" {
				return snapshot, fmt.Errorf("duplicate notes ls-remote output")
			}
			state.OID = fields[0]
		}
	}
	for _, canonical := range refs {
		state := snapshot.state(canonical)
		if state.OID == "" {
			continue
		}
		if _, err = p.git(ctx, "fetch", "origin", canonical+":"+state.Ref); err != nil {
			return snapshot, err
		}
		var fetched []byte
		fetched, err = p.git(ctx, "rev-parse", "--verify", state.Ref)
		if err != nil {
			return snapshot, err
		}
		oid := strings.TrimSpace(string(fetched))
		if !isSHA(oid) || oid != state.OID {
			return snapshot, fmt.Errorf("notes ref moved while fetching %s", canonical)
		}
	}
	return snapshot, nil
}

func (p *Poller) deleteNotes(snapshot pollNoteSnapshot) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var cleanup error
	for _, canonical := range coordinationNoteRefs() {
		ref := snapshot.state(canonical).Ref
		if ref == "" {
			continue
		}
		if _, err := p.git(ctx, "update-ref", "-d", ref); err != nil {
			cleanup = errors.Join(cleanup, err)
		}
	}
	return cleanup
}

func (p *Poller) confirmSnapshot(ctx context.Context, tip branchTip, notes pollNoteSnapshot) error {
	branchRef := "refs/heads/" + tip.Name
	refs := coordinationNoteRefs()
	args := make([]string, 0, 3+len(refs))
	args = append(args, "ls-remote", "origin", branchRef)
	args = append(args, refs...)
	output, err := p.git(ctx, args...)
	if err != nil {
		return err
	}

	expected := make(map[string]string, 1+len(refs))
	expected[branchRef] = tip.SHA
	for _, ref := range refs {
		if oid := notes.state(ref).OID; oid != "" {
			expected[ref] = oid
		}
	}
	actual := make(map[string]string, len(expected))
	text := strings.TrimSpace(string(output))
	if text != "" {
		for _, line := range strings.Split(text, "\n") {
			fields := strings.Fields(line)
			if len(fields) != 2 || !isSHA(fields[0]) || fields[1] != branchRef && notes.state(fields[1]) == nil {
				return fmt.Errorf("malformed snapshot ls-remote output")
			}
			if _, exists := actual[fields[1]]; exists {
				return fmt.Errorf("duplicate snapshot ls-remote output")
			}
			actual[fields[1]] = fields[0]
		}
	}
	if len(actual) != len(expected) {
		return fmt.Errorf("remote snapshot moved for %s", tip.Name)
	}
	for ref, oid := range expected {
		if actual[ref] != oid {
			return fmt.Errorf("remote snapshot moved for %s", tip.Name)
		}
	}
	return nil
}

var pollMissingNote = errors.New("missing coordination note")

// pollEnumerationSkip reports that a coordination-note tree scan exceeded the
// bounded capacity. A Poll treats the tick as no work (exit 1) and never as an
// operational error; the Auditor is the only integrity reporter.
var pollEnumerationSkip = errors.New("coordination note enumeration exceeds capacity")

func (p *Poller) note(ctx context.Context, ref, sha string) ([]byte, error) {
	payload, err := p.git(ctx, "notes", "--ref="+ref, "show", sha)
	if err == nil {
		return payload, nil
	}
	if soleExitCode(err, 1) {
		return nil, pollMissingNote
	}
	return nil, err
}

func (p *Poller) coordinationNote(ctx context.Context, ref, sha string, roles ...string) ([]byte, error) {
	payload, err := p.note(ctx, ref, sha)
	if err != nil {
		return nil, err
	}
	treeOutput, err := p.git(ctx, "ls-tree", "-r", "--name-only", ref)
	if err != nil {
		if errors.Is(err, errTrustedTransportOutputOverflow) {
			fmt.Fprintf(os.Stderr, "poll: canonical note tree %s enumeration transport output overflow; treating tick as no work\n", pollCanonicalKey(ref))
			return nil, pollEnumerationSkip
		}
		return nil, err
	}
	path, err := pollNotePath(treeOutput, ref, sha)
	if err != nil {
		if errors.Is(err, pollEnumerationSkip) {
			fmt.Fprintf(os.Stderr, "poll: canonical note tree %s exceeds the %d-entry enumeration bound; treating tick as no work\n", pollCanonicalKey(ref), auditorCapacityEntries)
		}
		return nil, err
	}
	identityOutput, err := p.git(ctx, "log", "-1", "--format=%an%x00%ae", ref, "--", path)
	if err != nil {
		return nil, err
	}
	identityLine, err := exactGitLine(identityOutput)
	if err != nil {
		return nil, fmt.Errorf("malformed note identity on %s for %s: %w", ref, sha, err)
	}
	identity := strings.SplitN(identityLine, "\x00", 2)
	if len(identity) != 2 || !validIdentity(noteEntry{Author: identity[0], Email: identity[1]}, roles...) {
		return nil, fmt.Errorf("wrong note identity on %s for %s", ref, sha)
	}
	return payload, nil
}

func isMissingNote(err error) bool {
	return errors.Is(err, pollMissingNote)
}

// pollNotePath finds the note tree path that binds sha. It parses at most
// auditorCapacityEntries rows; a larger tree returns pollEnumerationSkip
// before scanning the remainder. A row inside the bound whose flattened name
// is not a valid revision is malformed and stays a hard error.
func pollNotePath(output []byte, ref, sha string) (string, error) {
	path := ""
	rows := 0
	rest := strings.TrimSpace(string(output))
	for rest != "" {
		rows++
		if rows > auditorCapacityEntries {
			return "", pollEnumerationSkip
		}
		candidate, remainder, _ := strings.Cut(rest, "\n")
		rest = remainder
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if !isSHA(strings.ReplaceAll(candidate, "/", "")) {
			return "", fmt.Errorf("malformed note tree row on %s", ref)
		}
		if strings.ReplaceAll(candidate, "/", "") != sha {
			continue
		}
		if path != "" {
			return "", fmt.Errorf("duplicate note path on %s for %s", ref, sha)
		}
		path = candidate
	}
	if path == "" {
		return "", fmt.Errorf("note path on %s does not bind %s", ref, sha)
	}
	return path, nil
}

// pollCanonicalKey maps a private Poll snapshot ref to its canonical
// coordination-note ref for operator-facing log lines.
func pollCanonicalKey(ref string) string {
	for _, canonical := range coordinationNoteRefs() {
		if strings.HasSuffix(ref, "/"+strings.TrimPrefix(canonical, "refs/notes/forest/")) {
			return canonical
		}
	}
	return ref
}
