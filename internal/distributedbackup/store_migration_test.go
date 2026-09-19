package distributedbackup

import (
	"strings"
	"testing"

	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

func TestStoreMigrateAddsSnapshotColumnsToLegacySQLiteSchema(t *testing.T) {
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SingularTable(true)
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	legacySet := `CREATE TABLE legacy_backup_set (
		id char(36) PRIMARY KEY, room_id varchar(255) NOT NULL, room_name varchar(128) NOT NULL,
		name varchar(128) NOT NULL, kind varchar(24) NOT NULL, mode varchar(32) NOT NULL,
		manifest_version integer NOT NULL, topology_revision varchar(128) NOT NULL,
		shared_sha256 char(64), status varchar(24) NOT NULL, size bigint NOT NULL,
		content_size bigint NOT NULL, file_count integer NOT NULL, original_running_worlds text NOT NULL,
		manifest_sha256 char(64), failure text, source_job_id char(36), verified_at datetime,
		created_at datetime NOT NULL, updated_at datetime NOT NULL, barrier_id varchar(128)
	)`
	legacyPart := `CREATE TABLE legacy_backup_part (
		id varchar(128) PRIMARY KEY, set_id char(36) NOT NULL, room_id varchar(255) NOT NULL,
		world_id varchar(255) NOT NULL, world_name varchar(128) NOT NULL, world_role varchar(24) NOT NULL,
		target_id varchar(128) NOT NULL, installation_id varchar(64) NOT NULL, cluster varchar(64) NOT NULL,
		shard varchar(64) NOT NULL, topology_revision varchar(128) NOT NULL, file_name varchar(255) NOT NULL,
		status varchar(24) NOT NULL, size bigint NOT NULL, content_size bigint NOT NULL, file_count integer NOT NULL,
		sha256 char(64), shared_sha256 char(64), failure text, verified_at datetime,
		created_at datetime NOT NULL, updated_at datetime NOT NULL
	)`
	legacyOperation := `CREATE TABLE legacy_backup_operation (
		id char(36) PRIMARY KEY, set_id char(36), protection_set_id char(36), room_id varchar(255) NOT NULL,
		kind varchar(24) NOT NULL, phase varchar(32) NOT NULL, status varchar(32) NOT NULL,
		topology_revision varchar(128) NOT NULL, lease_id char(36), fencing_token bigint NOT NULL,
		original_running_worlds text NOT NULL, failure text, created_at datetime NOT NULL, updated_at datetime NOT NULL
	)`
	for _, statement := range []string{legacySet, legacyPart, legacyOperation} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}

	if err := db.Exec(`INSERT INTO legacy_backup_operation
  (id,room_id,kind,phase,status,topology_revision,fencing_token,original_running_worlds,created_at,updated_at)
  VALUES ('before-upgrade','room','restore','published','running','revision',1,'["master"]',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`).Error; err != nil {
		t.Fatal(err)
	}

	if err := NewStore(db, "legacy_").Migrate(); err != nil {
		t.Fatalf("legacy migration failed: %v", err)
	}

	original, err := NewStore(db, "legacy_").Operation("before-upgrade")
	if err != nil || original.Status != OperationRunning || original.Phase != "published" || len(original.OriginalRunningWorlds) != 1 || original.OriginalRunningWorlds[0] != "master" || len(original.OriginalRuntimeModes) != 0 {
		t.Fatalf("existing recovery operation changed during migration: %#v err=%v", original, err)
	}

	for _, table := range []string{"legacy_backup_set", "legacy_backup_part"} {
		var schema string
		if err := db.Raw("SELECT sql FROM sqlite_master WHERE name = ?", table).Row().Scan(&schema); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(schema, "DEFAULT 0") {
			t.Fatalf("%s snapshot migration has no default: %s", table, schema)
		}
	}
}
