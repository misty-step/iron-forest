package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
		review, reviewErr := p.coordinationNote(ctx, notes.refs[0], tip.SHA, "builder", "fixer")
		if reviewErr != nil {
			if !isMissingNote(reviewErr) {
				return 2
			}
			continue
		}
		if err := validatePollReviewRequestBranch(review, tip.SHA, tip.Name); err != nil {
			return 2
		}
		verdict, verdictErr := p.coordinationNote(ctx, notes.refs[2], tip.SHA, "verifier")
		if verdictErr == nil {
			if _, err := decodeVerdict(verdict, tip.SHA); err != nil {
				return 2
			}
			continue
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
		verdict, verdictErr := p.coordinationNote(ctx, notes.refs[2], tip.SHA, "verifier")
		if verdictErr != nil {
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
			review, reviewErr := p.coordinationNote(ctx, notes.refs[0], tip.SHA, "builder", "fixer")
			if reviewErr != nil || validatePollReviewRequestBranch(review, tip.SHA, tip.Name) != nil {
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

var forestNoteRefs = [...]string{
	"refs/notes/forest/review-request",
	"refs/notes/forest/checks",
	"refs/notes/forest/verdict",
}

const pollNotesNamespace = "refs/notes/forest-poll"

type pollNoteSnapshot struct {
	refs [len(forestNoteRefs)]string
	oids [len(forestNoteRefs)]string
}

func forestNoteRefIndex(ref string) int {
	for index, canonical := range forestNoteRefs {
		if ref == canonical {
			return index
		}
	}
	return -1
}

func newPollNoteSnapshot() (pollNoteSnapshot, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return pollNoteSnapshot{}, err
	}
	base := fmt.Sprintf("%s/%x", pollNotesNamespace, id[:])
	var snapshot pollNoteSnapshot
	for index, canonical := range forestNoteRefs {
		snapshot.refs[index] = base + "/" + strings.TrimPrefix(canonical, "refs/notes/forest/")
	}
	return snapshot, nil
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

	args := make([]string, 0, 2+len(forestNoteRefs))
	args = append(args, "ls-remote", "origin")
	args = append(args, forestNoteRefs[:]...)
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
			index := forestNoteRefIndex(fields[1])
			if index < 0 {
				return snapshot, fmt.Errorf("malformed notes ls-remote output")
			}
			if snapshot.oids[index] != "" {
				return snapshot, fmt.Errorf("duplicate notes ls-remote output")
			}
			snapshot.oids[index] = fields[0]
		}
	}
	for index, canonical := range forestNoteRefs {
		if snapshot.oids[index] == "" {
			continue
		}
		if _, err = p.git(ctx, "fetch", "origin", canonical+":"+snapshot.refs[index]); err != nil {
			return snapshot, err
		}
		var fetched []byte
		fetched, err = p.git(ctx, "rev-parse", "--verify", snapshot.refs[index])
		if err != nil {
			return snapshot, err
		}
		oid := strings.TrimSpace(string(fetched))
		if !isSHA(oid) || oid != snapshot.oids[index] {
			return snapshot, fmt.Errorf("notes ref moved while fetching %s", canonical)
		}
	}
	return snapshot, nil
}

