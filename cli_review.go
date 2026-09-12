package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
)

type reviewCommitIdentity struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Time  string `json:"time"`
}

type reviewCommit struct {
	SHA       string               `json:"sha"`
	Author    reviewCommitIdentity `json:"author"`
	Committer reviewCommitIdentity `json:"committer"`
}

// Each piece is independently readable, missing, or unreadable. A valid verdict
// is evidence of a decision, not a claim that checks passed or primary advanced.
type reviewRow struct {
	Revision      string            `json:"revision"`
	Branch        string            `json:"branch,omitempty"`
	Work          *WorkReference    `json:"work,omitempty"`
	Decision      string            `json:"decision,omitempty"`
	Summary       string            `json:"summary,omitempty"`
	RunID         string            `json:"run_id,omitempty"`
	VerifierRunID string            `json:"verifier_run_id,omitempty"`
	RequestID     string            `json:"request_id,omitempty"`
	Authority     string            `json:"authority,omitempty"`
	RequestRef    string            `json:"request_ref,omitempty"`
	ChecksRef     string            `json:"checks_ref,omitempty"`
	VerdictRef    string            `json:"verdict_ref,omitempty"`
	RequestCommit *reviewCommit     `json:"request_commit,omitempty"`
	ChecksCommit  *reviewCommit     `json:"checks_commit,omitempty"`
	VerdictCommit *reviewCommit     `json:"verdict_commit,omitempty"`
	RequestState  string            `json:"request_state"`
	ChecksState   string            `json:"checks_state"`
	VerdictState  string            `json:"verdict_state"`
	Errors        map[string]string `json:"errors,omitempty"`
	Runs          []RunRecord       `json:"runs"`
}

func runReviewList(_ []string, flags cliFlags) cliOutcome {
	return runReviewRead("", flags)
}

func runReviewShow(rest []string, flags cliFlags) cliOutcome {
	if !isSHA(rest[0]) {
		return failure(exitInvalidArg, "review show requires a full candidate SHA")
	}
	return runReviewRead(strings.ToLower(rest[0]), flags)
}

func runReviewRead(revision string, flags cliFlags) cliOutcome {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	rows, err := readPublishedReviews(ctx, flags.root, revision)
	if err != nil {
		return failure(exitError, "read published reviews: %s", err)
	}
	if revision != "" {
		if len(rows) == 0 {
			return failure(exitNotFound, "no published review for %s", revision)
		}
		return cliOutcome{Data: struct {
			Review reviewRow `json:"review"`
		}{rows[0]}, Human: humanReviews(rows)}
	}
	return cliOutcome{Data: struct {
		Reviews []reviewRow `json:"reviews"`
	}{rows}, Human: humanReviews(rows)}
}

