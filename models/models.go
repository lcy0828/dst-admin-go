package models

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"dont/pkg/setting"

	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

const (
	CurrentMigrationVersion = "20260812-v2"
	fileMaxOpenConnections  = 4
	fileMaxIdleConnections  = 2
	busyTimeoutMilliseconds = 15000
)

var (
	dbMu          sync.Mutex
	db            *gorm.DB
	dbTablePrefix string

	tablePrefixPattern = regexp.MustCompile(`^[A-Za-z0-9_]*$`)
)

type Model struct {
	ID         int `gorm:"primary_key" json:"id"`
	CreatedOn  int `json:"created_on"`
	ModifiedOn int `json:"modified_on"`
}

type DatabaseStatus struct {
	Driver                  string `json:"driver"`
	JournalMode             string `json:"journalMode"`
	BusyTimeoutMilliseconds int    `json:"busyTimeoutMilliseconds"`
	ForeignKeys             bool   `json:"foreignKeys"`
	MaxOpenConnections      int    `json:"maxOpenConnections"`
	MigrationVersion        string `json:"migrationVersion"`
}

// OpenConfigured opens the process database once and validates the SQLite mode.
func OpenConfigured() (*gorm.DB, bool, error) {
	dbMu.Lock()
	defer dbMu.Unlock()
	if db != nil {
		return db, false, nil
	}
	section, err := setting.Cfg.GetSection("database")
	if err != nil {
		return nil, false, fmt.Errorf("read database configuration: %w", err)
	}
	driver := strings.TrimSpace(section.Key("TYPE").String())
	if driver != "sqlite3" {
		return nil, false, fmt.Errorf("unsupported database driver %q: this release requires sqlite3", driver)
	}
	path := strings.TrimSpace(section.Key("PATH").String())
	if override := strings.TrimSpace(os.Getenv("DST_ADMIN_DATABASE_PATH")); override != "" {
		path = override
	} else if strings.HasSuffix(os.Args[0], ".test") {
		path = ":memory:"
	} else {
		path = setting.ResolvePath(path)
	}
	prefix := strings.TrimSpace(section.Key("TABLE_PREFIX").String())
	opened, err := openSQLite(path, prefix)
	if err != nil {
		return nil, false, err
	}
	db = opened
	dbTablePrefix = prefix
	return db, true, nil
}

func openSQLite(path, tablePrefix string) (*gorm.DB, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("database path is required")
	}
	if !tablePrefixPattern.MatchString(tablePrefix) {
		return nil, errors.New("database table prefix is invalid")
	}
	memory := path == ":memory:"
	dsn, err := sqliteDSN(path, memory)
	if err != nil {
		return nil, err
	}
	opened, err := gorm.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}
	closeOnError := func(cause error) (*gorm.DB, error) {
		_ = opened.Close()
		return nil, cause
	}
	gorm.DefaultTableNameHandler = func(_ *gorm.DB, defaultTableName string) string {
		return tablePrefix + defaultTableName
	}
	opened.SingularTable(true)
	opened.LogMode(setting.RunMode == "debug" && os.Getenv("DST_ADMIN_SQL_LOG") == "1")
	if memory {
		opened.DB().SetMaxOpenConns(1)
		opened.DB().SetMaxIdleConns(1)
	} else {
		opened.DB().SetMaxOpenConns(fileMaxOpenConnections)
		opened.DB().SetMaxIdleConns(fileMaxIdleConnections)
	}
	opened.DB().SetConnMaxLifetime(0)
	opened.DB().SetConnMaxIdleTime(5 * time.Minute)
	if err := opened.DB().Ping(); err != nil {
		return closeOnError(fmt.Errorf("ping sqlite database: %w", err))
	}
	status, err := inspectDatabase(opened)
	if err != nil {
		return closeOnError(err)
	}
	expectedJournal := "wal"
	if memory {
		expectedJournal = "memory"
	}
	if status.JournalMode != expectedJournal || status.BusyTimeoutMilliseconds < busyTimeoutMilliseconds || !status.ForeignKeys {
		return closeOnError(fmt.Errorf(
			"sqlite safety settings were not applied: journal=%s busy_timeout=%d foreign_keys=%t",
			status.JournalMode, status.BusyTimeoutMilliseconds, status.ForeignKeys,
		))
	}
	if err := ensureMigrationTable(opened, tablePrefix); err != nil {
		return closeOnError(err)
	}
	return opened, nil
}

