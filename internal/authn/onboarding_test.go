package authn

import (
	"path/filepath"
	"testing"

	"github.com/jinzhu/gorm"
)

func TestOnboardingSurvivesRestartAndCompletion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admin.db")
	open := func() (*gorm.DB, *Service) {
		db, err := gorm.Open("sqlite3", path)
		if err != nil {
			t.Fatal(err)
		}
		db.LogMode(false)
		s := NewService(db)
		if err := s.Migrate(); err != nil {
			t.Fatal(err)
		}
		return db, s
	}
	db, service := open()
	created, _, err := service.Setup("owner", "test-password", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := OnboardingFor(created.Admin); !got.Required || got.Step != "deployment" {
		t.Fatalf("new admin: %+v", got)
	}
	if _, err := service.SaveOnboarding(created.Admin.ID, "game"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, service = open()
	defer db.Close()
	loggedIn, _, err := service.Login("owner", "test-password", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := OnboardingFor(loggedIn.Admin); !got.Required || got.Step != "game" {
		t.Fatalf("resumed: %+v", got)
	}
	if _, err := service.SaveOnboarding(loggedIn.Admin.ID, "destroy-saves"); err != ErrOnboardingStep {
		t.Fatalf("invalid step: %v", err)
	}
	if got, err := service.SaveOnboarding(loggedIn.Admin.ID, "complete"); err != nil || got.Required {
		t.Fatalf("complete: %+v %v", got, err)
	}
	if got, err := service.SaveOnboarding(loggedIn.Admin.ID, "room"); err != nil || got.Required || got.Step != "complete" {
		t.Fatalf("late write reopened setup: %+v %v", got, err)
	}
	if _, _, err := service.Setup("replacement", "test-password", "", ""); err != ErrSetupComplete {
		t.Fatalf("setup overwritten: %v", err)
	}
}

func TestOnboardingMigrationPreservesExistingAdministrator(t *testing.T) {
	db := openTestDB(t)
	if err := db.Exec("CREATE TABLE legacy_auth (id INTEGER PRIMARY KEY, username TEXT, password TEXT)").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO legacy_auth (id, username, password) VALUES (1, ?, ?)", "existing", "legacy-password").Error; err != nil {
		t.Fatal(err)
	}
	service, err := NewServiceWithTablePrefix(db, "legacy_")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Migrate(); err != nil {
		t.Fatal(err)
	}
	loggedIn, _, err := service.Login("existing", "legacy-password", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := OnboardingFor(loggedIn.Admin); got.Required {
		t.Fatalf("existing admin forced into setup: %+v", got)
	}
}
