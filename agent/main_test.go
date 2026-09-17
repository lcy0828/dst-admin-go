package agent

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// Unit tests use explicit fake native runtimes. Container-profile tests
	// override this value per test, keeping the production guard covered.
	_ = os.Setenv("DST_ADMIN_AGENT_DEPLOYMENT", "native")
	os.Exit(m.Run())
}
