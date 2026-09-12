package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// WorkReference is an opaque, immutable association. Kernel never resolves it.
type WorkReference struct {
	System string `json:"system"`
	ID     string `json:"id"`
	Key    string `json:"key,omitempty"`
	URL    string `json:"url,omitempty"`
}

type RunRequest struct {
	Schema    string         `json:"schema"`
	ID        string         `json:"id"`
	Prompt    string         `json:"prompt"`
	Authority string         `json:"authority,omitempty"`
	Work      *WorkReference `json:"work,omitempty"`
}

const maxRequestBytes = 1 << 20

var errRequestNoWork = errors.New("request selection returned no work")

func validRunAuthority(authority string) bool {
	return authority == "" || authority == "land" || authority == "review"
}

func decodeRunRequest(data []byte) (*RunRequest, error) {
	if len(data) > maxRequestBytes {
		return nil, fmt.Errorf("request exceeds %d bytes", maxRequestBytes)
	}
	var request RunRequest
	// Keep presence separate from the retained string: null and an explicit
	// empty string must not silently acquire the legacy profile authority.
	type requestFields RunRequest
	wire := struct {
		*requestFields
		Authority json.RawMessage `json:"authority"`
	}{requestFields: (*requestFields)(&request)}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return nil, fmt.Errorf("parse request: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("request must contain exactly one JSON object")
	}
	if len(wire.Authority) != 0 {
		if err := json.Unmarshal(wire.Authority, &request.Authority); err != nil || request.Authority == "" || !validRunAuthority(request.Authority) {
			return nil, errors.New("request authority must be land or review when supplied")
		}
	}
	if request.Schema != "forest.request.v1" || strings.TrimSpace(request.ID) == "" || strings.TrimSpace(request.Prompt) == "" {
		return nil, errors.New("request requires schema forest.request.v1, id and prompt")
	}
	if request.Work != nil && (strings.TrimSpace(request.Work.System) == "" || strings.TrimSpace(request.Work.ID) == "") {
		return nil, errors.New("request work requires system and immutable id")
	}
	return &request, nil
}

func readRunRequest(path string) (*RunRequest, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxRequestBytes+1))
	if err != nil {
		return nil, err
	}
	return decodeRunRequest(data)
}

// requestForRun executes a profile-owned selector only after admission, with the
// allocated Run identity. stdout is the request; diagnostics stay in the Run log.
func (r *Runner) requestForRun(ctx context.Context, declaration Declaration, record RunRecord, log io.Writer) (*RunRequest, error) {
	if declaration.Request != nil {
		request := *declaration.Request
		if request.Work != nil {
			work := *request.Work
			request.Work = &work
		}
		return &request, nil
	}
	if declaration.RequestCommand == "" {
		return nil, nil
	}
	requestCtx, cancel := context.WithTimeout(ctx, pollCommandTimeout)
	defer cancel()
	command := exec.Command("/bin/sh", "-c", declaration.RequestCommand)
	command.Dir = r.Root
	command.Stderr = log
	path, err := trustedPath(r.Root)
	if err != nil {
		return nil, err
	}
	command.Env = append(childEnvironment(), "PATH="+path, "FOREST_ROOT="+r.Root, "FOREST_RUN_ID="+record.RunID)
	data, err := processGroupOutput(requestCtx, command)
	if err != nil {
		// A clean exit 1 is a healthy selection race. Do not hide a timeout,
		// output overflow, or cleanup error joined to that process exit.
		cause := err
		for {
			joined, ok := cause.(interface{ Unwrap() []error })
			if !ok || len(joined.Unwrap()) != 1 {
				break
			}
			cause = joined.Unwrap()[0]
		}
		if exit, ok := cause.(*exec.ExitError); ok && exit.ExitCode() == exitNoWork && requestCtx.Err() == nil {
			return nil, errRequestNoWork
		}
		return nil, fmt.Errorf("request command: %w", err)
	}
	return decodeRunRequest(data)
}

