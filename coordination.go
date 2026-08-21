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

const (
	reviewRequestNoteRef = "refs/notes/forest/review-request"
	checksNoteRef        = "refs/notes/forest/checks"
	verdictNoteRef       = "refs/notes/forest/verdict"
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
	Schema   string `json:"schema"`
	Subject  string `json:"subject"`
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
	if err := decodeStrictJSON(data, &note, objectJSONShape("schema", "subject", "branch", "revision", "time")); err != nil {
		return note, err
	}
	if note.Schema != "forest.review-request.v2" || note.Revision != sha || !branchBelongsToSubject(note.Branch, note.Subject) || !validNoteTime(note.Time) {
		return note, fmt.Errorf("invalid review-request note")
	}
	return note, nil
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

// decodeRequestEvidence decodes review-request evidence for the Auditor
// read-only sweep. The v2 shape keeps its strict decoder as the authority;
// the legacy v1 shape is tolerated so immutable pre-cutover refs do not
// produce a permanent violation.
func decodeRequestEvidence(data []byte, sha string) error {
	var probe struct {
		Schema string `json:"schema"`
	}
	_ = json.Unmarshal(data, &probe)
	if probe.Schema == "forest.review-request.v1" {
		_, err := decodeLegacyReview(data, sha)
		return err
	}
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
