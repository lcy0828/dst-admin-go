package configuration

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"dont/shared"
	"golang.org/x/text/encoding/unicode"
)

func TestAccessListsReadEditorEncodingsWithoutChangingFiles(t *testing.T) {
	plain := []byte("KU_TWO\r\nKU_ONE\r\nKU_ONE\r\n\r\n")
	le, _ := unicode.UTF16(unicode.LittleEndian, unicode.UseBOM).NewEncoder().Bytes(plain)
	be, _ := unicode.UTF16(unicode.BigEndian, unicode.UseBOM).NewEncoder().Bytes(plain)
	for name, data := range map[string][]byte{"utf8-bom": append([]byte{0xef, 0xbb, 0xbf}, plain...), "utf16-le": le, "utf16-be": be, "utf8": plain} {
		t.Run(name, func(t *testing.T) {
			for _, routed := range []bool{false, true} {
				service, roomPath, _ := newConfigurationService(t)
				path := filepath.Join(roomPath, "adminlist.txt")
				if err := os.WriteFile(path, data, 0o640); err != nil {
					t.Fatal(err)
				}
				if routed {
					if err := service.ConfigureReader(configurationSnapshotReader{files: []shared.RuntimeConfigurationFile{
						{Name: "adminlist.txt", Exists: true, Data: data}, {Name: "blocklist.txt"}, {Name: "whitelist.txt"},
					}}); err != nil {
						t.Fatal(err)
					}
				}
				current, err := service.AccessListsContext(context.Background(), "room")
				if err != nil || !reflect.DeepEqual(current.Admins, []string{"KU_ONE", "KU_TWO"}) || current.Blocked == nil || current.Whitelist == nil {
					t.Fatalf("routed=%v lists=%+v error=%v", routed, current, err)
				}
				before, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(before, data) {
					t.Fatal("read changed original file")
				}
			}
		})
	}
}

func TestAccessListParseErrorIdentifiesOnlyFileAndLine(t *testing.T) {
	service, roomPath, _ := newConfigurationService(t)
	if err := os.WriteFile(filepath.Join(roomPath, "blocklist.txt"), []byte("KU_GOOD\nKU_private/value\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	_, err := service.AccessLists("room")
	var detail *AccessReadError
	if !errors.As(err, &detail) || detail.File != "blocklist.txt" || detail.Line != 2 || detail.Reason != "invalid_id" {
		t.Fatal(err)
	}
	if bytes.Contains([]byte(err.Error()), []byte("private")) {
		t.Fatal("player record leaked in error")
	}
}
