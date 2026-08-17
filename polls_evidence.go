package main

import (
	"context"
	"fmt"
	"strings"
)

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
	ref := evidenceKindRef(kind, sha)
	if ref == "" || !isSHA(sha) {
		return nil, fmt.Errorf("invalid evidence identity")
	}
	output, err := p.git(ctx, "ls-remote", "origin", ref)
	if err != nil {
		return nil, err
	}
	oid, err := parseSingleRemoteOID(output, ref)
	if err != nil {
		return nil, err
	}
	if oid == "" {
		return nil, pollMissingNote
	}
	local := "refs/forest/private/poll/" + kind + "/" + sha
	if _, err := p.git(ctx, "fetch", "origin", ref+":"+local); err != nil {
		return nil, err
	}
	defer func() {
		clean, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		_, _ = p.git(clean, "update-ref", "-d", local)
	}()
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

func (p *Poller) remoteEvidenceOID(ctx context.Context, kind, sha string) (string, error) {
	ref := evidenceKindRef(kind, sha)
	output, err := p.git(ctx, "ls-remote", "origin", ref)
	if err != nil {
		return "", err
	}
	return parseSingleRemoteOID(output, ref)
}
