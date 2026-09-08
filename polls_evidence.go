package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

func newPrivateEvidenceRef(prefix string) (string, error) {
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(id[:]), nil
}

func evidenceKindRef(kind, sha string) string {
	switch kind {
	case "request":
		return evidenceRequestRefPrefix + sha
	case "checks":
		return evidenceChecksRefPrefix + sha
	case "verdict":
		return evidenceVerdictRefPrefix + sha
	default:
		return ""
	}
}

func evidenceFileName(kind string) string {
	switch kind {
	case "request":
		return "request.json"
	case "checks":
		return "checks.json"
	case "verdict":
		return "verdict.json"
	default:
		return ""
	}
}

func (p *Poller) evidencePayload(ctx context.Context, kind, sha string, roles ...string) ([]byte, error) {
	payload, _, err := p.evidencePayloadAndOID(ctx, kind, sha, roles...)
	return payload, err
}

func (p *Poller) evidencePayloadAndOID(ctx context.Context, kind, sha string, roles ...string) ([]byte, string, error) {
	ref := evidenceKindRef(kind, sha)
	if ref == "" || !isSHA(sha) {
		return nil, "", fmt.Errorf("invalid evidence identity")
	}
	output, err := p.git(ctx, "ls-remote", "origin", ref)
	if err != nil {
		return nil, "", err
	}
	oid, err := parseSingleRemoteOID(output, ref)
	if err != nil {
		return nil, "", err
	}
	if oid == "" {
		return nil, "", pollMissingNote
	}
	payload, err := p.fetchEvidencePayload(ctx, kind, sha, oid, roles...)
	if err != nil {
		return nil, "", err
	}
	return payload, oid, nil
}

func (p *Poller) fetchEvidencePayload(ctx context.Context, kind, sha, oid string, roles ...string) ([]byte, error) {
	ref := evidenceKindRef(kind, sha)
	if ref == "" || !isSHA(sha) || !isSHA(oid) {
		return nil, fmt.Errorf("invalid evidence identity")
	}
	local, err := newPrivateEvidenceRef("refs/forest/private/poll/" + kind + "/")
	if err != nil {
		return nil, err
	}
	if _, err := p.git(ctx, "fetch", "origin", ref+":"+local); err != nil {
		return nil, err
	}
	defer p.deletePrivateRef(local)
	fetched, err := p.git(ctx, "rev-parse", "--verify", local)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(fetched)) != oid {
		return nil, fmt.Errorf("evidence ref moved while fetching %s", ref)
	}
	identityOut, err := p.git(ctx, "log", "-1", "--format=%an%x00%ae", local)
	if err != nil {
		return nil, err
	}
	identityLine, err := exactGitLine(identityOut)
	if err != nil {
		return nil, fmt.Errorf("malformed evidence identity on %s", ref)
	}
	parts := strings.SplitN(identityLine, "\x00", 2)
	if len(parts) != 2 || !validIdentity(noteEntry{Author: parts[0], Email: parts[1]}, roles...) {
		return nil, fmt.Errorf("wrong note identity on %s for %s", ref, sha)
	}
	return p.git(ctx, "show", local+":"+evidenceFileName(kind))
}

func (p *Poller) deletePrivateRef(ref string) {
	clean, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	_, _ = p.git(clean, "update-ref", "-d", ref)
}

func (p *Poller) fetchPollPrimary(ctx context.Context, snapshot pollSnapshot) (string, error) {
	private, err := newPrivateEvidenceRef(pollPrimaryPrivatePrefix)
	if err != nil {
		return "", err
	}
	fail := func(cause error) (string, error) {
		p.deletePrivateRef(private)
		return "", cause
	}
	if _, err := p.git(ctx, "fetch", "origin", "+"+snapshot.PrimaryRef+":"+private); err != nil {
		return fail(fmt.Errorf("fetch primary snapshot: %w", err))
	}
	fetched, err := p.git(ctx, "rev-parse", "--verify", private)
	if err != nil {
		return fail(err)
	}
	if strings.TrimSpace(string(fetched)) != snapshot.PrimarySHA {
		return fail(fmt.Errorf("remote snapshot moved for %s", snapshot.PrimaryRef))
	}
	return private, nil
}

func (p *Poller) tipIsAncestor(ctx context.Context, tip branchTip, primaryPrivate string) (bool, error) {
	if _, err := p.git(ctx, "merge-base", "--is-ancestor", tip.SHA, primaryPrivate); err != nil {
		// Exit 1 is "not an ancestor". Exit 128 is "not a valid commit name",
		// which for a tip we have already fetched is a missing local object;
		// a missing tip cannot be an ancestor and must stay a candidate.
		if soleExitCode(err, 1) || soleExitCode(err, 128) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func parseSingleRemoteOID(output []byte, wantRef string) (string, error) {
	text := strings.TrimSpace(string(output))
	if text == "" {
		return "", nil
	}
	fields := strings.Fields(text)
	if len(fields) != 2 || !isSHA(fields[0]) || fields[1] != wantRef {
		return "", fmt.Errorf("malformed ls-remote for %s", wantRef)
	}
	return fields[0], nil
}

func (p *Poller) confirmEvidence(ctx context.Context, tip branchTip, requestOID, verdictOID string) error {
	branchRef := "refs/heads/" + tip.Name
	requestRef := evidenceKindRef("request", tip.SHA)
	verdictRef := evidenceKindRef("verdict", tip.SHA)
	args := []string{"ls-remote", "origin", branchRef, requestRef, verdictRef}
	output, err := p.git(ctx, args...)
	if err != nil {
		return err
	}
	expected := map[string]string{branchRef: tip.SHA}
	if requestOID != "" {
		expected[requestRef] = requestOID
	}
	if verdictOID != "" {
		expected[verdictRef] = verdictOID
	}
	actual := map[string]string{}
	text := strings.TrimSpace(string(output))
	if text != "" {
		for _, line := range strings.Split(text, "\n") {
			fields := strings.Fields(line)
			if len(fields) != 2 || !isSHA(fields[0]) {
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