func attachRunRequest(record *RunRecord, request *RunRequest) {
	if request == nil {
		return
	}
	record.RequestID = request.ID
	record.Authority = request.Authority
	if request.Work != nil {
		work := *request.Work
		record.Work = &work
	}
}

func requestPrompt(standing string, request *RunRequest) string {
	if request == nil {
		return standing
	}
	prompt := standing + "\n\n## Explicit Run request\n\n"
	if request.Authority != "" {
		prompt += "Publication authority: " + request.Authority + "\n\n"
	}
	return prompt + request.Prompt
}

// RunCompletion is the profile's observation of its required external effect.
// Kernel retains this opaque evidence without interpreting the work system.
type RunCompletion struct {
	Schema   string `json:"schema"`
	Status   string `json:"status"`
	Reason   string `json:"reason,omitempty"`
	Evidence string `json:"evidence,omitempty"`
}

type completionContext struct {
	Schema  string      `json:"schema"`
	Run     RunRecord   `json:"run"`
	Request *RunRequest `json:"request"`
}

func decodeRunCompletion(data []byte) (*RunCompletion, error) {
	if len(data) > trustedTransportOutputLimit {
		return nil, fmt.Errorf("completion exceeds %d bytes", trustedTransportOutputLimit)
	}
	var completion RunCompletion
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&completion); err != nil {
		return nil, fmt.Errorf("parse completion: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("completion must contain exactly one JSON object")
	}
	if completion.Schema != "forest.completion.v1" {
		return nil, errors.New("completion requires schema forest.completion.v1")
	}
	switch completion.Status {
	case "completed":
		if strings.TrimSpace(completion.Evidence) == "" {
			return nil, errors.New("completed observation requires evidence")
		}
	case "incomplete", "unknown":
		if strings.TrimSpace(completion.Reason) == "" {
			return nil, errors.New("incomplete or unknown observation requires a reason")
		}
	default:
		return nil, errors.New("completion status must be completed, incomplete, or unknown")
	}
	return &completion, nil
}

// completionForRun is a bounded, profile-owned observer. A missing command
// leaves evidence absent; malformed output or a failed command is unknown,
// even when its stdout contains an otherwise valid completed observation.
func (r *Runner) completionForRun(ctx context.Context, declaration Declaration, record RunRecord, request *RunRequest, log io.Writer) *RunCompletion {
	if declaration.CompletionCommand == "" {
		return nil
	}
	unknown := func(err error) *RunCompletion {
		_, _ = fmt.Fprintf(log, "completion observation: %v\n", err)
		return &RunCompletion{Schema: "forest.completion.v1", Status: "unknown", Reason: err.Error()}
	}
	if err := r.verifyDeclarationDigest(declaration); err != nil {
		return unknown(err)
	}
	input, err := json.Marshal(completionContext{Schema: "forest.completion-context.v1", Run: record, Request: request})
	if err != nil {
		return unknown(fmt.Errorf("encode completion context: %w", err))
	}
	path, err := trustedPath(r.Root)
	if err != nil {
		return unknown(err)
	}
	root, err := filepath.Abs(r.Root)
	if err != nil {
		return unknown(fmt.Errorf("resolve completion root: %w", err))
	}
	observerCtx, cancel := context.WithTimeout(ctx, pollCommandTimeout)
	defer cancel()
	command := exec.Command("/bin/sh", "-c", declaration.CompletionCommand)
	command.Dir = root
	command.Stdin = bytes.NewReader(input)
	command.Stderr = log
	command.Env = append(childEnvironment(), "PATH="+path, "FOREST_ROOT="+root, "FOREST_RUN_ID="+record.RunID)
	data, err := processGroupOutput(observerCtx, command)
	if err != nil {
		return unknown(fmt.Errorf("completion command: %w", err))
	}
	completion, err := decodeRunCompletion(data)
	if err != nil {
		return unknown(err)
	}
	return completion
}
