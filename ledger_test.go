package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func readLedgerTail(root string, limit int) ([]RunRecord, error) {
	if limit < 0 {
		return nil, errors.New("ledger tail limit must be non-negative")
	}
	return readLedger(root, limit)
}

func TestLedgerAppendAndParseDurably(t *testing.T) {
	root := t.TempDir()
	files := defaultLedgerFileOps()
	syncFile := files.syncFile
	var events []string
	files.syncFile = func(file *os.File) error {
		switch {
		case file.Name() == root:
			events = append(events, "sync-root")
		case file.Name() == filepath.Dir(ledgerPath(root)):
			events = append(events, "sync-forest")
		case isLedgerTemp(root, file.Name()):
			events = append(events, "sync-temp")
		}
		return syncFile(file)
	}
	closeFile := files.closeFile
	files.closeFile = func(file *os.File) error {
		if isLedgerTemp(root, file.Name()) {
			events = append(events, "close-temp")
		}
		return closeFile(file)
	}
	renameFile := files.renameFile
	files.renameFile = func(oldPath, newPath string) error {
		events = append(events, "rename")
		return renameFile(oldPath, newPath)
	}

	want := RunRecord{RunID: "run-1", Agent: "builder", Started: "2026-08-10T00:00:00Z", Duration: 1.5, Exit: 0, TokensIn: 3, TokensOut: 5, CacheRead: 7, CacheWrite: 11, Reasoning: 13}
	if err := appendRun(root, want, files); err != nil {
		t.Fatal(err)
	}
	wantEvents := []string{"sync-root", "sync-temp", "close-temp", "rename", "sync-forest", "sync-root"}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Fatalf("publication events=%v, want %v", events, wantEvents)
	}
	raw, err := os.ReadFile(ledgerPath(root))
	if err != nil {
		t.Fatal(err)
	}
	const wantRow = `{"run_id":"run-1","agent":"builder","started":"2026-08-10T00:00:00Z","duration":1.5,"exit":0,"tokens_in":3,"tokens_out":5,"cache_read":7,"cache_write":11,"reasoning":13}` + "\n"
	if got := string(raw); got != wantRow {
		t.Fatalf("ledger bytes=%q, want literal durable row %q", got, wantRow)
	}
	info, err := os.Stat(ledgerPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Fatalf("ledger permissions=%#o, want 0644", got)
	}
	requireLedgerState(t, root, raw, []RunRecord{want})
	requireNoLedgerTemps(t, root)
}

func TestLedgerAppendPreservesPrefixOrderAndPermissions(t *testing.T) {
	root := t.TempDir()
	first := RunRecord{RunID: "run-1", Agent: "builder", Started: "2026-08-10T00:00:00Z"}
	second := RunRecord{RunID: "run-2", Agent: "verifier", Started: "2026-08-10T00:01:00Z"}
	third := RunRecord{RunID: "run-3", Agent: "fixer", Started: "2026-08-10T00:02:00Z"}
	if err := AppendRun(root, first); err != nil {
		t.Fatal(err)
	}
	if err := AppendRun(root, second); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(ledgerPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ledgerPath(root), 0o640); err != nil {
		t.Fatal(err)
	}

	if err := AppendRun(root, third); err != nil {
		t.Fatal(err)
	}
	const thirdRow = `{"run_id":"run-3","agent":"fixer","started":"2026-08-10T00:02:00Z","duration":0,"exit":0,"tokens_in":0,"tokens_out":0,"cache_read":0,"cache_write":0,"reasoning":0}` + "\n"
	wantBytes := append(append([]byte(nil), before...), thirdRow...)
	info, err := os.Stat(ledgerPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Fatalf("ledger permissions=%#o, want 0640", got)
	}
	requireLedgerState(t, root, wantBytes, []RunRecord{first, second, third})
	requireNoLedgerTemps(t, root)
}

