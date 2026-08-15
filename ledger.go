package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

var ledgerMu sync.Mutex

type ledgerFileOps struct {
	writeFile  func(*os.File, []byte) (int, error)
	syncFile   func(*os.File) error
	closeFile  func(*os.File) error
	renameFile func(string, string) error
	removeFile func(string) error
}

func defaultLedgerFileOps() ledgerFileOps {
	return ledgerFileOps{
		writeFile:  (*os.File).Write,
		syncFile:   (*os.File).Sync,
		closeFile:  (*os.File).Close,
		renameFile: os.Rename,
		removeFile: os.Remove,
	}
}

type Usage struct {
	TokensIn   int64 `json:"tokens_in"`
	TokensOut  int64 `json:"tokens_out"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
	Reasoning  int64 `json:"reasoning"`
}

type RunRecord struct {
	RunID      string  `json:"run_id"`
	Agent      string  `json:"agent"`
	Started    string  `json:"started"`
	Duration   float64 `json:"duration"`
	Exit       int     `json:"exit"`
	TokensIn   int64   `json:"tokens_in"`
	TokensOut  int64   `json:"tokens_out"`
	CacheRead  int64   `json:"cache_read"`
	CacheWrite int64   `json:"cache_write"`
	Reasoning  int64   `json:"reasoning"`
	// DefinitionSHA records the verified declaration digest (the ordered
	// agent.md + task.md pair) that was loaded and confirmed unchanged at
	// dispatch, so a later check can see which declaration a Run executed.
	DefinitionSHA string `json:"definition_sha,omitempty"`
}

func ledgerPath(root string) string { return filepath.Join(root, workspaceName, "runs.jsonl") }

func lockLedger(file *os.File, operation int) error {
	for {
		err := syscall.Flock(int(file.Fd()), operation)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

func AppendRun(root string, record RunRecord) error {
	return appendRun(root, record, defaultLedgerFileOps())
}

func appendRun(root string, record RunRecord, files ledgerFileOps) (err error) {
	if record.Started == "" {
		record.Started = time.Now().UTC().Format(time.RFC3339Nano)
	}
	row, err := json.Marshal(record)
	if err != nil {
		return err
	}
	row = append(row, '\n')

	ledgerMu.Lock()
	defer ledgerMu.Unlock()
	path := ledgerPath(root)
	dir := filepath.Dir(path)
	if err := os.Mkdir(dir, 0o755); err == nil {
		if err := syncLedgerDirectory(filepath.Dir(dir), files); err != nil {
			return err
		}
	} else if !os.IsExist(err) {
		return err
	}

	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := lockLedger(directory, syscall.LOCK_EX); err != nil {
		return errors.Join(err, files.closeFile(directory))
	}
	defer func() {
		err = errors.Join(err, files.closeFile(directory))
	}()

	if err := cleanupLedgerTemps(directory, path, files); err != nil {
		return err
	}

	previous, mode, existed, err := openLedgerForAppend(path, files)
	if err != nil {
		return err
	}
	if previous != nil {
		defer func() {
			if previous != nil {
				err = errors.Join(err, files.closeFile(previous))
			}
		}()
	}
	var rollback *os.File
	if existed {
		rollback, err = os.Open(path)
		if err != nil {
			return err
		}
		defer func() {
			err = errors.Join(err, rollback.Close())
		}()
	}

	tempPath, err := writeLedgerTemp(dir, path, mode, files, func(temp *os.File) error {
		if previous != nil {
			if err := scanLedger(previous, func(row []byte, _ RunRecord) error {
				return writeLedgerBytes(temp, row, files)
			}); err != nil {
				return err
			}
			current := previous
			previous = nil
			if err := files.closeFile(current); err != nil {
				return err
			}
		}
		return writeLedgerBytes(temp, row, files)
	})
	if err != nil {
		return err
	}
	if err := files.renameFile(tempPath, path); err != nil {
		return errors.Join(err, files.removeFile(tempPath))
	}
	if err := files.syncFile(directory); err != nil {
		return errors.Join(err, rollbackPublishedLedger(directory, path, rollback, mode, existed, files))
	}
	if !existed {
		if err := syncLedgerDirectory(filepath.Dir(dir), files); err != nil {
			return errors.Join(err, rollbackPublishedLedger(directory, path, rollback, mode, existed, files))
		}
	}
	return nil
}

func cleanupLedgerTemps(directory *os.File, path string, files ledgerFileOps) error {
	dir := filepath.Dir(path)
	entries, err := directory.ReadDir(-1)
	if err != nil {
		return err
	}
	prefix := "." + filepath.Base(path) + "-"
	var cleanupErr error
	removed := false
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		if err := files.removeFile(filepath.Join(dir, entry.Name())); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
			continue
		}
		removed = true
	}
	if removed {
		cleanupErr = errors.Join(cleanupErr, files.syncFile(directory))
	}
	return cleanupErr
}

func openLedgerForAppend(path string, files ledgerFileOps) (*os.File, os.FileMode, bool, error) {
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, 0o644, false, nil
	}
	if err != nil {
		return nil, 0, false, err
	}
	info, statErr := file.Stat()
	if statErr != nil {
		return nil, 0, true, errors.Join(statErr, files.closeFile(file))
	}
	return file, info.Mode().Perm(), true, nil
}

func writeLedgerTemp(dir, path string, mode os.FileMode, files ledgerFileOps, write func(*os.File) error) (string, error) {
	file, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
	if err != nil {
		return "", err
	}
	tempPath := file.Name()
	if err := file.Chmod(mode); err != nil {
		return "", discardOpenLedgerTemp(file, err, files)
	}
	if err := write(file); err != nil {
		return "", discardOpenLedgerTemp(file, err, files)
	}
	if err := files.syncFile(file); err != nil {
		return "", discardOpenLedgerTemp(file, err, files)
	}
	if err := files.closeFile(file); err != nil {
		return "", errors.Join(err, files.removeFile(tempPath))
	}
	return tempPath, nil
}

func writeLedgerBytes(file *os.File, data []byte, files ledgerFileOps) error {
	written, err := files.writeFile(file, data)
	if written != len(data) {
		err = errors.Join(err, io.ErrShortWrite)
	}
	return err
}

func discardOpenLedgerTemp(file *os.File, cause error, files ledgerFileOps) error {
	return errors.Join(cause, files.closeFile(file), files.removeFile(file.Name()))
}

func rollbackPublishedLedger(directory *os.File, path string, previous *os.File, mode os.FileMode, existed bool, files ledgerFileOps) error {
	if !existed {
		return errors.Join(files.removeFile(path), files.syncFile(directory))
	}
	if _, err := previous.Seek(0, io.SeekStart); err != nil {
		return err
	}
	tempPath, err := writeLedgerTemp(filepath.Dir(path), path, mode, files, func(temp *os.File) error {
		return scanLedger(previous, func(row []byte, _ RunRecord) error {
			return writeLedgerBytes(temp, row, files)
		})
	})
	if err != nil {
		return err
	}
	if err := files.renameFile(tempPath, path); err != nil {
		return errors.Join(err, files.removeFile(tempPath))
	}
	return files.syncFile(directory)
}

func scanLedger(reader io.Reader, visit func([]byte, RunRecord) error) error {
	input := bufio.NewReader(reader)
	for rowNumber := 1; ; rowNumber++ {
		row, err := input.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			if len(row) == 0 {
				return nil
			}
			return fmt.Errorf("parse ledger row %d: missing newline", rowNumber)
		}
		if err != nil {
			return fmt.Errorf("read ledger row %d: %w", rowNumber, err)
		}
		if len(row) == 1 {
			return fmt.Errorf("parse ledger row %d: empty row", rowNumber)
		}
		var record RunRecord
		if err := json.Unmarshal(row[:len(row)-1], &record); err != nil {
			return fmt.Errorf("parse ledger row %d: %w", rowNumber, err)
		}
		if err := visit(row, record); err != nil {
			return err
		}
	}
}

func syncLedgerDirectory(path string, files ledgerFileOps) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := files.syncFile(dir)
	closeErr := files.closeFile(dir)
	return errors.Join(syncErr, closeErr)
}

func ReadLedger(root string) ([]RunRecord, error) {
	return readLedger(root, -1)
}

func ReadLedgerTail(root string, limit int) ([]RunRecord, error) {
	if limit < 0 {
		return nil, errors.New("ledger tail limit must be non-negative")
	}
	return readLedger(root, limit)
}

// errLedgerCursorUnknown reports a paging cursor that names no ledger row, so a
// caller paging with a stale cursor learns it instead of silently restarting.
var errLedgerCursorUnknown = errors.New("run identity is not in the ledger")

// errLedgerIdentityUnusable reports a ledger row whose identity cannot carry a
// paging cursor: empty, or shared with an older row. Identities are made
// unique, so this is a corrupt ledger rather than a caller mistake. Failing here
// is what stops a client from looping on a cursor that never advances.
var errLedgerIdentityUnusable = errors.New("ledger row identity cannot carry a cursor")

// ReadLedgerPage returns one newest-first page. after names the oldest identity
// already delivered; the page continues from the next older row. The returned
// cursor is empty on the last page.
func ReadLedgerPage(root string, limit int, after string) ([]RunRecord, string, error) {
	if limit <= 0 {
		return nil, "", errors.New("ledger page limit must be positive")
	}
	// Without a cursor the newest rows are the page, so a bounded tail read
	// suffices; one extra row reveals whether an older page exists.
	window := limit + 1
	if after != "" {
		window = -1
	}
	records, err := readLedger(root, window)
	if err != nil {
		return nil, "", err
	}
	slices.Reverse(records)
	if after != "" {
		index := slices.IndexFunc(records, func(record RunRecord) bool { return record.RunID == after })
		if index < 0 {
			return nil, "", fmt.Errorf("%w: %q", errLedgerCursorUnknown, after)
		}
		records = records[index+1:]
		if slices.ContainsFunc(records, func(record RunRecord) bool { return record.RunID == after }) {
			return nil, "", fmt.Errorf("%w: %q names more than one row", errLedgerIdentityUnusable, after)
		}
	}
	if len(records) <= limit {
		return records, "", nil
	}
	page := records[:limit]
	cursor := page[limit-1].RunID
	if cursor == "" {
		return nil, "", fmt.Errorf("%w: the page boundary has an empty identity", errLedgerIdentityUnusable)
	}
	return page, cursor, nil
}

// FindRun returns the ledger row for one run identity, stopping at the first
// match.
func FindRun(root, runID string) (RunRecord, bool, error) {
	var found RunRecord
	stop := errors.New("stop")
	err := visitLedger(root, func(record RunRecord) error {
		if record.RunID != runID {
			return nil
		}
		found = record
		return stop
	})
	if errors.Is(err, stop) {
		return found, true, nil
	}
	if err != nil {
		return RunRecord{}, false, err
	}
	return RunRecord{}, false, nil
}

func readLedger(root string, limit int) ([]RunRecord, error) {
	var records []RunRecord
	if limit > 0 {
		records = make([]RunRecord, 0, limit)
	}
	next := 0
	if err := visitLedger(root, func(record RunRecord) error {
		switch {
		case limit < 0:
			records = append(records, record)
		case limit == 0:
		case len(records) < limit:
			records = append(records, record)
		default:
			records[next] = record
			next = (next + 1) % limit
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if next != 0 {
		slices.Reverse(records[:next])
		slices.Reverse(records[next:])
		slices.Reverse(records)
	}
	return records, nil
}

// visitLedger reads every row under the shared ledger lock. A visitor error
// stops the scan and reaches the caller, which is how bounded lookups exit
// early.
func visitLedger(root string, visit func(RunRecord) error) (err error) {
	ledgerMu.Lock()
	defer ledgerMu.Unlock()
	path := ledgerPath(root)
	directory, err := os.Open(filepath.Dir(path))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := lockLedger(directory, syscall.LOCK_SH); err != nil {
		return errors.Join(err, directory.Close())
	}
	defer func() {
		err = errors.Join(err, directory.Close())
	}()
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	scanErr := scanLedger(file, func(_ []byte, record RunRecord) error { return visit(record) })
	return errors.Join(scanErr, file.Close())
}