func readPublishedReviews(ctx context.Context, root, revision string) (rows []reviewRow, err error) {
	deps := defaultAuditDependencies()
	snapshot, err := newAuditSnapshot()
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, clearEvidenceSnapshot(root, snapshot, deps)) }()
	// Reuse the Auditor's advertised/fetched/confirmed remote snapshot. Local
	// loose refs, packed refs, branches, and PR comments are not alternate stores.
	snapshot, err = fetchEvidenceAuditSnapshot(ctx, root, snapshot, deps)
	if err != nil {
		return nil, err
	}
	revisions := map[string]bool{}
	for ref := range snapshot.Evidence {
		_, sha, ok := parseEvidenceRef(ref)
		if !ok {
			return nil, fmt.Errorf("malformed evidence ref %s", ref)
		}
		if revision == "" || sha == revision {
			revisions[sha] = true
		}
	}
	runsByWork := map[WorkReference][]RunRecord{}
	if err := visitLedger(root, func(run RunRecord) error {
		if run.Work != nil {
			runsByWork[*run.Work] = append(runsByWork[*run.Work], run)
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read review runs: %w", err)
	}
	ordered := make([]string, 0, len(revisions))
	for sha := range revisions {
		ordered = append(ordered, sha)
	}
	slices.Sort(ordered)
	rows = make([]reviewRow, 0, len(ordered))
	for _, sha := range ordered {
		row := reviewRow{Revision: sha, RequestState: "missing", ChecksState: "missing", VerdictState: "missing"}
		for _, kind := range []string{"request", "checks", "verdict"} {
			ref := evidenceKindRef(kind, sha)
			oid := snapshot.Evidence[ref]
			if oid == "" {
				continue
			}
			commit, payload, pieceErr := readReviewPiece(ctx, root, kind, oid, deps)
			state := "readable"
			if pieceErr == nil {
				switch kind {
				case "request":
					var note reviewRequest
					note, pieceErr = decodeReview(payload, sha)
					if pieceErr == nil {
						row.Branch, row.Work = note.Branch, note.Work
						row.RunID, row.RequestID, row.Authority = note.RunID, note.RequestID, note.Authority
					}
				case "checks":
					_, pieceErr = decodeChecks(payload, sha)
				case "verdict":
					var note verdictNote
					note, pieceErr = decodeVerdict(payload, sha)
					if pieceErr == nil {
						row.Decision, row.Summary = note.Verdict, note.Summary
						if note.VerifierRunID != nil {
							row.VerifierRunID = *note.VerifierRunID
						}
					}
				}
			}
			if pieceErr != nil {
				state = "unreadable"
				if row.Errors == nil {
					row.Errors = map[string]string{}
				}
				row.Errors[kind] = pieceErr.Error()
			}
			switch kind {
			case "request":
				row.RequestRef, row.RequestCommit, row.RequestState = ref, commit, state
			case "checks":
				row.ChecksRef, row.ChecksCommit, row.ChecksState = ref, commit, state
			case "verdict":
				row.VerdictRef, row.VerdictCommit, row.VerdictState = ref, commit, state
			}
		}
		row.Runs = []RunRecord{}
		if row.Work != nil && len(runsByWork[*row.Work]) > 0 {
			row.Runs = runsByWork[*row.Work]
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func readReviewPiece(ctx context.Context, root, kind, oid string, deps auditDependencies) (*reviewCommit, []byte, error) {
	commit := &reviewCommit{SHA: oid}
	output, err := deps.runGit(ctx, root, "show", "-s", "--format=%an%x00%ae%x00%aI%x00%cn%x00%ce%x00%cI", oid)
	if err != nil {
		return commit, nil, fmt.Errorf("read evidence commit: %w", err)
	}
	line, err := exactGitLine(output)
	fields := strings.Split(line, "\x00")
	if err != nil || len(fields) != 6 {
		return commit, nil, fmt.Errorf("malformed evidence commit identity")
	}
	commit.Author = reviewCommitIdentity{Name: fields[0], Email: fields[1], Time: fields[2]}
	commit.Committer = reviewCommitIdentity{Name: fields[3], Email: fields[4], Time: fields[5]}
	roles := []string{"verifier"}
	if kind == "request" {
		roles = []string{"builder", "fixer"}
	}
	if !validIdentity(noteEntry{Author: commit.Committer.Name, Email: commit.Committer.Email}, roles...) {
		return commit, nil, fmt.Errorf("wrong evidence committer identity")
	}
	payload, err := deps.runGit(ctx, root, "show", oid+":"+evidenceFileName(kind))
	if err != nil {
		return commit, nil, fmt.Errorf("read evidence payload: %w", err)
	}
	return commit, payload, nil
}

func humanReviews(rows []reviewRow) string {
	if len(rows) == 0 {
		return "no published reviews"
	}
	var text strings.Builder
	for index, row := range rows {
		if index > 0 {
			text.WriteByte('\n')
		}
		fmt.Fprintf(&text, "%s", row.Revision)
		if row.Branch != "" {
			fmt.Fprintf(&text, " %s", row.Branch)
		}
		fmt.Fprintf(&text, " request=%s checks=%s verdict=%s", row.RequestState, row.ChecksState, row.VerdictState)
		if row.VerifierRunID != "" {
			fmt.Fprintf(&text, " verifier_run_id=%s", row.VerifierRunID)
		} else if row.VerdictState == "readable" {
			text.WriteString(" verifier=unbound")
		}
		if row.Decision != "" {
			fmt.Fprintf(&text, " decision=%s\n  %s", row.Decision, row.Summary)
		}
		for _, kind := range []string{"request", "checks", "verdict"} {
			if reason := row.Errors[kind]; reason != "" {
				fmt.Fprintf(&text, "\n  %s: %s", kind, reason)
			}
		}
	}
	return text.String()
}