func TestLedgerAppendRemovesStaleTempsAndPreservesUnrelatedFiles(t *testing.T) {
	root := t.TempDir()
	first := RunRecord{RunID: "run-1", Agent: "builder", Started: "2026-08-10T00:00:00Z"}
	second := RunRecord{RunID: "run-2", Agent: "verifier", Started: "2026-08-10T00:01:00Z"}
	if err := AppendRun(root, first); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(ledgerPath(root))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(ledgerPath(root))
	for range 3 {
		temp, err := os.CreateTemp(dir, ".runs.jsonl-*")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := temp.WriteString("stale"); err != nil {
			t.Fatal(err)
		}
		if err := temp.Close(); err != nil {
			t.Fatal(err)
		}
	}
	unrelated := map[string]string{
		".runs.jsonl":        "hidden unrelated",
		".runs.jsonl.backup": "backup",
		"runs.jsonl-backup":  "visible backup",
	}
	for name, content := range unrelated {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := AppendRun(root, second); err != nil {
		t.Fatal(err)
	}
	wantBytes := append(append([]byte(nil), before...), ledgerFixtureRow(t, second)...)
	requireLedgerState(t, root, wantBytes, []RunRecord{first, second})
	requireNoLedgerTemps(t, root)
	for name, want := range unrelated {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Fatalf("%s=%q, want %q", name, got, want)
		}
	}
}

func TestLedgerStaleTempCleanupJoinsErrorsWithoutPublishing(t *testing.T) {
	root := t.TempDir()
	first := RunRecord{RunID: "run-1", Agent: "builder", Started: "2026-08-10T00:00:00Z"}
	if err := AppendRun(root, first); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(ledgerPath(root))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(ledgerPath(root))
	stale := make([]string, 3)
	for i := range stale {
		temp, err := os.CreateTemp(dir, ".runs.jsonl-*")
		if err != nil {
			t.Fatal(err)
		}
		stale[i] = temp.Name()
		if err := temp.Close(); err != nil {
			t.Fatal(err)
		}
	}
	unrelatedPath := filepath.Join(dir, ".runs.jsonl.backup")
	if err := os.WriteFile(unrelatedPath, []byte("unrelated"), 0o644); err != nil {
		t.Fatal(err)
	}

	firstRemoveErr := errors.New("first stale remove failed")
	secondRemoveErr := errors.New("second stale remove failed")
	syncErr := errors.New("stale cleanup directory sync failed")
	files := defaultLedgerFileOps()
	removeFile := files.removeFile
	cleanupObserved := false
	files.removeFile = func(path string) error {
		if !cleanupObserved {
			cleanupObserved = true
			if count := countLedgerTemps(t, root); count != len(stale) {
				t.Fatalf("ledger temps before cleanup=%d, want %d stale temps and no new temp", count, len(stale))
			}
			probe, err := os.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			lockErr := lockLedger(probe, syscall.LOCK_SH|syscall.LOCK_NB)
			closeErr := probe.Close()
			if !errors.Is(lockErr, syscall.EWOULDBLOCK) {
				t.Fatalf("cleanup lock probe error=%v, want writer exclusion", lockErr)
			}
			if closeErr != nil {
				t.Fatal(closeErr)
			}
		}
		switch path {
		case stale[0]:
			return firstRemoveErr
		case stale[2]:
			return secondRemoveErr
		default:
			return removeFile(path)
		}
	}
	syncFile := files.syncFile
	files.syncFile = func(file *os.File) error {
		if file.Name() == dir {
			return syncErr
		}
		return syncFile(file)
	}

	err = appendRun(root, RunRecord{RunID: "run-2", Agent: "verifier"}, files)
	if !errors.Is(err, firstRemoveErr) || !errors.Is(err, secondRemoveErr) || !errors.Is(err, syncErr) {
		t.Fatalf("AppendRun() error=%v, want both removal errors and directory sync error", err)
	}
	if !cleanupObserved {
		t.Fatal("stale cleanup was not attempted")
	}
	requireLedgerState(t, root, before, []RunRecord{first})
	if count := countLedgerTemps(t, root); count != 2 {
		t.Fatalf("stale ledger temps=%d, want only the two failed removals", count)
	}
	if _, err := os.Stat(stale[1]); !os.IsNotExist(err) {
		t.Fatalf("successfully removed stale temp error=%v, want absent", err)
	}
	for _, path := range []string{stale[0], stale[2], unrelatedPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("preserved path %s: %v", path, err)
		}
	}
}

