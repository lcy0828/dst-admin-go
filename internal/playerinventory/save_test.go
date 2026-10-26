package playerinventory

import (
	"bytes"
	"compress/flate"
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dont/shared"
)

const testUser = "KU_01234567"
const testSession = "0123456789ABCDEF"

func TestNativeUserDirectoryVectors(t *testing.T) {
	for id, want := range map[string]string{"KU_00000000": "A7G000000000", "KU_abcdefgh": "A7KIB6JQ56LB", "KU_ABCDEFGH": "A7H8MC6JHT0H", "KU_01234567": "A7G0221H0KC7", "KU_zzzzzzzz": "A7NNRTUVFNRT", "KU_Az09az09": "A7HBQ04P7K09", "KU_--------": "A7NRTUVFNRTU", "KU_________": "A7NVVVVVVVVV"} {
		got, err := EncodedUserDirectory(id)
		if err != nil || got != want {
			t.Fatalf("%s: %s %v", id, got, err)
		}
	}
	if _, err := EncodedUserDirectory("KU_short"); Code(err) != "ID_MAPPING_UNSUPPORTED" {
		t.Fatal(err)
	}
}

func saveFixture(t *testing.T, directory string) (string, string) {
	t.Helper()
	root := t.TempDir()
	session := filepath.Join(root, "Room", "Master", "save", "session", testSession)
	if err := os.MkdirAll(filepath.Join(session, directory), 0750); err != nil {
		t.Fatal(err)
	}
	return root, filepath.Join(session, directory)
}
func writeFixture(t *testing.T, path, source string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(source), 0640); err != nil {
		t.Fatal(err)
	}
}
func querySave(root string) (shared.PlayerInventory, error) {
	return ReadSave(context.Background(), root, "Room", "Master", shared.PlayerInventoryRequest{PlayerID: testUser, Source: "save"})
}

func TestSaveInventoryReferencesRawValuesAndNoWrites(t *testing.T) {
	for _, dir := range []string{"A7G0221H0KC7", testUser + "_", testUser} {
		t.Run(dir, func(t *testing.T) {
			root, path := saveFixture(t, dir)
			source := `KLEI     1 return {prefab="wilson",data={inventory={items={[1]={prefab="twigs",data={stackable={stack=40}}},[3]={prefab="bundle",data={unwrappable={items={{prefab="axe",data={finiteuses={uses=0}}}}}}}},equip={BODY={prefab="backpack",data={container={items={[2]={prefab="carrot",data={perishable={time=100}}}}}}}},activeitem={prefab="goldnugget"}}}}`
			file := filepath.Join(path, "0000000003")
			writeFixture(t, file, source)
			if err := os.Chmod(file, 0440); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(file)
			value, err := querySave(root)
			if err != nil {
				t.Fatal(err)
			}
			if value.Status != "complete" || len(value.Inventory) != 2 || len(value.Equipment) != 1 || value.ActiveItem.Prefab != "goldnugget" || value.SlotCount != nil {
				t.Fatalf("%+v", value)
			}
			if *value.Inventory[0].Count != 40 {
				t.Fatal(value.Inventory[0])
			}
			metric := value.Inventory[1].Contents[0].Metrics["uses"]
			if metric.Value == nil || *metric.Value != 0 || metric.Ratio != nil || metric.Maximum != nil {
				t.Fatal(metric)
			}
			value.Inventory[0].Prefab = "modified-by-caller"
			again, err := querySave(root)
			if err != nil || again.Inventory[0].Prefab != "twigs" {
				t.Fatalf("cache shared mutable result: %+v %v", again, err)
			}
			after, _ := os.ReadFile(file)
			if !bytes.Equal(before, after) {
				t.Fatal("query changed save")
			}
		})
	}
}

func TestSaveSelectionRollbackAndSameMetadataRewrite(t *testing.T) {
	root, path := saveFixture(t, "A7G0221H0KC7")
	old := filepath.Join(path, "0000000003")
	latest := filepath.Join(path, "0000000004")
	source := func(prefab string) string {
		return `return {prefab="wilson",data={inventory={items={{prefab="` + prefab + `"}},equip={}}}}`
	}
	writeFixture(t, old, source("twigs"))
	writeFixture(t, latest, source("rocks"))
	past := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(latest, past, past); err != nil {
		t.Fatal(err)
	}
	value, err := querySave(root)
	if err != nil || value.Inventory[0].Prefab != "rocks" {
		t.Fatalf("mtime chose wrong snapshot: %+v %v", value, err)
	}
	writeFixture(t, latest, source("flint"))
	if err := os.Chtimes(latest, past, past); err != nil {
		t.Fatal(err)
	}
	value, err = querySave(root)
	if err != nil || value.Inventory[0].Prefab != "flint" {
		t.Fatalf("same metadata cache was stale: %+v %v", value, err)
	}
	if err = os.Remove(latest); err != nil {
		t.Fatal(err)
	} // Simulated engine rollback in a temporary fixture only.
	value, err = querySave(root)
	if err != nil || value.Inventory[0].Prefab != "twigs" {
		t.Fatalf("rollback cache was stale: %+v %v", value, err)
	}
}

