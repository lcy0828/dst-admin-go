package worldquery

import (
	"fmt"
	"testing"
	"time"
)

func TestRetentionIsBoundedAndDoesNotTouchOtherTables(t *testing.T) {
	s, _, _, q := fixture(t)
	now := s.now()
	if err := s.store.db.Exec("CREATE TABLE unrelated_command_audit (id TEXT)").Error; err != nil {
		t.Fatal(err)
	}
	if err := s.store.db.Exec("INSERT INTO unrelated_command_audit (id) VALUES (?)", "administrator-command").Error; err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2001; i++ {
		r := base(q)
		r.QueryRunID = fmt.Sprintf("query-%04d", i)
		if err := s.store.save(record{ID: r.QueryRunID, CreatedAt: now.Add(-time.Duration(i) * time.Second)}, r); err != nil {
			t.Fatal(err)
		}
	}
	old := base(q)
	old.QueryRunID = "expired"
	if err := s.store.save(record{ID: old.QueryRunID, CreatedAt: now.Add(-8 * 24 * time.Hour)}, old); err != nil {
		t.Fatal(err)
	}
	if err := s.store.prune(now); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.store.db.Table(s.store.table).Count(&count).Error; err != nil || count != 1999 {
		t.Fatal(count, err)
	}
	if _, _, err := s.store.get("expired"); err != ErrNotFound {
		t.Fatal(err)
	}
	if err := s.store.db.Table("unrelated_command_audit").Count(&count).Error; err != nil || count != 1 {
		t.Fatal("administrator audit changed", count, err)
	}
}