func TestLedgerLargeHistoryPreservesPrefixAndBoundsTail(t *testing.T) {
	const historyRows = 4096

	root := t.TempDir()
	if err := os.Mkdir(filepath.Dir(ledgerPath(root)), 0o755); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(ledgerPath(root), os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		t.Fatal(err)
	}
	var prefix bytes.Buffer
	history := io.MultiWriter(file, &prefix)
	for i := range historyRows {
		record := RunRecord{
			RunID:   "run-" + strconv.Itoa(i),
			Agent:   "builder",
			Started: "2026-08-10T00:00:00Z",
		}
		if _, err := history.Write(ledgerFixtureRow(t, record)); err != nil {
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	last := RunRecord{RunID: "run-appended", Agent: "fixer", Started: "2026-08-10T01:00:00Z"}
	if err := AppendRun(root, last); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(ledgerPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(raw, prefix.Bytes()) {
		t.Fatal("append did not preserve the exact large-history prefix")
	}
	const wantSuffix = `{"run_id":"run-appended","agent":"fixer","started":"2026-08-10T01:00:00Z","duration":0,"exit":0,"tokens_in":0,"tokens_out":0,"cache_read":0,"cache_write":0,"reasoning":0}` + "\n"
	if got := string(raw[prefix.Len():]); got != wantSuffix {
		t.Fatalf("appended suffix=%q, want literal row %q", got, wantSuffix)
	}
	tail, err := readLedgerTail(root, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) != 10 {
		t.Fatalf("tail rows=%d, want 10", len(tail))
	}
	for i := range 9 {
		want := "run-" + strconv.Itoa(historyRows-9+i)
		if tail[i].RunID != want {
			t.Fatalf("tail[%d].run_id=%q, want %q", i, tail[i].RunID, want)
		}
	}
	if tail[9] != last {
		t.Fatalf("tail[9]=%#v, want %#v", tail[9], last)
	}
}

func TestScanLedgerVisitsEachRowBeforeReadingTheNext(t *testing.T) {
	want := []RunRecord{
		{RunID: "run-1", Agent: "builder"},
		{RunID: "run-2", Agent: "verifier"},
	}
	visits := 0
	reader := stagedLedgerReader{
		rows: [][]byte{
			ledgerFixtureRow(t, want[0]),
			ledgerFixtureRow(t, want[1]),
		},
		visits: &visits,
	}
	var got []RunRecord
	err := scanLedger(&reader, func(_ []byte, record RunRecord) error {
		if reader.reads != visits+1 {
			t.Fatalf("visitor %d observed after %d reads, want one unread row", visits+1, reader.reads)
		}
		visits++
		got = append(got, record)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("visited records=%#v, want %#v", got, want)
	}
}

func TestReadLedgerTailValidatesRowsOutsideTail(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Dir(ledgerPath(root)), 0o755); err != nil {
		t.Fatal(err)
	}
	var data bytes.Buffer
	data.WriteString("{\n")
	for i := range 11 {
		data.Write(ledgerFixtureRow(t, RunRecord{RunID: "run-" + strconv.Itoa(i)}))
	}
	if err := os.WriteFile(ledgerPath(root), data.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := readLedgerTail(root, 10); err == nil || !strings.Contains(err.Error(), "parse ledger row 1") {
		t.Fatalf("readLedgerTail() error=%v, want validation failure outside retained tail", err)
	}
}

func TestLedgerRejectsInvalidExistingBytesWithoutPublication(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Dir(ledgerPath(root)), 0o755); err != nil {
		t.Fatal(err)
	}
	before := []byte(`{"run_id":"run-1"}`)
	if err := os.WriteFile(ledgerPath(root), before, 0o600); err != nil {
		t.Fatal(err)
	}

	err := AppendRun(root, RunRecord{RunID: "run-2", Agent: "builder"})
	if err == nil || !strings.Contains(err.Error(), "missing newline") {
		t.Fatalf("AppendRun() error=%v, want missing-newline validation error", err)
	}
	got, err := os.ReadFile(ledgerPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, before) {
		t.Fatalf("ledger bytes=%q, want unchanged %q", got, before)
	}
	if _, err := readLedger(root, -1); err == nil || !strings.Contains(err.Error(), "missing newline") {
		t.Fatalf("readLedger() error=%v, want missing-newline validation error", err)
	}
	requireNoLedgerTemps(t, root)
}

func TestLedgerFailuresBeforeRenameLeaveCanonicalUnchanged(t *testing.T) {
	tests := []struct {
		name   string
		inject func(string, ledgerFileOps) (ledgerFileOps, error)
	}{
		{
			name: "short write",
			inject: func(_ string, files ledgerFileOps) (ledgerFileOps, error) {
				writeFile := files.writeFile
				files.writeFile = func(file *os.File, data []byte) (int, error) {
					return writeFile(file, data[:len(data)/2])
				}
				return files, io.ErrShortWrite
			},
		},
		{
			name: "temp sync",
			inject: func(root string, files ledgerFileOps) (ledgerFileOps, error) {
				wantErr := errors.New("temp sync failed")
				syncFile := files.syncFile
				files.syncFile = func(file *os.File) error {
					if isLedgerTemp(root, file.Name()) {
						return wantErr
					}
					return syncFile(file)
				}
				return files, wantErr
			},
		},
		{
			name: "temp close",
			inject: func(root string, files ledgerFileOps) (ledgerFileOps, error) {
				wantErr := errors.New("temp close failed")
				closeFile := files.closeFile
				files.closeFile = func(file *os.File) error {
					err := closeFile(file)
					if isLedgerTemp(root, file.Name()) {
						return errors.Join(err, wantErr)
					}
					return err
				}
				return files, wantErr
			},
		},
		{
			name: "rename",
			inject: func(_ string, files ledgerFileOps) (ledgerFileOps, error) {
				wantErr := errors.New("rename failed")
				files.renameFile = func(string, string) error { return wantErr }
				return files, wantErr
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
				root := t.TempDir()
				if err := os.Mkdir(filepath.Dir(ledgerPath(root)), 0o755); err != nil {
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

				files, wantErr := test.inject(root, defaultLedgerFileOps())
				err := appendRun(root, RunRecord{RunID: "run-2", Agent: "builder", Started: "2026-08-10T00:01:00Z"}, files)
				if !errors.Is(err, wantErr) {
					t.Fatalf("AppendRun() error=%v, want %v", err, wantErr)
				}
				requireLedgerState(t, root, before, wantRows)
				requireNoLedgerTemps(t, root)
			})
		}
	}
}

func TestLedgerFailureAfterRenameRestoresCanonicalAndReportsRollback(t *testing.T) {
	root := t.TempDir()
	first := RunRecord{RunID: "run-1", Agent: "builder", Started: "2026-08-10T00:00:00Z"}
	if err := AppendRun(root, first); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(ledgerPath(root))
	if err != nil {
		t.Fatal(err)
	}
	files := defaultLedgerFileOps()
	syncFile := files.syncFile
	publicationErr := errors.New("publication directory sync failed")
	rollbackErr := errors.New("rollback directory sync failed")
	directorySyncs := 0
	files.syncFile = func(file *os.File) error {
		if file.Name() == filepath.Dir(ledgerPath(root)) {
			directorySyncs++
			if directorySyncs == 1 {
				return publicationErr
			}
			if directorySyncs == 2 {
				return errors.Join(syncFile(file), rollbackErr)
			}
		}
		return syncFile(file)
	}
	renameFile := files.renameFile
	renames := 0
	files.renameFile = func(oldPath, newPath string) error {
		renames++
		return renameFile(oldPath, newPath)
	}

	err = appendRun(root, RunRecord{RunID: "run-2", Agent: "builder", Started: "2026-08-10T00:01:00Z"}, files)
	if !errors.Is(err, publicationErr) || !errors.Is(err, rollbackErr) {
		t.Fatalf("AppendRun() error=%v, want publication and rollback errors", err)
	}
	if renames != 2 {
		t.Fatalf("rename calls=%d, want publication then rollback", renames)
	}
	requireLedgerState(t, root, before, []RunRecord{first})
	requireNoLedgerTemps(t, root)
}

func TestLedgerFirstPublicationRootSyncFailureRestoresAbsentLedger(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Dir(ledgerPath(root))
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	priorPath := filepath.Join(dir, "triggers.json")
	if err := os.WriteFile(priorPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	files := defaultLedgerFileOps()
	syncFile := files.syncFile
	rootSyncErr := errors.New("repository root sync failed")
	rootSyncs := 0
	files.syncFile = func(file *os.File) error {
		if file.Name() == root {
			rootSyncs++
			return rootSyncErr
		}
		return syncFile(file)
	}

	err := appendRun(root, RunRecord{RunID: "run-1", Agent: "builder"}, files)
	if !errors.Is(err, rootSyncErr) {
		t.Fatalf("AppendRun() error=%v, want %v", err, rootSyncErr)
	}
	if rootSyncs != 1 {
		t.Fatalf("repository root syncs=%d, want one after first publication", rootSyncs)
	}
	requireLedgerState(t, root, nil, nil)
	prior, err := os.ReadFile(priorPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(prior) != "{}\n" {
		t.Fatalf("pre-existing file=%q, want unchanged", prior)
	}
	requireNoLedgerTemps(t, root)
}

func TestLedgerProcessDeathKeepsCanonicalIntegrity(t *testing.T) {
	const rootEnv = "IRON_FOREST_TEST_LEDGER_CRASH_ROOT"
	if root := os.Getenv(rootEnv); root != "" {
		ready := os.NewFile(3, "ledger-crash-ready")
		block := os.NewFile(4, "ledger-crash-block")
		if ready == nil || block == nil {
			t.Fatal("missing crash process pipes")
		}
		files := defaultLedgerFileOps()
		writeFile := files.writeFile
		files.writeFile = func(file *os.File, data []byte) (int, error) {
			written, err := writeFile(file, data[:len(data)/2])
			if err != nil {
				return written, err
			}
			if _, err := ready.Write([]byte{1}); err != nil {
				return written, err
			}
			if err := ready.Close(); err != nil {
				return written, err
			}
			_, err = block.Read(make([]byte, 1))
			return written, err
		}
		if err := appendRun(root, RunRecord{RunID: "run-2", Agent: "builder", Started: "2026-08-10T00:01:00Z"}, files); err != nil {
			t.Fatal(err)
		}
		t.Fatal("crash writer unexpectedly completed")
	}

	root := t.TempDir()
	first := RunRecord{RunID: "run-1", Agent: "builder", Started: "2026-08-10T00:00:00Z"}
	second := RunRecord{RunID: "run-2", Agent: "verifier", Started: "2026-08-10T00:01:00Z"}
	if err := AppendRun(root, first); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(ledgerPath(root))
	if err != nil {
		t.Fatal(err)
	}

	readyReader, readyWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readyReader.Close()
	blockReader, blockWriter, err := os.Pipe()
	if err != nil {
		readyWriter.Close()
		t.Fatal(err)
	}
	defer blockWriter.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestLedgerProcessDeathKeepsCanonicalIntegrity$")
	cmd.Env = append(os.Environ(), rootEnv+"="+root)
	cmd.ExtraFiles = []*os.File{readyWriter, blockReader}
	var childOutput strings.Builder
	cmd.Stdout = &childOutput
	cmd.Stderr = &childOutput
	if err := cmd.Start(); err != nil {
		readyWriter.Close()
		blockReader.Close()
		t.Fatal(err)
	}
	waited := false
	defer func() {
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	if err := errors.Join(readyWriter.Close(), blockReader.Close()); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(readyReader, make([]byte, 1)); err != nil {
		t.Fatalf("crash writer readiness: %v: %s", err, childOutput.String())
	}
	whileWriting, err := os.ReadFile(ledgerPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(whileWriting, before) {
		t.Fatalf("canonical Ledger changed before rename: got %q, want %q", whileWriting, before)
	}

	lockProbe, err := os.Open(filepath.Dir(ledgerPath(root)))
	if err != nil {
		t.Fatal(err)
	}
	lockErr := lockLedger(lockProbe, syscall.LOCK_SH|syscall.LOCK_NB)
	closeErr := lockProbe.Close()
	if !errors.Is(lockErr, syscall.EWOULDBLOCK) {
		t.Fatalf("shared directory lock error=%v, want writer exclusion", lockErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}

	type readResult struct {
		rows []RunRecord
		err  error
	}
	readerStarted := make(chan struct{})
	readDone := make(chan readResult, 1)
	go func() {
		close(readerStarted)
		rows, err := readLedger(root, -1)
		readDone <- readResult{rows: rows, err: err}
	}()
	<-readerStarted
	select {
	case result := <-readDone:
		t.Fatalf("readLedger completed while writer held the directory lock: rows=%#v error=%v", result.rows, result.err)
	case <-time.After(50 * time.Millisecond):
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	waitErr := cmd.Wait()
	waited = true
	if waitErr == nil {
		t.Fatal("crash writer exited successfully, want process death")
	}
	var result readResult
	select {
	case result = <-readDone:
	case <-time.After(5 * time.Second):
		t.Fatal("readLedger did not continue after crash writer released the directory lock")
	}
	if result.err != nil || !reflect.DeepEqual(result.rows, []RunRecord{first}) {
		t.Fatalf("readLedger after process death rows=%#v error=%v, want prior row", result.rows, result.err)
	}
	if countLedgerTemps(t, root) == 0 {
		t.Fatal("crash writer left no temp, so the stale-temp read path was not exercised")
	}

	if err := AppendRun(root, second); err != nil {
		t.Fatal(err)
	}
	rows, err := readLedger(root, -1)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rows, []RunRecord{first, second}) {
		t.Fatalf("Ledger after recovery=%#v, want prior row then later append", rows)
	}
	raw, err := os.ReadFile(ledgerPath(root))
	if err != nil {
		t.Fatal(err)
	}
	const secondRow = `{"run_id":"run-2","agent":"verifier","started":"2026-08-10T00:01:00Z","duration":0,"exit":0,"tokens_in":0,"tokens_out":0,"cache_read":0,"cache_write":0,"reasoning":0}` + "\n"
	wantRaw := append(append([]byte(nil), before...), secondRow...)
	if !reflect.DeepEqual(raw, wantRaw) {
		t.Fatalf("canonical Ledger after recovery=%q, want %q", raw, wantRaw)
	}
	requireNoLedgerTemps(t, root)
}

func TestLedgerConcurrentAppendSerialization(t *testing.T) {
	root := t.TempDir()
	files := defaultLedgerFileOps()
	writeFile := files.writeFile
	var stateMu sync.Mutex
	active := 0
	overlapped := false
	files.writeFile = func(file *os.File, data []byte) (int, error) {
		stateMu.Lock()
		active++
		if active > 1 {
			overlapped = true
		}
		stateMu.Unlock()
		time.Sleep(time.Millisecond)
		written, err := writeFile(file, data)
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
			errs <- appendRun(root, RunRecord{RunID: "run", Agent: "builder"}, files)
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
		t.Fatal("ledger temp writes overlapped")
	}
	rows, err := readLedger(root, -1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != count {
		t.Fatalf("ledger rows=%d, want %d", len(rows), count)
	}
	requireNoLedgerTemps(t, root)
}

func ledgerFixtureRow(t *testing.T, record RunRecord) []byte {
	t.Helper()
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
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
	gotRows, err := readLedger(root, -1)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotRows, wantRows) {
		t.Fatalf("ledger rows=%#v, want %#v", gotRows, wantRows)
	}
}

func requireNoLedgerTemps(t *testing.T, root string) {
	t.Helper()
	if count := countLedgerTemps(t, root); count != 0 {
		t.Fatalf("stale ledger temps=%d, want none", count)
	}
}

func countLedgerTemps(t *testing.T, root string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(ledgerPath(root)))
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		if isLedgerTemp(root, filepath.Join(filepath.Dir(ledgerPath(root)), entry.Name())) {
			count++
		}
	}
	return count
}

func isLedgerTemp(root, path string) bool {
	return filepath.Dir(path) == filepath.Dir(ledgerPath(root)) &&
		strings.HasPrefix(filepath.Base(path), "."+filepath.Base(ledgerPath(root))+"-")
}

type stagedLedgerReader struct {
	rows   [][]byte
	reads  int
	visits *int
}

func (reader *stagedLedgerReader) Read(data []byte) (int, error) {
	if reader.reads > *reader.visits {
		return 0, errors.New("read requested before visiting the prior row")
	}
	if reader.reads == len(reader.rows) {
		return 0, io.EOF
	}
	n := copy(data, reader.rows[reader.reads])
	reader.reads++
	return n, nil
}
