package main

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestConfigAndDeclarationValidation(t *testing.T) {
	root := t.TempDir()
	config := `repo: owner/name
agents:
  builder: {poll: "forest poll builder", interval: 5, timeout: 20}
checks:
  - {name: test, run: "go test ./..."}
`
	if err := os.WriteFile(filepath.Join(root, "forest.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "agents", "builder"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "agents", "builder", "agent.md"), []byte("---\nmodel: local/model\ntools: [git, read]\nthinking: low\n---\nSystem rules\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "agents", "builder", "task.md"), []byte("Select one item."), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(filepath.Join(root, "forest.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	declaration, err := loadDeclaration(root, "builder")
	if err != nil {
		t.Fatal(err)
	}
	if declaration.Model != "local/model" || !reflect.DeepEqual(declaration.Tools, []string{"git", "read"}) || declaration.SystemPrompt != "System rules\n" {
		t.Fatalf("declaration parsed incorrectly: %#v", declaration)
	}
	cfg.Checks = append(cfg.Checks, Check{Name: "test", Run: "again"})
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "duplicated") {
		t.Fatalf("expected duplicate check error, got %v", err)
	}
	bad := Config{Repo: "owner", Agents: map[string]AgentConfig{"builder": {Poll: "x", Interval: 1, Timeout: 1}}}
	if err := bad.Validate(); err == nil {
		t.Fatal("expected invalid repo error")
	}
}

func TestConfigYAMLIsStrictAndSingleDocument(t *testing.T) {
	const valid = `repo: owner/name
agents:
  builder: {poll: "forest poll builder", interval: 5, timeout: 20}
checks:
  - {name: test, run: "go test ./..."}
`
	tests := []struct {
		name string
		data string
		want string
	}{
		{name: "unknown top-level key", data: valid + "repository: owner/name\n", want: "field repository not found"},
		{name: "unknown nested key", data: strings.Replace(valid, "timeout: 20}", "timeout: 20, timeuot: 20}", 1), want: "field timeuot not found"},
		{name: "extra document", data: valid + "---\nrepo: owner/other\nagents:\n  builder: {poll: x, interval: 1, timeout: 1}\n", want: "multiple YAML documents"},
		{name: "boolean repo", data: strings.Replace(valid, "repo: owner/name", "repo: true", 1), want: "must be a YAML string scalar"},
		{name: "numeric agent name", data: strings.Replace(valid, "builder:", "1:", 1), want: "must be a YAML string scalar"},
		{name: "boolean poll", data: strings.Replace(valid, `"forest poll builder"`, "true", 1), want: "must be a YAML string scalar"},
		{name: "numeric check name", data: strings.Replace(valid, "name: test", "name: 1", 1), want: "must be a YAML string scalar"},
		{name: "fractional interval", data: strings.Replace(valid, "interval: 5", "interval: 1.5", 1), want: "must be a YAML integer scalar"},
		{name: "fractional timeout", data: strings.Replace(valid, "timeout: 20", "timeout: 20.9", 1), want: "must be a YAML integer scalar"},
		{name: "string interval", data: strings.Replace(valid, "interval: 5", `interval: "5"`, 1), want: "must be a YAML integer scalar"},
		{name: "mapping check command", data: strings.Replace(valid, `run: "go test ./..."`, "run: {command: test}", 1), want: "must be a YAML string scalar"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := decodeConfig([]byte(test.data), "forest.yaml"); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("decodeConfig() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestYAMLIntScalarBoundaries(t *testing.T) {
	max := ^uint(0) >> 1
	tests := []struct {
		name    string
		scalar  string
		want    int
		wantErr bool
	}{
		{name: "max int", scalar: strconv.FormatUint(uint64(max), 10), want: int(max)},
		{name: "max int plus one", scalar: strconv.FormatUint(uint64(max+1), 10), wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var got yamlInt
			err := yaml.Unmarshal([]byte(test.scalar), &got)
			if (err != nil) != test.wantErr {
				t.Fatalf("yaml.Unmarshal(%q) error = %v, wantErr %t", test.scalar, err, test.wantErr)
			}
			if int(got) != test.want {
				t.Fatalf("yaml.Unmarshal(%q) = %d, want %d", test.scalar, got, test.want)
			}
		})
	}
}

func TestDeclarationYAMLValidation(t *testing.T) {
	const validAgent = "---\nmodel: local/model\n---\nSystem rules\n"
	tests := []struct {
		name  string
		agent string
		task  string
		want  string
	}{
		{name: "unknown key", agent: "---\nmodel: local/model\nmodle: other/model\n---\nSystem rules\n", task: "Select one item.", want: "field modle not found"},
		{name: "missing model", agent: "---\nthinking: low\n---\nSystem rules\n", task: "Select one item.", want: "model is required"},
		{name: "empty task", agent: validAgent, task: " \n\t", want: "task.md is empty"},
		{name: "extra document", agent: "---\nmodel: local/model\n--- # appended document\nmodel: other/model\n---\nSystem rules\n", task: "Select one item.", want: "multiple YAML documents"},
		{name: "boolean model", agent: "---\nmodel: true\n---\nSystem rules\n", task: "Select one item.", want: "must be a YAML string scalar"},
		{name: "numeric thinking", agent: "---\nmodel: local/model\nthinking: 1\n---\nSystem rules\n", task: "Select one item.", want: "must be a YAML string scalar"},
		{name: "boolean tools scalar", agent: "---\nmodel: local/model\ntools: true\n---\nSystem rules\n", task: "Select one item.", want: "must be a YAML string scalar"},
		{name: "numeric tools scalar", agent: "---\nmodel: local/model\ntools: 1\n---\nSystem rules\n", task: "Select one item.", want: "must be a YAML string scalar"},
		{name: "null tools scalar", agent: "---\nmodel: local/model\ntools: null\n---\nSystem rules\n", task: "Select one item.", want: "must be a YAML string scalar"},
		{name: "mapping tools", agent: "---\nmodel: local/model\ntools: {git: true}\n---\nSystem rules\n", task: "Select one item.", want: "must be a YAML string scalar or sequence"},
		{name: "mixed tools sequence", agent: "---\nmodel: local/model\ntools: [git, true]\n---\nSystem rules\n", task: "Select one item.", want: "must be a YAML string scalar"},
		{name: "null thinking", agent: "---\nmodel: local/model\nthinking: null\n---\nSystem rules\n", task: "Select one item.", want: "must be a YAML string scalar"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "agents", "builder")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "agent.md"), []byte(test.agent), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "task.md"), []byte(test.task), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := loadDeclaration(root, "builder"); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("loadDeclaration() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestDurationFromSecondsBoundaries(t *testing.T) {
	type testCase struct {
		name    string
		seconds int
		want    time.Duration
		wantErr bool
	}
	tests := []testCase{
		{name: "minus one", seconds: -1, wantErr: true},
		{name: "zero", seconds: 0, wantErr: true},
		{name: "one", seconds: 1, want: time.Second},
	}
	if int64(^uint(0)>>1) >= maxDurationSeconds+1 {
		max := int(maxDurationSeconds)
		tests = append(tests,
			testCase{name: "max", seconds: max, want: time.Duration(maxDurationSeconds) * time.Second},
			testCase{name: "max plus one", seconds: max + 1, wantErr: true},
		)
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := durationFromSeconds(test.seconds)
			if (err != nil) != test.wantErr {
				t.Fatalf("durationFromSeconds(%d) error = %v, wantErr %t", test.seconds, err, test.wantErr)
			}
			if got != test.want {
				t.Fatalf("durationFromSeconds(%d) = %v, want %v", test.seconds, got, test.want)
			}
		})
	}
}

func TestLedgerAppendAndParse(t *testing.T) {
	root := t.TempDir()
	originalSync := ledgerSyncFile
	t.Cleanup(func() { ledgerSyncFile = originalSync })
	var syncOrder []string
	ledgerSyncFile = func(file *os.File) error {
		syncOrder = append(syncOrder, file.Name())
		return originalSync(file)
	}

	want := RunRecord{RunID: "run-1", Agent: "builder", Started: "2026-08-10T00:00:00Z", Duration: 1.5, Exit: 0, TokensIn: 3, TokensOut: 5, CacheRead: 7, CacheWrite: 11, Reasoning: 13}
	if err := AppendRun(root, want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(syncOrder, []string{root, ledgerPath(root), filepath.Dir(ledgerPath(root)), root}) {
		t.Fatalf("sync order=%v, want root, file, containing directory, root", syncOrder)
	}
	raw, err := os.ReadFile(ledgerPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(raw); !strings.HasSuffix(got, "\n") || strings.Count(got, "\n") != 1 {
		t.Fatalf("ledger bytes=%q", got)
	}
	rows, err := ReadLedger(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !reflect.DeepEqual(rows[0], want) {
		t.Fatalf("ledger mismatch: %#v", rows)
	}
}

func TestLedgerFirstAppendWithPrecreatedDirectorySyncsAncestors(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Dir(ledgerPath(root))
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	originalSync := ledgerSyncFile
	t.Cleanup(func() { ledgerSyncFile = originalSync })
	var syncOrder []string
	ledgerSyncFile = func(file *os.File) error {
		syncOrder = append(syncOrder, file.Name())
		return originalSync(file)
	}

	if err := AppendRun(root, RunRecord{RunID: "run-1", Agent: "builder"}); err != nil {
		t.Fatal(err)
	}
	wantOrder := []string{ledgerPath(root), dir, root}
	if !reflect.DeepEqual(syncOrder, wantOrder) {
		t.Fatalf("sync order=%v, want %v", syncOrder, wantOrder)
	}
}

func TestLedgerFirstAppendRootSyncFailureRestoresPrecreatedNamespace(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Dir(ledgerPath(root))
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	priorPath := filepath.Join(dir, "triggers.json")
	if err := os.WriteFile(priorPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	originalSync := ledgerSyncFile
	t.Cleanup(func() { ledgerSyncFile = originalSync })
	rootSyncErr := errors.New("repository root sync failed")
	var syncOrder []string
	ledgerSyncFile = func(file *os.File) error {
		syncOrder = append(syncOrder, file.Name())
		if file.Name() == root {
			return rootSyncErr
		}
		return originalSync(file)
	}

	err := AppendRun(root, RunRecord{RunID: "run-1", Agent: "builder"})
	if !errors.Is(err, rootSyncErr) {
		t.Fatalf("AppendRun() error=%v, want %v", err, rootSyncErr)
	}
	wantOrder := []string{ledgerPath(root), dir, root, ledgerPath(root), dir}
	if !reflect.DeepEqual(syncOrder, wantOrder) {
		t.Fatalf("sync order=%v, want %v", syncOrder, wantOrder)
	}
	requireLedgerState(t, root, nil, nil)
	rootEntries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(rootEntries) != 1 || rootEntries[0].Name() != workspaceName || !rootEntries[0].IsDir() {
		t.Fatalf("repository namespace=%v, want only pre-created %s directory", rootEntries, workspaceName)
	}
	forestEntries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(forestEntries) != 1 || forestEntries[0].Name() != "triggers.json" || forestEntries[0].IsDir() {
		t.Fatalf("forest namespace=%v, want only pre-existing triggers.json", forestEntries)
	}
	prior, err := os.ReadFile(priorPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(prior) != "{}\n" {
		t.Fatalf("pre-existing file=%q, want unchanged", prior)
	}
}

func TestLedgerAppendFailureRestoresPreviousLedger(t *testing.T) {
	originalWrite := ledgerWriteFile
	originalSync := ledgerSyncFile
	originalClose := ledgerCloseFile
	t.Cleanup(func() {
		ledgerWriteFile = originalWrite
		ledgerSyncFile = originalSync
		ledgerCloseFile = originalClose
	})

	tests := []struct {
		name   string
		inject func(string) []error
	}{
		{
			name: "short write",
			inject: func(string) []error {
				ledgerWriteFile = func(file *os.File, data []byte) (int, error) {
					return originalWrite(file, data[:len(data)/2])
				}
				return []error{io.ErrShortWrite}
			},
		},
		{
			name: "partial write error",
			inject: func(string) []error {
				writeErr := errors.New("partial write failed")
				ledgerWriteFile = func(file *os.File, data []byte) (int, error) {
					written, err := originalWrite(file, data[:len(data)/2])
					return written, errors.Join(err, writeErr)
				}
				return []error{writeErr, io.ErrShortWrite}
			},
		},
		{
			name: "sync and rollback sync errors",
			inject: func(root string) []error {
				appendSyncErr := errors.New("append sync failed")
				rollbackSyncErr := errors.New("rollback sync failed")
				syncCalls := 0
				ledgerSyncFile = func(file *os.File) error {
					if file.Name() == ledgerPath(root) {
						syncCalls++
						if syncCalls == 1 {
							return appendSyncErr
						}
						if syncCalls == 2 {
							return rollbackSyncErr
						}
					}
					return originalSync(file)
				}
				return []error{appendSyncErr, rollbackSyncErr}
			},
		},
		{
			name: "close and rollback close errors",
			inject: func(root string) []error {
				closeErr := errors.New("close failed")
				rollbackCloseErr := errors.New("rollback close failed")
				closeCalls := 0
				ledgerCloseFile = func(file *os.File) error {
					err := originalClose(file)
					if file.Name() == ledgerPath(root) {
						closeCalls++
						if closeCalls == 1 {
							return errors.Join(err, closeErr)
						}
						if closeCalls == 2 {
							return errors.Join(err, rollbackCloseErr)
						}
					}
					return err
				}
				return []error{closeErr, rollbackCloseErr}
			},
		},
	}
	previousStates := []struct {
		name string
		seed bool
	}{
		{name: "absent"},
		{name: "existing", seed: true},
	}
	for _, test := range tests {
		for _, previous := range previousStates {
			t.Run(test.name+"/"+previous.name, func(t *testing.T) {
				ledgerWriteFile = originalWrite
				ledgerSyncFile = originalSync
				ledgerCloseFile = originalClose
				t.Cleanup(func() {
					ledgerWriteFile = originalWrite
					ledgerSyncFile = originalSync
					ledgerCloseFile = originalClose
				})

				root := t.TempDir()
				if err := os.MkdirAll(filepath.Dir(ledgerPath(root)), 0o755); err != nil {
					t.Fatal(err)
				}
				first := RunRecord{RunID: "run-1", Agent: "builder", Started: "2026-08-10T00:00:00Z"}
				var before []byte
				var wantRows []RunRecord
				if previous.seed {
					if err := AppendRun(root, first); err != nil {
						t.Fatal(err)
					}
					var err error
					before, err = os.ReadFile(ledgerPath(root))
					if err != nil {
						t.Fatal(err)
					}
					wantRows = []RunRecord{first}
				}

				wantErrors := test.inject(root)
				err := AppendRun(root, RunRecord{RunID: "run-2", Agent: "builder", Started: "2026-08-10T00:01:00Z"})
				for _, wantErr := range wantErrors {
					if !errors.Is(err, wantErr) {
						t.Fatalf("AppendRun() error=%v, want %v", err, wantErr)
					}
				}
				requireLedgerState(t, root, before, wantRows)
			})
		}
	}
}

func TestLedgerFirstAppendDirectorySyncFailureRestoresAbsentLedger(t *testing.T) {
	root := t.TempDir()
	originalSync := ledgerSyncFile
	t.Cleanup(func() { ledgerSyncFile = originalSync })
	directorySyncErr := errors.New("directory sync failed")
	rollbackSyncErr := errors.New("namespace rollback sync failed")
	directorySyncCalls := 0
	ledgerSyncFile = func(file *os.File) error {
		if file.Name() == filepath.Dir(ledgerPath(root)) {
			directorySyncCalls++
			if directorySyncCalls == 1 {
				return directorySyncErr
			}
			return rollbackSyncErr
		}
		return originalSync(file)
	}

	err := AppendRun(root, RunRecord{RunID: "run-1", Agent: "builder"})
	if !errors.Is(err, directorySyncErr) || !errors.Is(err, rollbackSyncErr) {
		t.Fatalf("AppendRun() error=%v, want directory and rollback sync errors", err)
	}
	requireLedgerState(t, root, nil, nil)
}

func requireLedgerState(t *testing.T, root string, wantBytes []byte, wantRows []RunRecord) {
	t.Helper()
	path := ledgerPath(root)
	if wantBytes == nil {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("ledger path error=%v, want absent", err)
		}
	} else {
		gotBytes, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(gotBytes, wantBytes) {
			t.Fatalf("ledger bytes=%q, want %q", gotBytes, wantBytes)
		}
	}
	gotRows, err := ReadLedger(root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotRows, wantRows) {
		t.Fatalf("ledger rows=%#v, want %#v", gotRows, wantRows)
	}
}

func TestLedgerReadWaitsForFailedAppendRollback(t *testing.T) {
	root := t.TempDir()
	first := RunRecord{RunID: "run-1", Agent: "builder", Started: "2026-08-10T00:00:00Z"}
	if err := AppendRun(root, first); err != nil {
		t.Fatal(err)
	}

	originalWrite := ledgerWriteFile
	t.Cleanup(func() { ledgerWriteFile = originalWrite })
	writeStarted := make(chan struct{})
	releaseWrite := make(chan struct{})
	ledgerWriteFile = func(file *os.File, data []byte) (int, error) {
		written, err := originalWrite(file, data[:len(data)/2])
		close(writeStarted)
		<-releaseWrite
		return written, err
	}

	appendDone := make(chan error, 1)
	go func() {
		appendDone <- AppendRun(root, RunRecord{RunID: "run-2", Agent: "builder"})
	}()
	<-writeStarted

	type readResult struct {
		rows []RunRecord
		err  error
	}
	readerStarted := make(chan struct{})
	readDone := make(chan readResult, 1)
	go func() {
		close(readerStarted)
		rows, err := ReadLedger(root)
		readDone <- readResult{rows: rows, err: err}
	}()
	<-readerStarted

	var result readResult
	readCompleted := false
	select {
	case result = <-readDone:
		readCompleted = true
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseWrite)
	if err := <-appendDone; !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("AppendRun() error=%v, want %v", err, io.ErrShortWrite)
	}
	if readCompleted {
		t.Fatalf("ReadLedger() completed before rollback: rows=%#v error=%v", result.rows, result.err)
	}
	result = <-readDone
	if result.err != nil || !reflect.DeepEqual(result.rows, []RunRecord{first}) {
		t.Fatalf("ReadLedger() rows=%#v error=%v, want previous Ledger", result.rows, result.err)
	}
}

func TestLedgerCrossProcessReadWaitsForFailedSyncRollback(t *testing.T) {
	const readerRootEnv = "IRON_FOREST_TEST_LEDGER_READER_ROOT"
	if root := os.Getenv(readerRootEnv); root != "" {
		ready := os.NewFile(3, "ledger-reader-ready")
		result := os.NewFile(4, "ledger-reader-result")
		if ready == nil || result == nil {
			t.Fatal("missing reader process pipes")
		}
		if _, err := ready.Write([]byte{1}); err != nil {
			t.Fatal(err)
		}
		if err := ready.Close(); err != nil {
			t.Fatal(err)
		}
		rows, err := ReadLedger(root)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if _, err := io.WriteString(result, row.RunID+"\n"); err != nil {
				t.Fatal(err)
			}
		}
		if err := result.Close(); err != nil {
			t.Fatal(err)
		}
		return
	}

	root := t.TempDir()
	first := RunRecord{RunID: "run-1", Agent: "builder", Started: "2026-08-10T00:00:00Z"}
	if err := AppendRun(root, first); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(ledgerPath(root))
	if err != nil {
		t.Fatal(err)
	}

	originalWrite := ledgerWriteFile
	originalSync := ledgerSyncFile
	t.Cleanup(func() {
		ledgerWriteFile = originalWrite
		ledgerSyncFile = originalSync
	})
	ledgerWriteFile = func(file *os.File, data []byte) (int, error) {
		written, err := originalWrite(file, data[:len(data)/2])
		if err != nil {
			return written, err
		}
		return len(data), nil
	}
	syncFailure := errors.New("append sync failed")
	syncStarted := make(chan struct{})
	releaseSync := make(chan struct{})
	syncCalls := 0
	ledgerSyncFile = func(file *os.File) error {
		if file.Name() == ledgerPath(root) {
			syncCalls++
			if syncCalls == 1 {
				close(syncStarted)
				<-releaseSync
				return syncFailure
			}
		}
		return originalSync(file)
	}

	appendDone := make(chan error, 1)
	go func() {
		appendDone <- AppendRun(root, RunRecord{RunID: "run-2", Agent: "builder", Started: "2026-08-10T00:01:00Z"})
	}()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseSync) }) }
	writerDone := false
	defer func() {
		release()
		if !writerDone {
			<-appendDone
		}
	}()
	select {
	case <-syncStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("append did not reach Sync")
	}

	readyReader, readyWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readyReader.Close()
	resultReader, resultWriter, err := os.Pipe()
	if err != nil {
		readyWriter.Close()
		t.Fatal(err)
	}
	defer resultReader.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLedgerCrossProcessReadWaitsForFailedSyncRollback$")
	cmd.Env = append(os.Environ(), readerRootEnv+"="+root)
	cmd.ExtraFiles = []*os.File{readyWriter, resultWriter}
	var childOutput strings.Builder
	cmd.Stdout = &childOutput
	cmd.Stderr = &childOutput
	if err := cmd.Start(); err != nil {
		readyWriter.Close()
		resultWriter.Close()
		t.Fatal(err)
	}
	childWaited := false
	defer func() {
		if !childWaited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	if err := errors.Join(readyWriter.Close(), resultWriter.Close()); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(readyReader, make([]byte, 1)); err != nil {
		t.Fatalf("reader process readiness: %v", err)
	}

	type processReadResult struct {
		data []byte
		err  error
	}
	readDone := make(chan processReadResult, 1)
	go func() {
		data, err := io.ReadAll(resultReader)
		readDone <- processReadResult{data: data, err: err}
	}()
	var result processReadResult
	readCompleted := false
	select {
	case result = <-readDone:
		readCompleted = true
	case <-time.After(50 * time.Millisecond):
	}

	release()
	appendErr := <-appendDone
	writerDone = true
	if !readCompleted {
		result = <-readDone
	}
	waitErr := cmd.Wait()
	childWaited = true
	if readCompleted {
		t.Fatalf("reader process completed before rollback: result=%q error=%v output=%q", result.data, result.err, childOutput.String())
	}
	if !errors.Is(appendErr, syncFailure) {
		t.Fatalf("AppendRun() error=%v, want %v", appendErr, syncFailure)
	}
	if waitErr != nil {
		t.Fatalf("reader process: %v: %s", waitErr, childOutput.String())
	}
	if result.err != nil || string(result.data) != "run-1\n" {
		t.Fatalf("reader process result=%q error=%v, want previous Ledger", result.data, result.err)
	}
	requireLedgerState(t, root, before, []RunRecord{first})
}

func TestLedgerConcurrentAppendSerialization(t *testing.T) {
	root := t.TempDir()
	originalWrite := ledgerWriteFile
	t.Cleanup(func() { ledgerWriteFile = originalWrite })
	var stateMu sync.Mutex
	active := 0
	overlapped := false
	ledgerWriteFile = func(file *os.File, data []byte) (int, error) {
		stateMu.Lock()
		active++
		if active > 1 {
			overlapped = true
		}
		stateMu.Unlock()
		time.Sleep(time.Millisecond)
		written, err := originalWrite(file, data)
		stateMu.Lock()
		active--
		stateMu.Unlock()
		return written, err
	}

	const count = 16
	var wg sync.WaitGroup
	errs := make(chan error, count)
	for range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- AppendRun(root, RunRecord{RunID: "run", Agent: "builder"})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if overlapped {
		t.Fatal("ledger writes overlapped")
	}
	rows, err := ReadLedger(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != count {
		t.Fatalf("ledger rows=%d, want %d", len(rows), count)
	}
}

func schedulerHealth(scheduler *Scheduler, agent string) TriggerHealth {
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	return scheduler.health[agent]
}

func TestSchedulerSkipWhileRunningAndUnhealthy(t *testing.T) {
	root := t.TempDir()
	writeTestDeclaration(t, root, "builder")
	cfg := Config{Repo: "owner/name", Agents: map[string]AgentConfig{"builder": {Poll: "poll", Interval: 1, Timeout: 1}}}
	scheduler := NewScheduler(root, cfg, nil)
	var release = make(chan struct{})
	var once sync.Once
	scheduler.Poll = func(context.Context, string) PollResult { return PollResult{Code: 0} }
	scheduler.Run = func(context.Context, Declaration, int) (RunRecord, error) {
		once.Do(func() { <-release })
		return RunRecord{Started: "now"}, nil
	}
	dispatched, err := scheduler.Tick(context.Background(), "builder")
	if err != nil || !dispatched {
		t.Fatalf("first tick: dispatched=%v err=%v", dispatched, err)
	}
	deadline := time.Now().Add(time.Second)
	for !schedulerHealth(scheduler, "builder").Running && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	dispatched, err = scheduler.Tick(context.Background(), "builder")
	if err != nil || dispatched {
		t.Fatalf("busy tick: dispatched=%v err=%v", dispatched, err)
	}
	close(release)
	deadline = time.Now().Add(time.Second)
	for schedulerHealth(scheduler, "builder").Running && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	pollErr := errors.New("forge down")
	scheduler.Poll = func(context.Context, string) PollResult { return PollResult{Code: 2, Err: pollErr} }
	for range 2 {
		dispatched, err = scheduler.Tick(context.Background(), "builder")
		if dispatched || err == nil || !strings.Contains(err.Error(), pollErr.Error()) {
			t.Fatalf("failed Poll dispatched=%v err=%v", dispatched, err)
		}
	}
	health := schedulerHealth(scheduler, "builder")
	if health.ConsecutiveErrors != 2 || health.LastCode != 2 || health.LastError != pollErr.Error() {
		t.Fatalf("unhealthy Poll state=%#v", health)
	}
}

func TestSchedulerOnceRunsBeforeReturn(t *testing.T) {
	root := t.TempDir()
	writeTestDeclaration(t, root, "builder")
	cfg := Config{Repo: "owner/name", Agents: map[string]AgentConfig{"builder": {Poll: "poll", Interval: 1, Timeout: 1}}}
	scheduler := NewScheduler(root, cfg, nil)
	called := false
	scheduler.Poll = func(context.Context, string) PollResult { return PollResult{Code: 0} }
	scheduler.Run = func(context.Context, Declaration, int) (RunRecord, error) {
		called = true
		return RunRecord{Started: "now"}, nil
	}
	dispatched, err := scheduler.Once(context.Background(), "builder")
	if err != nil || !dispatched || !called {
		t.Fatalf("once dispatched=%v called=%v err=%v", dispatched, called, err)
	}
}

func TestBuilderPollMatrix(t *testing.T) {
	ctx := context.Background()
	sha := strings.Repeat("a", 40)
	cases := []struct {
		name   string
		issues string
		branch string
		ghErr  error
		gitErr error
		want   int
	}{
		{name: "empty tracker", issues: `[]`, want: 1},
		{name: "many unready", issues: `[[{"number":99,"pull_request":{}},{"number":100,"pull_request":{}}],[{"number":101,"pull_request":{}}]]`, want: 1},
		{name: "ready", issues: `[[{"number":4}]]`, want: 0},
		{name: "ready branch active", issues: `[[{"number":4}]]`, branch: sha + " refs/heads/forest/4-work\n", want: 1},
		{name: "malformed issue output", issues: `not json`, want: 2},
		{name: "malformed branch output", issues: `[[{"number":4}]]`, branch: "not a branch\n", want: 2},
		{name: "nested issue branch", issues: `[[{"number":4}]]`, branch: sha + " refs/heads/forest/4-work/nested\n", want: 2},
		{name: "forge outage", ghErr: errors.New("offline"), want: 2},
		{name: "git outage", issues: `[[{"number":4}]]`, gitErr: errors.New("git offline"), want: 2},
		{name: "timeout", ghErr: context.DeadlineExceeded, want: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &Poller{Root: t.TempDir(), Repo: "owner/name"}
			p.Run = func(_ context.Context, name string, args ...string) ([]byte, error) {
				if name == "gh" {
					if !hasArg(args, "--paginate") || !hasArg(args, "--slurp") {
						t.Fatalf("pagination args missing: %v", args)
					}
					if args[len(args)-1] != "repos/owner/name/issues?state=open&labels=forest%3Aready&per_page=100" {
						t.Fatalf("repository or label args missing: %v", args)
					}
					return []byte(tc.issues), tc.ghErr
				}
				return []byte(tc.branch), tc.gitErr
			}
			if got := p.builder(ctx); got != tc.want {
				t.Fatalf("poll exit=%d want %d", got, tc.want)
			}
		})
	}
}

func TestConcurrentPollsPreserveCanonicalNotesAcrossLinkedWorktrees(t *testing.T) {
	root, _ := testClone(t)
	master := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	runGitDir(t, root, "checkout", "-b", "forest/4-work")
	if err := os.WriteFile(filepath.Join(root, "poll-change"), []byte("change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "add", "poll-change")
	runGitDir(t, root, "commit", "-m", "poll change")
	tip := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	runGitDir(t, root, "push", "origin", "HEAD:refs/heads/forest/4-work")
	addNote(t, root, forestNoteRefs[0], tip, pollReviewNote(tip), "Iron Forest Builder", "builder@forest.invalid")
	runGitDir(t, root, "push", "origin", forestNoteRefs[0]+":"+forestNoteRefs[0])

	addNote(t, root, forestNoteRefs[1], master, pollChecksNote(master, `{"name":"test","ok":true,"exit":0}`), "Iron Forest Verifier", "verifier@forest.invalid")
	addNote(t, root, forestNoteRefs[2], master, pollVerdictNote(master, "changes"), "Iron Forest Verifier", "verifier@forest.invalid")
	runGitDir(t, root, "push", "origin", forestNoteRefs[1]+":"+forestNoteRefs[1], forestNoteRefs[2]+":"+forestNoteRefs[2])
	var before [len(forestNoteRefs)]string
	for index, ref := range forestNoteRefs {
		before[index] = strings.TrimSpace(string(runGitDir(t, root, "rev-parse", ref)))
	}

	var linked [2]string
	for index := range linked {
		linked[index] = filepath.Join(t.TempDir(), "linked")
		runGitDir(t, root, "worktree", "add", "--detach", linked[index], master)
	}

	var fetchMu sync.Mutex
	ready := make(chan struct{}, len(linked))
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	var fetched [2][]string
	var cleaned [2][]string
	results := make(chan int, len(linked))
	for index, worktree := range linked {
		index := index
		poller := NewPoller(worktree, "owner/name")
		run := poller.Run
		poller.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if hasArg(args, "fetch") {
				fetchMu.Lock()
				output, err := run(ctx, name, args...)
				fetchMu.Unlock()
				if err == nil {
					for _, arg := range args {
						refspec := strings.SplitN(arg, ":", 2)
						if len(refspec) == 2 && forestNoteRefIndex(refspec[0]) >= 0 && strings.HasPrefix(refspec[1], pollNotesNamespace+"/") {
							fetched[index] = append(fetched[index], refspec[1])
						}
					}
				}
				return output, err
			}
			output, err := run(ctx, name, args...)
			if err != nil {
				return output, err
			}
			if hasArg(args, "rev-parse") && strings.HasSuffix(args[len(args)-1], "/verdict") {
				ready <- struct{}{}
				<-release
			}
			if hasArg(args, "update-ref") && hasArg(args, "-d") {
				cleaned[index] = append(cleaned[index], args[len(args)-1])
			}
			return output, nil
		}
		go func() {
			results <- poller.verifier(context.Background())
		}()
	}

	for range linked {
		select {
		case <-ready:
		case got := <-results:
			t.Fatalf("Poll finished with exit %d before both snapshots were ready", got)
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for overlapping Poll snapshots")
		}
	}
	liveOutput := runGitDir(t, root, "for-each-ref", "--format=%(refname)", pollNotesNamespace+"/")
	live := make(map[string]bool)
	for _, ref := range strings.Fields(string(liveOutput)) {
		live[ref] = true
	}
	if len(live) != len(linked)*len(forestNoteRefs) {
		t.Fatalf("live private refs=%v want %d", live, len(linked)*len(forestNoteRefs))
	}
	owner := make(map[string]int, len(live))
	for index, refs := range fetched {
		if len(refs) != len(forestNoteRefs) {
			t.Fatalf("Poll %d fetched refs=%v want %d", index, refs, len(forestNoteRefs))
		}
		for _, ref := range refs {
			if other, exists := owner[ref]; exists {
				t.Fatalf("private ref %s shared by Polls %d and %d", ref, other, index)
			}
			if !live[ref] {
				t.Fatalf("Poll %d private ref %s is not live at barrier", index, ref)
			}
			owner[ref] = index
		}
	}

	close(release)
	released = true
	for range linked {
		select {
		case got := <-results:
			if got != 0 {
				t.Fatalf("poll exit=%d want 0", got)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for concurrent Poll")
		}
	}
	for index := range cleaned {
		if !reflect.DeepEqual(cleaned[index], fetched[index]) {
			t.Fatalf("Poll %d cleanup refs=%v want %v", index, cleaned[index], fetched[index])
		}
	}
	for index, ref := range forestNoteRefs {
		after := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", ref)))
		if after != before[index] {
			t.Fatalf("%s changed from %s to %s", ref, before[index], after)
		}
	}
	if refs := strings.TrimSpace(string(runGitDir(t, root, "for-each-ref", "--format=%(refname)", pollNotesNamespace+"/"))); refs != "" {
		t.Fatalf("private Poll refs remain: %s", refs)
	}
}

func TestPollRejectsDuplicateNoteBlobFromWrongTargetWriter(t *testing.T) {
	root, _ := testClone(t)
	master := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	runGitDir(t, root, "checkout", "-b", "forest/4-work")
	if err := os.WriteFile(filepath.Join(root, "poll-change"), []byte("change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "add", "poll-change")
	runGitDir(t, root, "commit", "-m", "poll change")
	tip := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	runGitDir(t, root, "push", "origin", "HEAD:refs/heads/forest/4-work")

	payload := pollReviewNote(tip)
	addNote(t, root, forestNoteRefs[0], tip, payload, "Operator", "operator@example.invalid")
	addNote(t, root, forestNoteRefs[0], master, payload, "Iron Forest Builder", "builder@forest.invalid")
	runGitDir(t, root, "push", "origin", forestNoteRefs[0]+":"+forestNoteRefs[0])

	if got := NewPoller(root, "owner/name").verifier(context.Background()); got != 2 {
		t.Fatalf("poll exit=%d want 2", got)
	}
}

func TestExactGitLine(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "LF preserves actor bytes", input: " Iron Forest Builder\x00builder@forest.invalid \n", want: " Iron Forest Builder\x00builder@forest.invalid "},
		{name: "CRLF", input: "Iron Forest Builder\x00builder@forest.invalid\r\n", want: "Iron Forest Builder\x00builder@forest.invalid"},
		{name: "missing terminator", input: "Iron Forest Builder\x00builder@forest.invalid", wantErr: true},
		{name: "second record", input: "Iron Forest Builder\x00builder@forest.invalid\nother\n", wantErr: true},
		{name: "embedded CR", input: "Iron Forest\rBuilder\x00builder@forest.invalid\n", wantErr: true},
		{name: "extra CR", input: "Iron Forest Builder\x00builder@forest.invalid\r\r\n", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := exactGitLine([]byte(tc.input))
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("exactGitLine=%q err=%v, want %q err=%v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

type notePollCase struct {
	name              string
	branches          string
	noteRefs          string
	reAdvertised      string
	reAdvertisedNotes string
	review            string
	verdict           string
	reviewIdentity    string
	verdictIdentity   string
	noteTarget        string
	notePaths         string
	fetchedOID        string
	branchErr         error
	reAdvertiseErr    error
	fetchErr          error
	actualFetchErr    error
	revParseErr       error
	reviewErr         error
	verdictErr        error
	treeErr           error
	logErr            error
	missingReview     bool
	missingVerdict    bool
	deletedRefs       *[]string
	want              int
}

func pollReviewNote(sha string) string {
	return pollReviewNoteBranch(sha, "forest/4-work")
}

func pollReviewNoteBranch(sha, branch string) string {
	return `{"schema":"forest.review-request.v1","issue":4,"branch":"` + branch + `","revision":"` + sha + `","time":"2026-08-10T00:00:00Z"}`
}

func pollVerdictNote(sha, verdict string) string {
	return `{"schema":"forest.verdict.v1","revision":"` + sha + `","verdict":"` + verdict + `","summary":"done","time":"2026-08-10T00:00:00Z"}`
}

func missingPollNote(t *testing.T) error {
	t.Helper()
	command := exec.Command("sh", "-c", "exit 1")
	if err := command.Run(); err == nil {
		t.Fatal("expected note lookup failure")
	} else {
		return err
	}
	return nil
}

func notePoller(t *testing.T, tc notePollCase) *Poller {
	t.Helper()
	privateCanonical := make(map[string]string)
	completedReads := make(map[string]bool)
	readTargets := make(map[string]string)
	allNoteRefs := func(args []string) bool {
		for _, ref := range forestNoteRefs {
			if !hasArg(args, ref) {
				return false
			}
		}
		return true
	}
	canonicalForArgs := func(args []string) string {
		for _, arg := range args {
			ref := strings.TrimPrefix(arg, "--ref=")
			for _, canonical := range forestNoteRefs {
				suffix := "/" + strings.TrimPrefix(canonical, "refs/notes/forest/")
				if strings.HasPrefix(ref, pollNotesNamespace+"/") && strings.HasSuffix(ref, suffix) {
					return canonical
				}
			}
		}
		return ""
	}
	advertisedOID := func(ref string) string {
		for _, line := range strings.Split(strings.TrimSpace(tc.noteRefs), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 2 && fields[1] == ref {
				return fields[0]
			}
		}
		return ""
	}

	p := &Poller{Root: t.TempDir()}
	p.Run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "git" {
			t.Fatalf("unexpected tool %q", name)
		}
		switch {
		case hasArg(args, "refs/heads/forest/*"):
			return []byte(tc.branches), tc.branchErr
		case hasArg(args, "refs/heads/forest/4-work"):
			if !allNoteRefs(args) {
				t.Fatalf("final confirmation omits note refs: %v", args)
			}
			if !completedReads[forestNoteRefs[0]] || !completedReads[forestNoteRefs[2]] {
				t.Fatalf("final confirmation preceded selected note reads: %v", completedReads)
			}
			branch := tc.reAdvertised
			if branch == "" {
				branch = tc.branches
			}
			notes := tc.reAdvertisedNotes
			if notes == "" {
				notes = tc.noteRefs
			}
			return []byte(branch + notes), tc.reAdvertiseErr
		case hasArg(args, "ls-remote") && allNoteRefs(args):
			return []byte(tc.noteRefs), tc.fetchErr
		case hasArg(args, "fetch"):
			for _, canonical := range forestNoteRefs {
				prefix := canonical + ":"
				for _, arg := range args {
					if strings.HasPrefix(arg, prefix) {
						private := strings.TrimPrefix(arg, prefix)
						if !strings.HasPrefix(private, pollNotesNamespace+"/") {
							t.Fatalf("fetch destination is not private: %v", args)
						}
						privateCanonical[private] = canonical
						return nil, tc.actualFetchErr
					}
				}
			}
			t.Fatalf("fetch does not use a canonical-to-private refspec: %v", args)
			return nil, nil
		case hasArg(args, "rev-parse"):
			canonical := privateCanonical[args[len(args)-1]]
			oid := advertisedOID(canonical)
			if tc.fetchedOID != "" {
				oid = tc.fetchedOID
			}
			return []byte(oid + "\n"), tc.revParseErr
		case hasArg(args, "update-ref"):
			ref := args[len(args)-1]
			if !hasArg(args, "-d") || !strings.HasPrefix(ref, pollNotesNamespace+"/") {
				t.Fatalf("Poll deleted non-private ref: %v", args)
			}
			if tc.deletedRefs != nil {
				*tc.deletedRefs = append(*tc.deletedRefs, ref)
			}
			return nil, nil
		case hasArg(args, "show"):
			canonical := canonicalForArgs(args)
			readTargets[canonical] = args[len(args)-1]
			switch canonical {
			case forestNoteRefs[0]:
				if tc.reviewErr != nil {
					return nil, tc.reviewErr
				}
				if tc.missingReview {
					completedReads[canonical] = true
					return nil, missingPollNote(t)
				}
				return []byte(tc.review), nil
			case forestNoteRefs[2]:
				if tc.verdictErr != nil {
					return nil, tc.verdictErr
				}
				if tc.missingVerdict {
					completedReads[canonical] = true
					return nil, missingPollNote(t)
				}
				return []byte(tc.verdict), nil
			default:
				t.Fatalf("unexpected note ref in show: %v", args)
				return nil, nil
			}
		case hasArg(args, "ls-tree"):
			if tc.treeErr != nil {
				return nil, tc.treeErr
			}
			if tc.notePaths != "" {
				return []byte(tc.notePaths), nil
			}
			target := tc.noteTarget
			if target == "" {
				target = strings.Fields(tc.branches)[0]
			}
			return []byte(target[:2] + "/" + target[2:] + "\n"), nil
		case hasArg(args, "log"):
			if tc.logErr != nil {
				return nil, tc.logErr
			}
			canonical := canonicalForArgs(args)
			target := readTargets[canonical]
			if !hasArg(args, "--") || canonical == "" || !isSHA(target) || args[len(args)-1] != target[:2]+"/"+target[2:] {
				t.Fatalf("identity lookup does not use the exact private-ref target path: %v", args)
			}
			completedReads[canonical] = true
			if canonical == forestNoteRefs[0] {
				if tc.reviewIdentity != "" {
					return []byte(tc.reviewIdentity), nil
				}
				return []byte("Iron Forest Builder\x00builder@forest.invalid\n"), nil
			}
			if tc.verdictIdentity != "" {
				return []byte(tc.verdictIdentity), nil
			}
			return []byte("Iron Forest Verifier\x00verifier@forest.invalid\n"), nil
		default:
			t.Fatalf("unexpected git args: %v", args)
			return nil, nil
		}
	}
	return p
}

func assertPollPrivateCleanup(t *testing.T, tc notePollCase, refs []string) {
	t.Helper()
	want := tc.branchErr == nil && strings.Contains(tc.branches, " refs/heads/forest/4-work")
	if !want {
		if len(refs) != 0 {
			t.Fatalf("deleted refs=%v want none", refs)
		}
		return
	}
	if len(refs) != len(forestNoteRefs) {
		t.Fatalf("deleted refs=%v want %d private refs", refs, len(forestNoteRefs))
	}
	seen := make(map[string]bool, len(refs))
	for _, ref := range refs {
		if !strings.HasPrefix(ref, pollNotesNamespace+"/") || seen[ref] {
			t.Fatalf("deleted refs are not unique private refs: %v", refs)
		}
		seen[ref] = true
	}
}

func TestPollTransportErrorsPreserveObservableIdentity(t *testing.T) {
	sha := strings.Repeat("d", 40)
	sentinel := errors.New("transport sentinel")
	cases := []struct {
		name string
		call func(*Poller) error
	}{
		{name: "branch lookup", call: func(p *Poller) error {
			_, err := p.branchTips(context.Background())
			return err
		}},
		{name: "review read", call: func(p *Poller) error {
			_, err := p.coordinationNote(context.Background(), forestNoteRefs[0], sha, "builder", "fixer")
			return err
		}},
		{name: "verdict read", call: func(p *Poller) error {
			_, err := p.coordinationNote(context.Background(), forestNoteRefs[2], sha, "verifier")
			return err
		}},
		{name: "final advertisement", call: func(p *Poller) error {
			return p.confirmSnapshot(context.Background(), branchTip{Name: "forest/4-work", SHA: sha}, pollNoteSnapshot{})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			poller := &Poller{Root: t.TempDir(), Run: func(context.Context, string, ...string) ([]byte, error) {
				return nil, sentinel
			}}
			if err := tc.call(poller); !errors.Is(err, sentinel) {
				t.Fatalf("error=%v, want transport sentinel", err)
			}
		})
	}

	t.Run("actual fetch", func(t *testing.T) {
		deleted := []string{}
		tc := notePollCase{
			branches:       sha + " refs/heads/forest/4-work\n",
			noteRefs:       sha + " refs/notes/forest/review-request\n",
			actualFetchErr: sentinel,
			deletedRefs:    &deleted,
		}
		_, err := notePoller(t, tc).fetchNotes(context.Background())
		if !errors.Is(err, sentinel) {
			t.Fatalf("error=%v, want transport sentinel", err)
		}
		assertPollPrivateCleanup(t, tc, deleted)
	})
}

func TestVerifierPollMatrix(t *testing.T) {
	sha := strings.Repeat("b", 40)
	validBranch := sha + " refs/heads/forest/4-work\n"
	cases := []notePollCase{
		{name: "empty", want: 1},
		{name: "no review request", branches: validBranch, missingReview: true, want: 1},
		{name: "eligible", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n", review: pollReviewNote(sha), missingVerdict: true, want: 0},
		{name: "eligible Fixer request", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n", review: pollReviewNote(sha), reviewIdentity: "Iron Forest Fixer\x00fixer@forest.invalid\n", missingVerdict: true, want: 0},
		{name: "duplicate note paths", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n", review: pollReviewNote(sha), notePaths: sha + "\n" + sha[:2] + "/" + sha[2:] + "\n", missingVerdict: true, want: 2},
		{name: "branch moved after notes", branches: validBranch, reAdvertised: strings.Repeat("e", 40) + " refs/heads/forest/4-work\n", noteRefs: sha + " refs/notes/forest/review-request\n", review: pollReviewNote(sha), missingVerdict: true, want: 2},
		{name: "note moved after reads", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n", reAdvertisedNotes: strings.Repeat("e", 40) + " refs/notes/forest/review-request\n", review: pollReviewNote(sha), missingVerdict: true, want: 2},
		{name: "verdict appeared after reads", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n", reAdvertisedNotes: sha + " refs/notes/forest/review-request\n" + sha + " refs/notes/forest/verdict\n", review: pollReviewNote(sha), missingVerdict: true, want: 2},
		{name: "advertised note moved during fetch", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n", fetchedOID: strings.Repeat("e", 40), want: 2},
		{name: "malformed branch advertisement", branches: validBranch, reAdvertised: "not a branch\n", noteRefs: sha + " refs/notes/forest/review-request\n", review: pollReviewNote(sha), missingVerdict: true, want: 2},
		{name: "wrong request branch", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n", review: pollReviewNoteBranch(sha, "forest/4-other"), missingVerdict: true, want: 2},
		{name: "wrong request identity", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n", review: pollReviewNote(sha), reviewIdentity: "Builder\x00builder@forest.invalid\n", missingVerdict: true, want: 2},
		{name: "padded request author", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n", review: pollReviewNote(sha), reviewIdentity: " Iron Forest Builder\x00builder@forest.invalid\n", missingVerdict: true, want: 2},
		{name: "wrong verdict identity", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n" + sha + " refs/notes/forest/verdict\n", review: pollReviewNote(sha), verdict: pollVerdictNote(sha, "approve"), verdictIdentity: "Operator\x00operator@example.invalid\n", want: 2},
		{name: "padded verdict email", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n" + sha + " refs/notes/forest/verdict\n", review: pollReviewNote(sha), verdict: pollVerdictNote(sha, "approve"), verdictIdentity: "Iron Forest Verifier\x00verifier@forest.invalid \n", want: 2},
		{name: "wrong note target", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n", review: pollReviewNote(sha), noteTarget: strings.Repeat("e", 40), missingVerdict: true, want: 2},
		{name: "malformed ref", branches: sha + " refs/heads/forest/bad\n", want: 2},
		{name: "malformed note", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n", review: `{}`, missingVerdict: true, want: 2},
		{name: "post-show tree exit 1", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n", review: pollReviewNote(sha), treeErr: missingPollNote(t), want: 2},
		{name: "post-show log exit 1", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n", review: pollReviewNote(sha), logErr: missingPollNote(t), want: 2},
		{name: "outage", branchErr: errors.New("offline"), want: 2},
		{name: "note fetch failure", branches: validBranch, fetchErr: errors.New("fetch failed"), want: 2},
		{name: "actual note fetch failure", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n", actualFetchErr: errors.New("actual fetch failed"), want: 2},
		{name: "fetched note lookup failure", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n", revParseErr: errors.New("fetched note lookup failed"), want: 2},
		{name: "review transport failure", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n", reviewErr: errors.New("review read failed"), want: 2},
		{name: "joined canceled note read", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n", reviewErr: errors.Join(missingPollNote(t), context.Canceled), want: 2},
		{name: "verdict transport failure", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n" + sha + " refs/notes/forest/verdict\n", review: pollReviewNote(sha), verdictErr: errors.New("verdict read failed"), want: 2},
		{name: "final advertisement failure", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n", review: pollReviewNote(sha), missingVerdict: true, reAdvertiseErr: errors.New("re-advertisement failed"), want: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deleted := []string{}
			tc.deletedRefs = &deleted
			if got := notePoller(t, tc).verifier(context.Background()); got != tc.want {
				t.Fatalf("poll exit=%d want %d", got, tc.want)
			}
			assertPollPrivateCleanup(t, tc, deleted)
		})
	}
}

func TestFixerPollMatrix(t *testing.T) {
	sha := strings.Repeat("c", 40)
	validBranch := sha + " refs/heads/forest/4-work\n"
	cases := []notePollCase{
		{name: "empty", want: 1},
		{name: "no verdict", branches: validBranch, missingVerdict: true, want: 1},
		{name: "eligible", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n" + sha + " refs/notes/forest/verdict\n", review: pollReviewNote(sha), verdict: pollVerdictNote(sha, "changes"), want: 0},
		{name: "eligible Fixer request", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n" + sha + " refs/notes/forest/verdict\n", review: pollReviewNote(sha), reviewIdentity: "Iron Forest Fixer\x00fixer@forest.invalid\n", verdict: pollVerdictNote(sha, "changes"), want: 0},
		{name: "branch moved after notes", branches: validBranch, reAdvertised: strings.Repeat("e", 40) + " refs/heads/forest/4-work\n", noteRefs: sha + " refs/notes/forest/review-request\n" + sha + " refs/notes/forest/verdict\n", review: pollReviewNote(sha), verdict: pollVerdictNote(sha, "changes"), want: 2},
		{name: "malformed branch advertisement", branches: validBranch, reAdvertised: "not a branch\n", noteRefs: sha + " refs/notes/forest/review-request\n" + sha + " refs/notes/forest/verdict\n", review: pollReviewNote(sha), verdict: pollVerdictNote(sha, "changes"), want: 2},
		{name: "wrong request branch", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n" + sha + " refs/notes/forest/verdict\n", review: pollReviewNoteBranch(sha, "forest/4-other"), verdict: pollVerdictNote(sha, "changes"), want: 2},
		{name: "wrong request identity", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n" + sha + " refs/notes/forest/verdict\n", review: pollReviewNote(sha), verdict: pollVerdictNote(sha, "changes"), reviewIdentity: "Builder\x00builder@forest.invalid\n", want: 2},
		{name: "padded request email", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n" + sha + " refs/notes/forest/verdict\n", review: pollReviewNote(sha), verdict: pollVerdictNote(sha, "changes"), reviewIdentity: "Iron Forest Builder\x00builder@forest.invalid \n", want: 2},
		{name: "wrong verdict identity", branches: validBranch, noteRefs: sha + " refs/notes/forest/verdict\n", verdict: pollVerdictNote(sha, "changes"), verdictIdentity: "Operator\x00operator@example.invalid\n", want: 2},
		{name: "padded verdict author", branches: validBranch, noteRefs: sha + " refs/notes/forest/verdict\n", verdict: pollVerdictNote(sha, "changes"), verdictIdentity: " Iron Forest Verifier\x00verifier@forest.invalid\n", want: 2},
		{name: "wrong note target", branches: validBranch, noteRefs: sha + " refs/notes/forest/verdict\n", verdict: pollVerdictNote(sha, "changes"), noteTarget: strings.Repeat("e", 40), want: 2},
		{name: "malformed ref", branches: sha + " refs/heads/forest/bad\n", want: 2},
		{name: "malformed note", branches: validBranch, noteRefs: sha + " refs/notes/forest/verdict\n", verdict: `{}`, want: 2},
		{name: "post-show tree exit 1", branches: validBranch, noteRefs: sha + " refs/notes/forest/verdict\n", verdict: pollVerdictNote(sha, "changes"), treeErr: missingPollNote(t), want: 2},
		{name: "absent remote ref stays local", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n" + sha + " refs/notes/forest/verdict\n", review: pollReviewNote(sha), verdict: pollVerdictNote(sha, "changes"), want: 0},
		{name: "outage", branchErr: errors.New("offline"), want: 2},
		{name: "note fetch failure", branches: validBranch, fetchErr: errors.New("fetch failed"), want: 2},
		{name: "actual note fetch failure", branches: validBranch, noteRefs: sha + " refs/notes/forest/verdict\n", actualFetchErr: errors.New("actual fetch failed"), want: 2},
		{name: "fetched note lookup failure", branches: validBranch, noteRefs: sha + " refs/notes/forest/verdict\n", revParseErr: errors.New("fetched note lookup failed"), want: 2},
		{name: "review transport failure", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n" + sha + " refs/notes/forest/verdict\n", reviewErr: errors.New("review read failed"), verdict: pollVerdictNote(sha, "changes"), want: 2},
		{name: "verdict transport failure", branches: validBranch, noteRefs: sha + " refs/notes/forest/verdict\n", verdictErr: errors.New("verdict read failed"), want: 2},
		{name: "final advertisement failure", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n" + sha + " refs/notes/forest/verdict\n", review: pollReviewNote(sha), verdict: pollVerdictNote(sha, "changes"), reAdvertiseErr: errors.New("re-advertisement failed"), want: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deleted := []string{}
			tc.deletedRefs = &deleted
			if got := notePoller(t, tc).fixer(context.Background()); got != tc.want {
				t.Fatalf("poll exit=%d want %d", got, tc.want)
			}
			assertPollPrivateCleanup(t, tc, deleted)
		})
	}
}

func TestPollPrivateCleanupUsesFreshContext(t *testing.T) {
	sha := strings.Repeat("a", 40)
	for _, failCleanup := range []bool{false, true} {
		name := "success"
		want := 1
		if failCleanup {
			name = "cleanup error"
			want = 2
		}
		t.Run(name, func(t *testing.T) {
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			poller := notePoller(t, notePollCase{
				branches: sha + " refs/heads/forest/4-work\n",
				noteRefs: sha + " refs/notes/forest/review-request\n" + sha + " refs/notes/forest/verdict\n",
				review:   pollReviewNote(sha),
				verdict:  pollVerdictNote(sha, "approve"),
			})
			run := poller.Run
			reads := 0
			cleanupCalls := 0
			cleanupErr := errors.New("cleanup failed")
			poller.Run = func(ctx context.Context, tool string, args ...string) ([]byte, error) {
				if hasArg(args, "update-ref") {
					if err := ctx.Err(); err != nil {
						t.Fatalf("cleanup context is already done: %v", err)
					}
					if parent.Err() != context.Canceled {
						t.Fatalf("Poll parent error=%v want canceled", parent.Err())
					}
					deadline, ok := ctx.Deadline()
					if !ok {
						t.Fatal("cleanup context has no deadline")
					}
					if remaining := time.Until(deadline); remaining <= 0 || remaining > time.Second {
						t.Fatalf("cleanup deadline remaining=%v want within one second", remaining)
					}
					cleanupCalls++
					if failCleanup && cleanupCalls == 1 {
						return nil, cleanupErr
					}
				}
				output, err := run(ctx, tool, args...)
				if err == nil && hasArg(args, "log") {
					reads++
					if reads == 2 {
						cancel()
					}
				}
				return output, err
			}
			if got := poller.verifier(parent); got != want {
				t.Fatalf("poll exit=%d want %d", got, want)
			}
			if reads != 2 {
				t.Fatalf("completed note reads=%d want 2", reads)
			}
			if cleanupCalls != len(forestNoteRefs) {
				t.Fatalf("cleanup calls=%d want %d", cleanupCalls, len(forestNoteRefs))
			}
		})
	}
}

func pollChecksNote(sha, result string) string {
	return `{"schema":"forest.checks.v1","revision":"` + sha + `","results":[` + result + `],"time":"2026-08-10T00:00:00Z"}`
}

func TestPollNoteDecodersRejectStrictJSON(t *testing.T) {
	sha := strings.Repeat("e", 40)
	validReview := pollReviewNote(sha)
	validChecks := pollChecksNote(sha, `{"name":"test","ok":true,"exit":0}`)
	validVerdict := pollVerdictNote(sha, "approve")
	decodeReviewNote := func(data []byte) error { _, err := decodeReview(data, sha); return err }
	decodeChecksNote := func(data []byte) error { _, err := decodeChecks(data, sha); return err }
	decodeVerdictNote := func(data []byte) error { _, err := decodeVerdict(data, sha); return err }
	if err := decodeChecksNote([]byte(validChecks)); err != nil {
		t.Fatalf("valid checks: %v", err)
	}

	objects := []struct {
		name   string
		data   string
		keys   []string
		decode func([]byte) error
	}{
		{name: "review", data: validReview, keys: []string{"schema", "issue", "branch", "revision", "time"}, decode: decodeReviewNote},
		{name: "checks", data: validChecks, keys: []string{"schema", "revision", "results", "time", "name", "ok", "exit"}, decode: decodeChecksNote},
		{name: "verdict", data: validVerdict, keys: []string{"schema", "revision", "verdict", "summary", "time"}, decode: decodeVerdictNote},
	}
	for _, object := range objects {
		for _, key := range object.keys {
			alias := strings.ToUpper(key[:1]) + key[1:]
			data := strings.Replace(object.data, `"`+key+`":`, `"`+alias+`":`, 1)
			t.Run(object.name+" "+key+" alias", func(t *testing.T) {
				if err := object.decode([]byte(data)); err == nil {
					t.Fatalf("accepted case-folded alias %q", alias)
				}
			})
		}
	}

	missingReview := `{"schema":"forest.review-request.v1","issue":4,"branch":"forest/4-work","revision":"` + sha + `"}`
	missingChecks := `{"schema":"forest.checks.v1","revision":"` + sha + `","time":"2026-08-10T00:00:00Z"}`
	missingVerdict := `{"schema":"forest.verdict.v1","revision":"` + sha + `","verdict":"approve","time":"2026-08-10T00:00:00Z"}`
	nullResults := strings.Replace(validChecks, `"results":[{"name":"test","ok":true,"exit":0}]`, `"results":null`, 1)
	cases := []struct {
		name   string
		data   string
		decode func([]byte) error
	}{
		{name: "review unknown member", data: strings.Replace(validReview, `,"time":`, `,"extra":true,"time":`, 1), decode: decodeReviewNote},
		{name: "checks unknown nested member", data: pollChecksNote(sha, `{"name":"test","ok":true,"exit":0,"extra":true}`), decode: decodeChecksNote},
		{name: "verdict unknown member", data: strings.Replace(validVerdict, `,"time":`, `,"extra":true,"time":`, 1), decode: decodeVerdictNote},
		{name: "review duplicate member", data: `{"schema":"forest.review-request.v1","schema":"forest.review-request.v1","issue":4,"branch":"forest/4-work","revision":"` + sha + `","time":"2026-08-10T00:00:00Z"}`, decode: decodeReviewNote},
		{name: "checks duplicate nested member", data: pollChecksNote(sha, `{"name":"test","name":"test","ok":true,"exit":0}`), decode: decodeChecksNote},
		{name: "verdict duplicate member", data: `{"schema":"forest.verdict.v1","revision":"` + sha + `","verdict":"approve","summary":"done","summary":"done","time":"2026-08-10T00:00:00Z"}`, decode: decodeVerdictNote},
		{name: "review mixed-case duplicate", data: strings.Replace(validReview, `"schema":"forest.review-request.v1"`, `"schema":"bad","Schema":"forest.review-request.v1"`, 1), decode: decodeReviewNote},
		{name: "check mixed-case duplicate", data: pollChecksNote(sha, `{"name":"bad","Name":"test","ok":true,"exit":0}`), decode: decodeChecksNote},
		{name: "trailing JSON", data: validReview + ` {}`, decode: decodeReviewNote},
		{name: "missing review member", data: missingReview, decode: decodeReviewNote},
		{name: "missing checks member", data: missingChecks, decode: decodeChecksNote},
		{name: "missing verdict member", data: missingVerdict, decode: decodeVerdictNote},
		{name: "null checks results", data: nullResults, decode: decodeChecksNote},
		{name: "empty checks results", data: pollChecksNote(sha, ""), decode: decodeChecksNote},
		{name: "null check result member", data: pollChecksNote(sha, `{"name":"test","ok":null,"exit":0}`), decode: decodeChecksNote},
		{name: "empty check result member", data: pollChecksNote(sha, `{"name":"","ok":true,"exit":0}`), decode: decodeChecksNote},
		{name: "check result missing name", data: pollChecksNote(sha, `{"ok":true,"exit":0}`), decode: decodeChecksNote},
		{name: "check result missing ok", data: pollChecksNote(sha, `{"name":"test","exit":0}`), decode: decodeChecksNote},
		{name: "check result missing exit", data: pollChecksNote(sha, `{"name":"test","ok":true}`), decode: decodeChecksNote},
		{name: "negative check result exit", data: pollChecksNote(sha, `{"name":"test","ok":false,"exit":-1}`), decode: decodeChecksNote},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.decode([]byte(tc.data)); err == nil {
				t.Fatalf("accepted malformed %s", tc.name)
			}
		})
	}
}

func hasArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func TestOMPUsageParser(t *testing.T) {
	path := filepath.Join(t.TempDir(), "omp.jsonl")
	data := `{"message":{"usage":{"input":3,"output":5,"cacheRead":7,"cacheWrite":11,"reasoning":13}}}
{"message":{"usage":{"input":17,"output":19,"cacheRead":23,"cacheWrite":29,"reasoning":31}}}
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	usage, err := parseOMPUsage(path)
	if err != nil {
		t.Fatal(err)
	}
	if usage.TokensIn != 17 || usage.TokensOut != 19 || usage.CacheRead != 23 || usage.CacheWrite != 29 || usage.Reasoning != 31 {
		t.Fatalf("usage=%#v", usage)
	}
}

func writeTestDeclaration(t *testing.T, root, agent string) {
	dir := filepath.Join(root, "agents", agent)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent.md"), []byte("---\nmodel: local\n---\nsystem\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "task.md"), []byte("task\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runGit(t *testing.T, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("git", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return output
}

func runGitDir(t *testing.T, dir string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git -C %s %v: %v\n%s", dir, args, err, output)
	}
	return output
}

func configGit(t *testing.T, dir, name, email string) {
	t.Helper()
	runGitDir(t, dir, "config", "user.name", name)
	runGitDir(t, dir, "config", "user.email", email)
}

func addNote(t *testing.T, root, ref, sha, payload, name, email string) {
	t.Helper()
	cmd := exec.Command("git", "-C", root, "-c", "user.name="+name, "-c", "user.email="+email, "notes", "--ref="+ref, "add", "-m", payload, sha)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("add note: %v\n%s", err, output)
	}
}

func TestAuditorSurfacesRemoteFailure(t *testing.T) {
	root, _ := testClone(t)
	runGitDir(t, root, "remote", "remove", "origin")
	if _, err := Audit(root); err == nil {
		t.Fatal("expected remote audit failure")
	}
	if _, err := os.Stat(auditStatePath(root)); !os.IsNotExist(err) {
		t.Fatalf("audit state should not be written after remote failure: %v", err)
	}
}

func testGitTransportStopsDescendants(t *testing.T, name, wantOutput string, run func(context.Context, string) ([]byte, error)) {
	t.Helper()
	tests := []struct {
		name       string
		leaderTail string
		timeout    time.Duration
		wantErr    error
	}{
		{name: "leader success", leaderTail: "exit 0\n", timeout: 3 * time.Second},
		{name: "cancellation", leaderTail: "trap '' TERM\nwhile :; do /bin/sleep 1; done\n", timeout: 100 * time.Millisecond, wantErr: context.DeadlineExceeded},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root, toolDir := t.TempDir(), t.TempDir()
			_, heartbeat := processHeartbeatFixture(t)
			script := `#!/bin/sh
set -eu
(
	trap '' HUP TERM
	while :; do
		printf x >> "$HEARTBEAT"
		/bin/sleep 0.02
	done
) &
child=$!
printf '%s\n' "$child" > "$CHILD_PID"
while [ ! -s "$HEARTBEAT" ]; do /bin/sleep 0.01; done
printf '%s\n' ` + wantOutput + "\n" + test.leaderTail
			if err := os.WriteFile(filepath.Join(toolDir, "git"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", toolDir)
			ctx, cancel := context.WithTimeout(context.Background(), test.timeout)
			started := time.Now()
			output, err := run(ctx, root)
			elapsed := time.Since(started)
			cancel()
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("%s transport error=%v want %v", name, err, test.wantErr)
			}
			if string(output) != wantOutput+"\n" {
				t.Fatalf("%s transport output=%q", name, output)
			}
			if elapsed >= 3*time.Second {
				t.Fatalf("%s transport took %s", name, elapsed)
			}
			assertProcessQuiescent(t, heartbeat, name+" transport descendant", test.name)
		})
	}
}

func TestPollRealToolStopsDescendants(t *testing.T) {
	testGitTransportStopsDescendants(t, "Poll", "poll-output", func(ctx context.Context, root string) ([]byte, error) {
		return NewPoller(root, "owner/name").git(ctx, "--version")
	})
}
