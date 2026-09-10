package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

type noteEntry struct {
	Ref      string
	Revision string
	Payload  []byte
	Author   string
	Email    string
}

func validIdentity(entry noteEntry, roles ...string) bool {
	for _, role := range roles {
		var name, email string
		switch role {
		case "builder":
			name, email = "Iron Forest Builder", "builder@forest.invalid"
		case "fixer":
			name, email = "Iron Forest Fixer", "fixer@forest.invalid"
		case "verifier":
			name, email = "Iron Forest Verifier", "verifier@forest.invalid"
		default:
			continue
		}
		if entry.Author == name && entry.Email == email {
			return true
		}
	}
	return false
}

// isAgentAuthor reports whether a commit author name and email match one of
// the shipped declaration identities. The Auditor uses it to distinguish an
// operator direct push from a factory-authored Revision that must carry Gate
// evidence.
func isAgentAuthor(name, email string) bool {
	return validIdentity(noteEntry{Author: name, Email: email}, "builder", "fixer", "verifier")
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

func isSHA(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, character := range value {
		if !(character >= '0' && character <= '9') && !(character >= 'a' && character <= 'f') && !(character >= 'A' && character <= 'F') {
			return false
		}
	}
	return true
}

type reviewRequest struct {
	Schema    string         `json:"schema"`
	Subject   string         `json:"subject"`
	Branch    string         `json:"branch"`
	Revision  string         `json:"revision"`
	Time      string         `json:"time"`
	RunID     string         `json:"run_id,omitempty"`
	RequestID string         `json:"request_id,omitempty"`
	Work      *WorkReference `json:"work,omitempty"`
	// Tracker is read-only compatibility for immutable v2 evidence.
	Tracker string `json:"tracker,omitempty"`
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
	stringOnly bool
}

func objectJSONShape(fields ...string) *strictJSONShape {
	value := &strictJSONShape{}
	shape := &strictJSONShape{fields: make(map[string]*strictJSONShape, len(fields))}
	for _, field := range fields {
		shape.fields[field] = value
	}
	return shape
}

func scanStrictJSON(decoder *json.Decoder, shape *strictJSONShape) error {
	token, err := decoder.Token()
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
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok {
					return fmt.Errorf("invalid JSON object key")
				}
				child, allowed := shape.fields[name]
				if !allowed {
					return fmt.Errorf("unknown JSON object key")
				}
				if _, ok := seen[name]; ok {
					return fmt.Errorf("duplicate JSON object key")
				}
				seen[name] = struct{}{}
				if err := scanStrictJSON(decoder, child); err != nil {
					return err
				}
			}
			closing, err := decoder.Token()
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
			for decoder.More() {
				if err := scanStrictJSON(decoder, shape.element); err != nil {
					return err
				}
			}
			closing, err := decoder.Token()
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
		if _, ok := token.(string); shape.stringOnly && !ok {
			return fmt.Errorf("invalid JSON string")
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

func validTracker(value string) bool {
	switch value {
	case "", "github", "powder":
		return true
	default:
		return false
	}
}

func decodeReview(data []byte, sha string) (reviewRequest, error) {
	var probe struct {
		Schema    string          `json:"schema"`
		RequestID json.RawMessage `json:"request_id"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return reviewRequest{}, err
	}
	if probe.Schema == "forest.review-request.v1" {
		legacy, err := decodeLegacyReview(data, sha)
		return reviewRequest{Schema: legacy.Schema, Subject: fmt.Sprint(legacy.Issue),
			Branch: legacy.Branch, Revision: legacy.Revision, Time: legacy.Time}, err
	}
	shape := objectJSONShape("schema", "subject", "branch", "revision", "time")
	switch probe.Schema {
	case "forest.review-request.v2":
		shape.fields["tracker"] = &strictJSONShape{}
	case "forest.review-request.v3":
		text := &strictJSONShape{stringOnly: true}
		shape.fields["run_id"] = text
		shape.fields["request_id"] = text
		for field := range shape.fields {
			shape.fields[field] = text
		}
		shape.fields["work"] = objectJSONShape("system", "id", "key", "url")
		for field := range shape.fields["work"].fields {
			shape.fields["work"].fields[field] = text
		}
	default:
		return reviewRequest{}, fmt.Errorf("invalid review-request schema")
	}
	var note reviewRequest
	if err := decodeStrictJSON(data, &note, shape); err != nil {
		return note, err
	}
	if !isSHA(sha) || note.Revision != sha || !branchBelongsToSubject(note.Branch, note.Subject) || !validNoteTime(note.Time) {
		return note, fmt.Errorf("invalid review-request note")
	}
	if note.Schema == "forest.review-request.v2" {
		if !validTracker(note.Tracker) {
			return note, fmt.Errorf("invalid review-request tracker")
		}
	} else if !validPublicationRunID(note.RunID) ||
		(probe.RequestID != nil && strings.TrimSpace(note.RequestID) == "") ||
		(note.Work != nil && (strings.TrimSpace(note.Work.System) == "" || strings.TrimSpace(note.Work.ID) == "")) {
		return note, fmt.Errorf("invalid review-request Run or work identity")
	}
	return note, nil
}

func validPublicationRunID(runID string) bool {
	return runID != "" && !strings.ContainsAny(runID, "/\\ \t\r\n") && runID != "." && runID != ".."
}

func sameWorkReference(left, right *WorkReference) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

type legacyReviewRequest struct {
	Schema   string `json:"schema"`
	Issue    int    `json:"issue"`
	Branch   string `json:"branch"`
	Revision string `json:"revision"`
	Time     string `json:"time"`
}

// decodeLegacyReview accepts the notes-era review-request shape that predates
// the v2 cutover. It is used only as read-only evidence compatibility: the old
// ref stays in the immutable refs/forest/v1/* namespace and is never rewritten.
func decodeLegacyReview(data []byte, sha string) (legacyReviewRequest, error) {
	var note legacyReviewRequest
	if err := decodeStrictJSON(data, &note, objectJSONShape("schema", "issue", "branch", "revision", "time")); err != nil {
		return note, err
	}
	if note.Schema != "forest.review-request.v1" || note.Issue <= 0 || strings.TrimSpace(note.Branch) == "" || note.Revision != sha || !validNoteTime(note.Time) {
		return note, fmt.Errorf("invalid legacy review-request note")
	}
	return note, nil
}

// decodeRequestEvidence accepts current and historical immutable evidence.
// Compatibility is read-only; publication accepts only the current schema.
func decodeRequestEvidence(data []byte, sha string) error {
	_, err := decodeReview(data, sha)
	return err
}

func validSubject(subject string) bool {
	if n := len(subject); n < 1 || n > 128 {
		return false
	}
	first := subject[0]
	if !((first >= 'A' && first <= 'Z') || (first >= 'a' && first <= 'z') || (first >= '0' && first <= '9')) {
		return false
	}
	for i := 1; i < len(subject); i++ {
		c := subject[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '.' || c == '_' || c == '-' {
			continue
		}
		return false
	}
	return true
}

func validWorkSlug(slug string) bool {
	if slug == "" {
		return false
	}
	for index := range len(slug) {
		character := slug[index]
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') {
			continue
		}
		if character != '-' || index == 0 || index == len(slug)-1 || slug[index-1] == '-' {
			return false
		}
	}
	return true
}

func parseForestBranch(branch string) (string, string, bool) {
	rest, ok := strings.CutPrefix(branch, "forest/")
	if !ok {
		return "", "", false
	}
	subject, slug, found := strings.Cut(rest, "/")
	if !found || strings.Contains(slug, "/") || !validSubject(subject) || !validWorkSlug(slug) {
		return "", "", false
	}
	return subject, slug, true
}

func validForestBranch(branch string) bool {
	_, _, ok := parseForestBranch(branch)
	return ok
}

func branchBelongsToSubject(branch, subject string) bool {
	parsed, _, ok := parseForestBranch(branch)
	return ok && parsed == subject
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
	shape := objectJSONShape("schema", "revision", "results", "time")
	shape.fields["results"] = &strictJSONShape{element: objectJSONShape("name", "ok", "exit")}
	var payload checksNotePayload
	if err := decodeStrictJSON(data, &payload, shape); err != nil {
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
	if err := decodeStrictJSON(data, &note, objectJSONShape("schema", "revision", "verdict", "summary", "time")); err != nil {
		return note, err
	}
	if note.Schema != "forest.verdict.v1" || note.Revision != sha || (note.Verdict != "approve" && note.Verdict != "changes") || strings.TrimSpace(note.Summary) == "" || !validNoteTime(note.Time) {
		return note, fmt.Errorf("invalid verdict note")
	}
	return note, nil
}
