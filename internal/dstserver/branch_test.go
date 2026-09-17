package dstserver

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSteamBranchUsesInstalledManifestAndPreservesTestChannel(t *testing.T) {
	manifest := filepath.Join(t.TempDir(), "appmanifest_343050.acf")
	for _, test := range []struct{ data, branch string }{
		{`"AppState" { "buildid" "24700372" "UserConfig" {} }`, "public"},
		{`"AppState" { "UserConfig" { "BetaKey" "updatebeta" } }`, "updatebeta"},
		{`"AppState" { "UserConfig" { "betakey" "beforemacoschanges" } }`, "beforemacoschanges"},
		{`"AppState" {`, ""},
	} {
		if err := os.WriteFile(manifest, []byte(test.data), 0600); err != nil {
			t.Fatal(err)
		}
		if got := SteamBranch(manifest); got != test.branch {
			t.Fatalf("branch=%q want=%q", got, test.branch)
		}
	}
}
