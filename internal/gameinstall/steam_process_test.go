package gameinstall

import (
	"path/filepath"
	"testing"
)

func TestSteamUpdateConflictsOnlyWithSameInstallation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "server")
	for _, test := range []struct {
		args []string
		want bool
	}{
		{[]string{"steamcmd", "+app_info_print", "343050"}, false},
		{[]string{"steamcmd", "+app_update", "343050"}, true},
		{[]string{"steamcmd", "+force_install_dir", root, "+app_update", "343050", "validate"}, true},
		{[]string{"steamcmd", "+force_install_dir", root + "-other", "+app_update", "343050"}, false},
		{[]string{"steamcmd", "+force_install_dir", "server", "+app_update", "343050"}, true},
	} {
		if got := steamUpdateConflicts(test.args, root, filepath.Dir(root)); got != test.want {
			t.Fatalf("args=%v got=%v want=%v", test.args, got, test.want)
		}
	}
}
