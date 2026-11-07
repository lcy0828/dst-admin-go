package aicontrol

// The same fixture is runnable against the previous source revision and this
// revision, opening one synthetic database across old -> new -> old migrations.
import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jinzhu/gorm"
)

func TestStoreCrossVersionFixture(t *testing.T) {
	root := os.Getenv("DST_UPGRADE_FIXTURE")
	if root == "" {
		t.Skip("explicit isolated cross-version fixture")
	}
	db, err := gorm.Open("sqlite3", filepath.Join(root, "fixture.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.LogMode(false)
	s := NewStore(db, "upgrade_", root)
	if err = s.Migrate(); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("DST_UPGRADE_CREATE") == "1" {
		if _, err = s.SaveConfig(ConfigInput{Config: Config{Revision: "initial", Enabled: true}}); err != nil {
			t.Fatal(err)
		}
		_, secret, e := s.CreateToken(TokenInput{Name: "synthetic scoped caller", Days: 1, Profile: ProfileGameRead, RoomIDs: []string{"fixture-room"}})
		if e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(filepath.Join(root, "synthetic-token"), []byte(secret), 0600); e != nil {
			t.Fatal(e)
		}
		for _, status := range []string{"succeeded", "executing", "uncertain"} {
			row := Call{ID: status, CallKey: status, Source: "fixture-caller", Tool: "api_listRooms", Status: status, Arguments: `{}`, Result: `{"data":{"value":7}}`, CreatedAt: time.Now()}
			if e = db.Table(s.callTable).Create(&row).Error; e != nil {
				t.Fatal(e)
			}
		}
		return
	}
	secret, err := os.ReadFile(filepath.Join(root, "synthetic-token"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Authenticate(string(secret))
	if err != nil || p.Profile != ProfileGameRead || len(p.RoomIDs) != 1 || p.RoomIDs[0] != "fixture-room" {
		t.Fatal("existing authority changed", err)
	}
	for _, id := range []string{"succeeded", "executing", "uncertain"} {
		call, e := s.GetCall(id)
		if e != nil {
			t.Fatal(e)
		}
		want := id
		if want == "executing" {
			want = "uncertain"
		}
		if call.Status != want {
			t.Fatalf("%s became %s", id, call.Status)
		}
		if id == "succeeded" && call.Result != `{"data":{"value":7}}` {
			t.Fatal("receipt changed")
		}
	}
	config, e := s.Config()
	if e != nil || !config.Enabled {
		t.Fatal("config changed", e)
	}
}
