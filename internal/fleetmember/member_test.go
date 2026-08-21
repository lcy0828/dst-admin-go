package fleetmember

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dont/agent"
	legacyserver "dont/server"

	"github.com/go-ini/ini"
)

func memberTestKey(value byte) string {
	return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{value}, 32))
}

func TestPrepareIdentityKeepsNodeIdentityAndRotatesConnection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "member.conf")
	if err := prepareIdentity(path, "wss://one.example/agent", memberTestKey('a'), "node-one"); err != nil {
		t.Fatal(err)
	}
	if err := prepareIdentity(path, "wss://two.example/agent", memberTestKey('b'), "node-one"); err != nil {
		t.Fatal(err)
	}
	configuration, err := ini.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	section := configuration.Section("agent")
	if section.Key("AGENT_UUID").String() != "node-one" || section.Key("SERVER_URL").String() != "wss://two.example/agent" || section.Key("SECURITY_KEY").String() != memberTestKey('b') {
		t.Fatalf("unexpected identity file: %#v", section.KeysHash())
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("identity mode = %o", info.Mode().Perm())
	}
}

func TestPrepareIdentityRejectsNodeIdentityReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "member.conf")
	if err := prepareIdentity(path, "wss://one.example/agent", memberTestKey('a'), "node-one"); err != nil {
		t.Fatal(err)
	}
	if err := prepareIdentity(path, "wss://one.example/agent", memberTestKey('a'), "node-two"); err == nil {
		t.Fatal("node identity replacement unexpectedly succeeded")
	}
}

func TestEmbeddedMemberConnectsToControllerGateway(t *testing.T) {
	t.Setenv("DST_ADMIN_AGENT_SERVER_URL", "")
	t.Setenv("DST_ADMIN_AGENT_SECURITY_KEY", "")
	root := t.TempDir()
	key := memberTestKey('c')
	controllerConfig := filepath.Join(root, "controller.conf")
	if err := os.WriteFile(controllerConfig, []byte("[server]\nSECURITY_KEY = "+key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gateway, err := legacyserver.NewServer(&legacyserver.Config{KeyFile: controllerConfig, SecurityKey: key})
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Stop()
	httpServer := httptest.NewServer(gateway.Handler())
	defer httpServer.Close()

	savePath, serverPath := filepath.Join(root, "saves"), filepath.Join(root, "server")
	for _, path := range []string{savePath, serverPath} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	member, err := New(Config{
		ControllerURL: "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/agent",
		SecurityKey:   key, NodeID: "embedded-member", StatePath: filepath.Join(root, "member"),
		Runtime: agent.RuntimeInstallation{ID: "default", Driver: "native", SavePath: savePath, ServerPath: serverPath, ServerMode: "64"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := member.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer member.Stop()

	deadline := time.Now().Add(5 * time.Second)
	for !member.Connected() {
		if time.Now().After(deadline) {
			t.Fatal("embedded Fleet member did not connect")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, exists := gateway.GetAllAgentInfo()["embedded-member"]; !exists {
		t.Fatalf("registered Agents = %#v", gateway.GetAllAgentInfo())
	}
}
