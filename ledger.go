package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

var (
	ledgerMu        sync.Mutex
	ledgerWriteFile = func(file *os.File, data []byte) (int, error) { return file.Write(data) }
	ledgerSyncFile  = func(file *os.File) error { return file.Sync() }
	ledgerCloseFile = func(file *os.File) error { return file.Close() }
)

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
	if record.Started == "" {
		record.Started = time.Now().UTC().Format(time.RFC3339Nano)
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	data = append(data, '\n')

	ledgerMu.Lock()
	defer ledgerMu.Unlock()
	path := ledgerPath(root)
	dir := filepath.Dir(path)
	if err := os.Mkdir(dir, 0o755); err != nil {
		if !os.IsExist(err) {
			return err
		}
	} else if err := syncLedgerDirectory(filepath.Dir(dir)); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, 0o644)
	created := err == nil
	if os.IsExist(err) {
		file, err = os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	}
	if err != nil {
		return err
	}
	if err := lockLedger(file, syscall.LOCK_EX); err != nil {
		closeErr := ledgerCloseFile(file)
		namespaceErr := restoreLedgerNamespace(path, created)
		return errors.Join(err, closeErr, namespaceErr)
	}
	info, err := file.Stat()
	if err != nil {
		namespaceErr := restoreLedgerNamespace(path, created)
		closeErr := ledgerCloseFile(file)
		return errors.Join(err, namespaceErr, closeErr)
	}
	previousLength := info.Size()

	written, writeErr := ledgerWriteFile(file, data)
	if written != len(data) {
		writeErr = errors.Join(writeErr, io.ErrShortWrite)
	}
	if writeErr != nil {
		return rollbackLedger(file, path, previousLength, created, writeErr)
	}
	if err := ledgerSyncFile(file); err != nil {
		return rollbackLedger(file, path, previousLength, created, err)
	}
	if created {
		if err := syncLedgerDirectory(dir); err != nil {
			return rollbackLedger(file, path, previousLength, created, err)
		}
		if err := syncLedgerDirectory(filepath.Dir(dir)); err != nil {
			return rollbackLedger(file, path, previousLength, created, err)
		}
	}
	return commitLedger(file, path, previousLength, created)
}

func restoreOpenLedger(file *os.File, length int64) error {
	truncateErr := file.Truncate(length)
	syncErr := ledgerSyncFile(file)
	return errors.Join(truncateErr, syncErr)
}

func rollbackLedger(file *os.File, path string, length int64, created bool, cause error) error {
	restoreErr := restoreOpenLedger(file, length)
	namespaceErr := restoreLedgerNamespace(path, created)
	closeErr := ledgerCloseFile(file)
	return errors.Join(cause, restoreErr, namespaceErr, closeErr)
}

func commitLedger(file *os.File, path string, length int64, created bool) error {
	guardFD, err := syscall.Dup(int(file.Fd()))
	if err != nil {
		return rollbackLedger(file, path, length, created, err)
	}
	guard := os.NewFile(uintptr(guardFD), file.Name())
	if err := ledgerCloseFile(file); err != nil {
		restoreErr := restoreOpenLedger(guard, length)
		namespaceErr := restoreLedgerNamespace(path, created)
		closeErr := ledgerCloseFile(guard)
		return errors.Join(err, restoreErr, namespaceErr, closeErr)
	}
	return guard.Close()
}

func restoreLedgerNamespace(path string, created bool) error {
	if !created {
		return nil
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return syncLedgerDirectory(filepath.Dir(path))
}

func syncLedgerDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := ledgerSyncFile(dir)
	closeErr := ledgerCloseFile(dir)
	return errors.Join(syncErr, closeErr)
}

func ReadLedger(root string) ([]RunRecord, error) {
	ledgerMu.Lock()
	defer ledgerMu.Unlock()
	file, err := os.Open(ledgerPath(root))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := lockLedger(file, syscall.LOCK_SH); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	defer file.Close()
	var records []RunRecord
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if len(scanner.Bytes()) == 0 {
			continue
		}
		var record RunRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return nil, fmt.Errorf("parse ledger row: %w", err)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return records, nil
}