func (p *Poller) deleteNotes(snapshot pollNoteSnapshot) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var cleanup error
	for _, ref := range snapshot.refs {
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
	args := make([]string, 0, 3+len(forestNoteRefs))
	args = append(args, "ls-remote", "origin", branchRef)
	args = append(args, forestNoteRefs[:]...)
	output, err := p.git(ctx, args...)
	if err != nil {
		return err
	}

	expected := make(map[string]string, 1+len(forestNoteRefs))
	expected[branchRef] = tip.SHA
	for index, ref := range forestNoteRefs {
		if notes.oids[index] != "" {
			expected[ref] = notes.oids[index]
		}
	}
	actual := make(map[string]string, len(expected))
	text := strings.TrimSpace(string(output))
	if text != "" {
		for _, line := range strings.Split(text, "\n") {
			fields := strings.Fields(line)
			if len(fields) != 2 || !isSHA(fields[0]) || fields[1] != branchRef && forestNoteRefIndex(fields[1]) < 0 {
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

func (p *Poller) note(ctx context.Context, ref, sha string) ([]byte, error) {
	payload, err := p.git(ctx, "notes", "--ref="+ref, "show", sha)
	if err == nil {
		return payload, nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil, err
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ProcessState != nil && exitErr.ProcessState.ExitCode() == 1 {
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
		return nil, err
	}
	path := ""
	for _, candidate := range strings.Split(strings.TrimSpace(string(treeOutput)), "\n") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" || strings.ReplaceAll(candidate, "/", "") != sha {
			continue
		}
		if path != "" {
			return nil, fmt.Errorf("duplicate note path on %s for %s", ref, sha)
		}
		path = candidate
	}
	if path == "" {
		return nil, fmt.Errorf("note path on %s does not bind %s", ref, sha)
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

func exactGitLine(output []byte) (string, error) {
	if len(output) == 0 || output[len(output)-1] != '\n' {
		return "", errors.New("git output is not one terminated line")
	}
	line := output[:len(output)-1]
	if len(line) > 0 && line[len(line)-1] == '\r' {
		line = line[:len(line)-1]
	}
	if bytes.IndexAny(line, "\r\n") >= 0 {
		return "", errors.New("git output is not one terminated line")
	}
	return string(line), nil
}

func isMissingNote(err error) bool {
	return errors.Is(err, pollMissingNote)
}

type reviewRequest struct {
	Schema   string `json:"schema"`
	Issue    int    `json:"issue"`
	Branch   string `json:"branch"`
	Revision string `json:"revision"`
	Time     string `json:"time"`
}

type checksNote struct {
	Results []checkResult
}

type checkResult struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	Exit int    `json:"exit"`
}

type checkResultPayload struct {
	Name *string `json:"name"`
	OK   *bool   `json:"ok"`
	Exit *int    `json:"exit"`
}

type checksNotePayload struct {
	Schema   string               `json:"schema"`
	Revision string               `json:"revision"`
	Results  []checkResultPayload `json:"results"`
	Time     string               `json:"time"`
}

type verdictNote struct {
	Schema   string `json:"schema"`
	Revision string `json:"revision"`
	Verdict  string `json:"verdict"`
	Summary  string `json:"summary"`
	Time     string `json:"time"`
}

type strictJSONShape struct {
	fields  map[string]*strictJSONShape
	element *strictJSONShape
}

var strictJSONValue = &strictJSONShape{}

var reviewJSONShape = &strictJSONShape{fields: map[string]*strictJSONShape{
	"schema":   strictJSONValue,
	"issue":    strictJSONValue,
	"branch":   strictJSONValue,
	"revision": strictJSONValue,
	"time":     strictJSONValue,
}}

var checkResultJSONShape = &strictJSONShape{fields: map[string]*strictJSONShape{
	"name": strictJSONValue,
	"ok":   strictJSONValue,
	"exit": strictJSONValue,
}}

var checksJSONShape = &strictJSONShape{fields: map[string]*strictJSONShape{
	"schema":   strictJSONValue,
	"revision": strictJSONValue,
	"results":  {element: checkResultJSONShape},
	"time":     strictJSONValue,
}}

var verdictJSONShape = &strictJSONShape{fields: map[string]*strictJSONShape{
	"schema":   strictJSONValue,
	"revision": strictJSONValue,
	"verdict":  strictJSONValue,
	"summary":  strictJSONValue,
	"time":     strictJSONValue,
}}

func scanStrictJSON(dec *json.Decoder, shape *strictJSONShape) error {
	token, err := dec.Token()
	if err != nil {
		return err
	}
	switch delimiter := token.(type) {
	case json.Delim:
		switch delimiter {
		case '{':
			if shape == nil || shape.fields == nil {
				return fmt.Errorf("invalid JSON object")
			}
			seen := make(map[string]struct{}, len(shape.fields))
			for dec.More() {
				key, err := dec.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok {
					return fmt.Errorf("invalid JSON object key")
				}
				child, allowed := shape.fields[name]
				if !allowed {
					return fmt.Errorf("unknown JSON object key %q", name)
				}
				if _, ok := seen[name]; ok {
					return fmt.Errorf("duplicate JSON object key %q", name)
				}
				seen[name] = struct{}{}
				if err := scanStrictJSON(dec, child); err != nil {
					return err
				}
			}
			closing, err := dec.Token()
			if err != nil {
				return err
			}
			if closing != json.Delim('}') {
				return fmt.Errorf("invalid JSON object")
			}
		case '[':
			if shape == nil || shape.element == nil {
				return fmt.Errorf("invalid JSON array")
			}
			for dec.More() {
				if err := scanStrictJSON(dec, shape.element); err != nil {
					return err
				}
			}
			closing, err := dec.Token()
			if err != nil {
				return err
			}
			if closing != json.Delim(']') {
				return fmt.Errorf("invalid JSON array")
			}
		default:
			return fmt.Errorf("invalid JSON delimiter %q", delimiter)
		}
	default:
		if shape == nil || shape.fields != nil || shape.element != nil {
			return fmt.Errorf("invalid JSON value")
		}
	}
	return nil
}

func decodeStrictJSON(data []byte, target any, shape *strictJSONShape) error {
	scanner := json.NewDecoder(bytes.NewReader(data))
	if err := scanStrictJSON(scanner, shape); err != nil {
		return err
	}
	if _, err := scanner.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing JSON value")
		}
		return err
	}
	return json.Unmarshal(data, target)
}

func validNoteTime(value string) bool {
	if strings.TrimSpace(value) == "" {
		return false
	}
	_, err := time.Parse(time.RFC3339Nano, value)
	return err == nil
}

func decodeReview(data []byte, sha string) (reviewRequest, error) {
	var note reviewRequest
	if err := decodeStrictJSON(data, &note, reviewJSONShape); err != nil {
		return note, err
	}
	if note.Schema != "forest.review-request.v1" || note.Revision != sha || note.Issue <= 0 || !validBranch(note.Branch, note.Issue) || !validNoteTime(note.Time) {
		return note, fmt.Errorf("invalid review-request note")
	}
	return note, nil
}

func validBranch(branch string, issue int) bool {
	if !strings.HasPrefix(branch, "forest/") {
		return false
	}
	parts := strings.SplitN(strings.TrimPrefix(branch, "forest/"), "-", 2)
	return len(parts) == 2 && parts[0] == strconv.Itoa(issue) && parts[1] != "" && !strings.Contains(parts[1], "/")
}

func validatePollReviewRequestBranch(data []byte, sha, branch string) error {
	note, err := decodeReview(data, sha)
	if err != nil {
		return err
	}
	if note.Branch != branch {
		return fmt.Errorf("review-request branch %q does not match observed branch %q", note.Branch, branch)
	}
	return nil
}

func decodeChecks(data []byte, sha string) (checksNote, error) {
	var payload checksNotePayload
	if err := decodeStrictJSON(data, &payload, checksJSONShape); err != nil {
		return checksNote{}, err
	}
	if payload.Schema != "forest.checks.v1" || payload.Revision != sha || !validNoteTime(payload.Time) || len(payload.Results) == 0 {
		return checksNote{}, fmt.Errorf("invalid checks note")
	}
	note := checksNote{Results: make([]checkResult, len(payload.Results))}
	seen := make(map[string]bool, len(payload.Results))
	for index, result := range payload.Results {
		if result.Name == nil || result.OK == nil || result.Exit == nil {
			return checksNote{}, fmt.Errorf("checks result fields are required")
		}
		note.Results[index] = checkResult{Name: *result.Name, OK: *result.OK, Exit: *result.Exit}
		if strings.TrimSpace(*result.Name) == "" || seen[*result.Name] || *result.Exit < 0 || (*result.OK && *result.Exit != 0) {
			return checksNote{}, fmt.Errorf("invalid checks result")
		}
		seen[*result.Name] = true
	}
	return note, nil
}

func decodeVerdict(data []byte, sha string) (verdictNote, error) {
	var note verdictNote
	if err := decodeStrictJSON(data, &note, verdictJSONShape); err != nil {
		return note, err
	}
	if note.Schema != "forest.verdict.v1" || note.Revision != sha || (note.Verdict != "approve" && note.Verdict != "changes") || strings.TrimSpace(note.Summary) == "" || !validNoteTime(note.Time) {
		return note, fmt.Errorf("invalid verdict note")
	}
	return note, nil
}
