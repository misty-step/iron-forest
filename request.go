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
	Schema string         `json:"schema"`
	ID     string         `json:"id"`
	Prompt string         `json:"prompt"`
	Work   *WorkReference `json:"work,omitempty"`
}

const maxRequestBytes = 1 << 20

var errRequestNoWork = errors.New("request selection returned no work")

func decodeRunRequest(data []byte) (*RunRequest, error) {
	if len(data) > maxRequestBytes {
		return nil, fmt.Errorf("request exceeds %d bytes", maxRequestBytes)
	}
	var request RunRequest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return nil, fmt.Errorf("parse request: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("request must contain exactly one JSON object")
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
	if request.Work != nil {
		work := *request.Work
		record.Work = &work
	}
}

func requestPrompt(standing string, request *RunRequest) string {
	if request == nil {
		return standing
	}
	return standing + "\n\n## Explicit Run request\n\n" + request.Prompt
}
