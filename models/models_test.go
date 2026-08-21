package models

import (
	"fmt"
	"net/url"
	"path/filepath"
	"sync"
	"testing"
)

func TestOpenSQLiteConfiguresFileDatabaseAndSupportsConcurrentWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	database, err := openSQLite(path, "test_")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Exec(`CREATE TABLE concurrent_write (id INTEGER PRIMARY KEY AUTOINCREMENT, worker INTEGER NOT NULL, sequence INTEGER NOT NULL)`).Error; err != nil {
		t.Fatal(err)
	}
	status, err := inspectDatabase(database)
	if err != nil {
		t.Fatal(err)
	}
	if status.JournalMode != "wal" || status.BusyTimeoutMilliseconds < busyTimeoutMilliseconds || !status.ForeignKeys || status.MaxOpenConnections != fileMaxOpenConnections {
		t.Fatalf("database status = %#v", status)
	}

	const workers, writesPerWorker = 12, 75
	errorsChannel := make(chan error, workers)
	var group sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		worker := worker
		group.Add(1)
		go func() {
			defer group.Done()
			for sequence := 0; sequence < writesPerWorker; sequence++ {
				if writeErr := database.Exec("INSERT INTO concurrent_write (worker, sequence) VALUES (?, ?)", worker, sequence).Error; writeErr != nil {
					errorsChannel <- fmt.Errorf("worker %d sequence %d: %w", worker, sequence, writeErr)
					return
				}
			}
		}()
	}
	group.Wait()
	close(errorsChannel)
	for writeErr := range errorsChannel {
		t.Fatal(writeErr)
	}
	var count int
	if err := database.Table("concurrent_write").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != workers*writesPerWorker {
		t.Fatalf("row count = %d, want %d", count, workers*writesPerWorker)
	}
}

func TestOpenSQLiteKeepsMemoryDatabaseOnOneConnection(t *testing.T) {
	database, err := openSQLite(":memory:", "test_")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	status, err := inspectDatabase(database)
	if err != nil {
		t.Fatal(err)
	}
	if status.JournalMode != "memory" || status.MaxOpenConnections != 1 || !status.ForeignKeys {
		t.Fatalf("memory database status = %#v", status)
	}
}

func TestSQLiteDSNAcquiresWriteLockAtTransactionStart(t *testing.T) {
	dsn, err := sqliteDSN(filepath.Join(t.TempDir(), "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	if query.Get("_txlock") != "immediate" || query.Get("_busy_timeout") != fmt.Sprint(busyTimeoutMilliseconds) {
		t.Fatalf("sqlite DSN query = %#v", query)
	}
}