func TestAmbiguousUnsafeAndCorruptSaves(t *testing.T) {
	t.Run("directories", func(t *testing.T) {
		root, path := saveFixture(t, "A7G0221H0KC7")
		source := `return {prefab="wilson",data={inventory={items={},equip={}}}}`
		writeFixture(t, filepath.Join(path, "0000000003"), source)
		other := filepath.Join(filepath.Dir(path), testUser+"_")
		os.Mkdir(other, 0750)
		writeFixture(t, filepath.Join(other, "0000000003"), source)
		value, err := querySave(root)
		if Code(err) != "SOURCE_AMBIGUOUS" || len(value.Candidates) != 2 {
			t.Fatalf("%+v %v", value, err)
		}
		q := shared.PlayerInventoryRequest{PlayerID: testUser, Source: "save", SourceID: value.Candidates[0].SourceID}
		value, err = ReadSave(context.Background(), root, "Room", "Master", q)
		if err != nil || !value.Empty {
			t.Fatalf("%+v %v", value, err)
		}
	})
	t.Run("latest corruption", func(t *testing.T) {
		root, path := saveFixture(t, "A7G0221H0KC7")
		writeFixture(t, filepath.Join(path, "0000000003"), `return {data={inventory={items={},equip={}}}}`)
		writeFixture(t, filepath.Join(path, "0000000004"), `return execute_arbitrary_code()`)
		if _, err := querySave(root); Code(err) != "SAVE_FORMAT_UNSUPPORTED" {
			t.Fatal(err)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		root, path := saveFixture(t, "A7G0221H0KC7")
		external := filepath.Join(t.TempDir(), "player")
		writeFixture(t, external, `return {}`)
		if err := os.Symlink(external, filepath.Join(path, "0000000003")); err != nil {
			t.Skip(err)
		}
		if _, err := querySave(root); Code(err) != "SAVE_INVALID" {
			t.Fatal(err)
		}
	})
}

func TestBoundedDataParserAndCompression(t *testing.T) {
	source := `local t={prefab="wilson",data={inventory={items={},equip={}}}}; local i={prefab="twigs"}; t.data.inventory.items[1]=i; return t`
	var compressed bytes.Buffer
	w, _ := flate.NewWriter(&compressed, flate.BestSpeed)
	w.Write([]byte(source))
	w.Close()
	packed := append(make([]byte, 16), compressed.Bytes()...)
	for _, data := range [][]byte{[]byte(source), []byte("KLEI0001   " + base64.StdEncoding.EncodeToString(packed))} {
		root, err := parse(context.Background(), data)
		if err != nil {
			t.Fatal(err)
		}
		value := baseResult(testUser, "save")
		if err = normalize(root, &value); err != nil || value.Inventory[0].Prefab != "twigs" {
			t.Fatal(value, err)
		}
	}
	for _, source := range []string{`return os.execute("touch /tmp/must-never-execute")`, `return (function() while true do end end)()`, `return {x=` + strings.Repeat("{", 70) + strings.Repeat("}", 70) + `}`, `return {x="` + strings.Repeat("a", (256<<10)+1) + `"}`} {
		if _, err := parse(context.Background(), []byte(source)); err == nil {
			t.Fatal("accepted unsupported/over-limit data")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := parse(ctx, []byte(`return {items={`+strings.Repeat(`{},`, 300)+`}}`)); err == nil {
		t.Fatal("parser ignored cancellation")
	}
}

func FuzzPlayerSaveData(f *testing.F) {
	f.Add([]byte(`return {prefab="wilson",data={inventory={items={},equip={}}}}`))
	f.Add([]byte(`local t={};t.self=t;return t`))
	f.Add([]byte("\x03\x22\x00" + `return {prefab="wilson",data={inventory={items={},equip={}}},pad=""}` + "\x01"))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > MaxInput {
			return
		}
		root, err := parse(context.Background(), data)
		if err == nil {
			value := baseResult(testUser, "save")
			_ = normalize(root, &value)
		}
	})
}

func BenchmarkSaveInventory(b *testing.B) {
	data := []byte(`return {prefab="wilson",data={inventory={items={` + strings.Repeat(`{prefab="twigs",data={stackable={stack=40}}},`, 80) + `},equip={}}}}`)
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		root, err := parse(context.Background(), data)
		if err != nil {
			b.Fatal(err)
		}
		value := baseResult(testUser, "save")
		if err = normalize(root, &value); err != nil {
			b.Fatal(err)
		}
	}
}

func TestParserNestedReferencesRespectDepthBudget(t *testing.T) {
	data := []byte(`local a={}; return a` + strings.Repeat(`[a`, 1000) + strings.Repeat(`]`, 1000))
	if _, e := parse(context.Background(), data); Code(e) != "SAVE_LIMIT_EXCEEDED" {
		t.Fatal(e)
	}
}
