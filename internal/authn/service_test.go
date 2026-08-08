package authn

import (
	"strings"
	"testing"

	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
	"golang.org/x/crypto/bcrypt"
)

func openTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	db.SingularTable(true)
	db.LogMode(false)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestMigrateHashesLegacyPassword(t *testing.T) {
	db := openTestDB(t)
	if err := db.CreateTable(&Admin{}).Error; err != nil {
		t.Fatalf("create legacy auth table: %v", err)
	}
	if err := db.Create(&Admin{ID: 1, Username: "lcy", Password: "001008"}).Error; err != nil {
		t.Fatalf("insert legacy admin: %v", err)
	}

	service := NewService(db)
	if err := service.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var migrated Admin
	if err := db.First(&migrated, 1).Error; err != nil {
		t.Fatalf("load migrated admin: %v", err)
	}
	if migrated.Password == "001008" || !strings.HasPrefix(migrated.Password, "$2") {
		t.Fatalf("password was not migrated to bcrypt: %q", migrated.Password)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(migrated.Password), []byte("001008")); err != nil {
		t.Fatalf("migrated password no longer matches: %v", err)
	}
	if _, _, err := service.Login("lcy", "001008", "127.0.0.1", "test"); err != nil {
		t.Fatalf("login after migration: %v", err)
	}
}

func TestMigrateUsesConfiguredTablePrefix(t *testing.T) {
	db := openTestDB(t)
	if err := db.Table("dont_auth").CreateTable(&Admin{}).Error; err != nil {
		t.Fatalf("create prefixed auth table: %v", err)
	}
	if !db.HasTable("dont_auth") {
		t.Fatal("auth model did not use the configured table prefix")
	}
	if err := db.Table("dont_auth").Create(&Admin{ID: 1, Username: "lcy", Password: "001008"}).Error; err != nil {
		t.Fatalf("insert prefixed legacy admin: %v", err)
	}
	service, err := NewServiceWithTablePrefix(db, "dont_")
	if err != nil {
		t.Fatalf("create prefixed service: %v", err)
	}
	if err := service.Migrate(); err != nil {
		t.Fatalf("migrate prefixed database: %v", err)
	}
	if !db.HasTable("dont_admin_session") {
		t.Fatal("session migration did not use the configured table prefix")
	}
	if _, _, err := service.Login("lcy", "001008", "", ""); err != nil {
		t.Fatalf("login from prefixed legacy table: %v", err)
	}
}

func TestChangePasswordInvalidatesEverySession(t *testing.T) {
	service := NewService(openTestDB(t))
	if err := service.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	first, firstToken, err := service.Setup("admin", "strong-password-one", "", "")
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	_, secondToken, err := service.Login("admin", "strong-password-one", "", "")
	if err != nil {
		t.Fatalf("second login: %v", err)
	}
	if err := service.ChangePassword(first.Admin.ID, "strong-password-one", "strong-password-two"); err != nil {
		t.Fatalf("change password: %v", err)
	}
	for _, token := range []string{firstToken, secondToken} {
		if _, err := service.Authenticate(token); err != ErrInvalidSession {
			t.Fatalf("old session should be invalid, got %v", err)
		}
	}
	if _, _, err := service.Login("admin", "strong-password-two", "", ""); err != nil {
		t.Fatalf("login with new password: %v", err)
	}
}

func TestPasswordBoundariesAndUnchangedPassword(t *testing.T) {
	service := NewService(openTestDB(t))
	if err := service.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	authenticated, _, err := service.Setup("admin", "strong-password-one", "", "")
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := service.ChangePassword(authenticated.Admin.ID, "strong-password-one", "strong-password-one"); err != ErrPasswordUnchanged {
		t.Fatalf("unchanged password error = %v, want %v", err, ErrPasswordUnchanged)
	}
	if err := service.ChangePassword(authenticated.Admin.ID, "strong-password-one", strings.Repeat("a", 73)); err != ErrPasswordTooLong {
		t.Fatalf("long password error = %v, want %v", err, ErrPasswordTooLong)
	}
}
