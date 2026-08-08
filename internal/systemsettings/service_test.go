package systemsettings

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSettingsMaskSecretPreviewAndApply(t *testing.T) {
	repository := NewMemoryRepository()
	service, err := NewService(repository)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := service.Settings()
	if err != nil {
		t.Fatal(err)
	}
	secret := fieldByID(t, settings, "mod.steamAPIKey")
	if secret.Value != "" || !secret.Configured || !secret.Sensitive {
		t.Fatalf("secret leaked or lost configuration state: %#v", secret)
	}
	preview, err := service.Preview(Input{Revision: settings.Revision, Values: map[string]string{"misc.logLevel": "debug"}})
	if err != nil || !preview.Valid || len(preview.Changes) != 1 || preview.Changes[0].Before != "info" {
		t.Fatalf("preview=%#v err=%v", preview, err)
	}
	if _, err := service.Apply(Input{Revision: settings.Revision, Values: map[string]string{"misc.logLevel": "debug"}, Confirmation: "wrong"}); !errors.Is(err, ErrConfirmationRequired) {
		t.Fatalf("confirmation error=%v", err)
	}
	result, err := service.Apply(Input{Revision: settings.Revision, Values: map[string]string{"misc.logLevel": "debug"}, Confirmation: ApplyConfirmation})
	if err != nil || !result.Settings.RestartRequired || result.Settings.Revision == settings.Revision || fieldByID(t, result.Settings, "misc.logLevel").Value != "debug" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if _, err := service.Preview(Input{Revision: settings.Revision, Values: map[string]string{"misc.logLevel": "warn"}}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale revision error=%v", err)
	}
}

func TestSettingsValidationAndEnvironmentLock(t *testing.T) {
	service, err := NewService(NewMemoryRepository())
	if err != nil {
		t.Fatal(err)
	}
	service.lookupEnv = func(name string) (string, bool) {
		if name == "DST_ADMIN_SAVE_PATH" {
			return "/environment/saves", true
		}
		return "", false
	}
	settings, _ := service.Settings()
	locked := fieldByID(t, settings, "paths.save")
	if locked.Editable || locked.Source != SourceEnvironment || locked.Environment != "DST_ADMIN_SAVE_PATH" {
		t.Fatalf("environment field=%#v", locked)
	}
	if _, err := service.Preview(Input{Revision: settings.Revision, Values: map[string]string{"paths.save": "/changed"}}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("environment override edit error=%v", err)
	}
	preview, err := service.Preview(Input{Revision: settings.Revision, Values: map[string]string{"paths.backup": "relative/path"}})
	if err != nil || preview.Valid || len(preview.Issues) == 0 {
		t.Fatalf("invalid path preview=%#v err=%v", preview, err)
	}
}

func TestFileRepositoryBacksUpAndRestrictsPermissions(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "app.conf")
	original := []byte("[misc]\nLOG_LEVEL = info\n")
	if err := os.WriteFile(path, original, 0644); err != nil {
		t.Fatal(err)
	}
	repository := NewFileRepository(path)
	snapshot, err := repository.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	updated, err := repository.Save(snapshot.Revision, map[string]string{"misc.logLevel": "warn"})
	if err != nil || updated.Revision == snapshot.Revision {
		t.Fatalf("updated=%#v err=%v", updated, err)
	}
	backup, err := os.ReadFile(path + ".bak")
	if err != nil || string(backup) != string(original) {
		t.Fatalf("backup=%q err=%v", backup, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("mode=%v err=%v", info.Mode().Perm(), err)
	}
	current, _ := os.ReadFile(path)
	if !strings.Contains(string(current), "LOG_LEVEL = warn") {
		t.Fatalf("updated config=%q", current)
	}
}

func fieldByID(t *testing.T, settings Settings, id string) Field {
	t.Helper()
	for _, field := range settings.Fields {
		if field.ID == id {
			return field
		}
	}
	t.Fatalf("field %s not found", id)
	return Field{}
}
