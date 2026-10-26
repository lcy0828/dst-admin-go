package playerinventory

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCurrentIndexContentOverridesTimestampAndOtherSessions(t *testing.T) {
	root, path := saveFixture(t, "A7G0221H0KC7")
	writeFixture(t, filepath.Join(path, "0000000003"), `return {data={inventory={items={{prefab="twigs"}},equip={}}}}`)
	save := filepath.Join(root, "Room", "Master", "save")
	other := "FEDCBA9876543210"
	otherPlayer := filepath.Join(save, "session", other, "A7G0221H0KC7")
	if e := os.MkdirAll(otherPlayer, 0750); e != nil {
		t.Fatal(e)
	}
	writeFixture(t, filepath.Join(otherPlayer, "0000000003"), `return {data={inventory={items={{prefab="rocks"}},equip={}}}}`)
	index := filepath.Join(save, "shardindex")
	at := time.Now().Add(-time.Hour)
	writeFixture(t, index, `KLEI     1 return {session_id="`+testSession+`"}`)
	os.Chtimes(index, at, at)
	v, e := querySave(root)
	if e != nil || v.Inventory[0].Prefab != "twigs" {
		t.Fatal(v, e)
	}
	writeFixture(t, index, `KLEI     1 return {session_id="`+other+`"}`)
	os.Chtimes(index, at, at)
	v, e = querySave(root)
	if e != nil || v.Inventory[0].Prefab != "rocks" {
		t.Fatal(v, e)
	}
}
func TestIndexSymlinkAndIndexCodeAreRejected(t *testing.T) {
	for _, kind := range []string{"symlink", "code"} {
		t.Run(kind, func(t *testing.T) {
			root, path := saveFixture(t, "A7G0221H0KC7")
			writeFixture(t, filepath.Join(path, "0000000003"), `return {data={inventory={items={},equip={}}}}`)
			index := filepath.Join(root, "Room", "Master", "save", "shardindex")
			if kind == "symlink" {
				external := filepath.Join(t.TempDir(), "index")
				writeFixture(t, external, `return {session_id="`+testSession+`"}`)
				if e := os.Symlink(external, index); e != nil {
					t.Skip(e)
				}
			} else {
				writeFixture(t, index, `return (function() while true do end end)()`)
			}
			if _, e := querySave(root); e == nil {
				t.Fatal("accepted unsafe index")
			}
		})
	}
}