func sqliteDSN(path string, memory bool) (string, error) {
	query := url.Values{
		"_busy_timeout": []string{fmt.Sprint(busyTimeoutMilliseconds)},
		"_foreign_keys": []string{"on"},
		"_synchronous":  []string{"NORMAL"},
		// All application transactions write state. Acquiring the reserved write
		// lock at BEGIN lets SQLite's busy handler wait instead of failing while
		// a deferred transaction is being promoted from read to write.
		"_txlock": []string{"immediate"},
	}
	if memory {
		query.Set("cache", "shared")
		query.Set("mode", "memory")
		return "file:dst-admin?" + query.Encode(), nil
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve database path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(absolute), 0o750); err != nil {
		return "", fmt.Errorf("create database directory: %w", err)
	}
	query.Set("_journal_mode", "WAL")
	location := &url.URL{Scheme: "file", Path: filepath.ToSlash(absolute), RawQuery: query.Encode()}
	return location.String(), nil
}

func inspectDatabase(database *gorm.DB) (DatabaseStatus, error) {
	if database == nil {
		return DatabaseStatus{}, errors.New("database is not open")
	}
	status := DatabaseStatus{Driver: "sqlite3", MaxOpenConnections: database.DB().Stats().MaxOpenConnections}
	var foreignKeys int
	if err := database.Raw("PRAGMA journal_mode").Row().Scan(&status.JournalMode); err != nil {
		return DatabaseStatus{}, fmt.Errorf("read sqlite journal mode: %w", err)
	}
	status.JournalMode = strings.ToLower(status.JournalMode)
	if err := database.Raw("PRAGMA busy_timeout").Row().Scan(&status.BusyTimeoutMilliseconds); err != nil {
		return DatabaseStatus{}, fmt.Errorf("read sqlite busy timeout: %w", err)
	}
	if err := database.Raw("PRAGMA foreign_keys").Row().Scan(&foreignKeys); err != nil {
		return DatabaseStatus{}, fmt.Errorf("read sqlite foreign keys: %w", err)
	}
	status.ForeignKeys = foreignKeys == 1
	return status, nil
}

func ensureMigrationTable(database *gorm.DB, tablePrefix string) error {
	table := tablePrefix + "schema_migration"
	statement := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
        id INTEGER PRIMARY KEY CHECK (id = 1),
        version TEXT NOT NULL,
        updated_at DATETIME NOT NULL
    )`, quoteIdentifier(table))
	if err := database.Exec(statement).Error; err != nil {
		return fmt.Errorf("create schema migration table: %w", err)
	}
	return nil
}

func RecordMigration(version string) error {
	dbMu.Lock()
	defer dbMu.Unlock()
	if db == nil {
		return errors.New("database is not open")
	}
	version = strings.TrimSpace(version)
	if version == "" || len(version) > 128 {
		return errors.New("migration version is invalid")
	}
	table := dbTablePrefix + "schema_migration"
	statement := fmt.Sprintf(`INSERT INTO %s (id, version, updated_at) VALUES (1, ?, ?)
        ON CONFLICT(id) DO UPDATE SET version = excluded.version, updated_at = excluded.updated_at`, quoteIdentifier(table))
	if err := db.Exec(statement, version, time.Now().UTC()).Error; err != nil {
		return fmt.Errorf("record schema migration: %w", err)
	}
	return nil
}

func Status() (DatabaseStatus, error) {
	dbMu.Lock()
	defer dbMu.Unlock()
	status, err := inspectDatabase(db)
	if err != nil {
		return DatabaseStatus{}, err
	}
	table := dbTablePrefix + "schema_migration"
	row := db.Raw(fmt.Sprintf("SELECT version FROM %s WHERE id = 1", quoteIdentifier(table))).Row()
	if err := row.Scan(&status.MigrationVersion); err != nil {
		return DatabaseStatus{}, fmt.Errorf("read schema migration: %w", err)
	}
	return status, nil
}

func CloseDB() error {
	dbMu.Lock()
	defer dbMu.Unlock()
	if db == nil {
		return nil
	}
	closing := db
	db = nil
	dbTablePrefix = ""
	return closing.Close()
}

func DB() *gorm.DB {
	dbMu.Lock()
	defer dbMu.Unlock()
	return db
}

func quoteIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}
