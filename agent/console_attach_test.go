package agent

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"dont/internal/consoledispatch"
	"dont/internal/shards"
)

type fakeConsoleAttachRuntime struct {
	dispatcher *consoledispatch.Dispatcher
	ended      chan error
}

func (f *fakeConsoleAttachRuntime) Status(context.Context, string, string) (shards.RuntimeStatus, error) {
	return shards.RuntimeStatus{State: shards.RuntimeRunning, SessionExists: true}, nil
}
func (f *fakeConsoleAttachRuntime) Start(context.Context, string, string) error { return nil }
func (f *fakeConsoleAttachRuntime) Stop(context.Context, string, string) error  { return nil }
func (f *fakeConsoleAttachRuntime) Send(context.Context, string, string, string) error {
	return nil
}
func (f *fakeConsoleAttachRuntime) ConsoleAttach(_, _ string, readOnly bool) (shards.ConsoleAttachSpec, error) {
	command := []string{"tmux", "attach-session", "-t", "=managed"}
	if readOnly {
		command = []string{"tmux", "attach-session", "-r", "-t", "=managed"}
	}
	return shards.ConsoleAttachSpec{Command: command, InstanceID: "managed@1"}, nil
}
func (f *fakeConsoleAttachRuntime) BeginConsoleMaintenance(ctx context.Context, _, _, owner string) (shards.ConsoleAttachSpec, *consoledispatch.MaintenanceLease, error) {
	if err := f.dispatcher.BindInstance("managed", "managed@1"); err != nil {
		return shards.ConsoleAttachSpec{}, nil, err
	}
	lease, err := f.dispatcher.BeginMaintenance(ctx, "managed", owner, "managed@1")
	spec, _ := f.ConsoleAttach("", "", false)
	return spec, lease, err
}
func (f *fakeConsoleAttachRuntime) EndConsoleMaintenance(_, _ string, lease *consoledispatch.MaintenanceLease) error {
	err := lease.Release()
	f.ended <- err
	return err
}

func TestConsoleAttachProtocolGatesWritableClientWithMaintenanceLease(t *testing.T) {
	root := t.TempDir()
	saveRoot, serverRoot := filepath.Join(root, "saves"), filepath.Join(root, "server")
	worldRoot := filepath.Join(saveRoot, "Cluster_1", "Master")
	for _, directory := range []string{worldRoot, serverRoot} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(saveRoot, "Cluster_1", "cluster.ini"), []byte("[MISC]\nconsole_enabled=true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worldRoot, "server.ini"), []byte("[NETWORK]\nserver_port=10999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runtimeControl := &fakeConsoleAttachRuntime{dispatcher: consoledispatch.New(), ended: make(chan error, 1)}
	a := &Agent{Config: &Config{RuntimeInstallations: []RuntimeInstallation{{ID: "default", Driver: "native", SavePath: saveRoot, ServerPath: serverRoot}}}, shardRuntimes: map[string]shardRuntimeControl{"default": runtimeControl}}
	server, client := net.Pipe()
	done := make(chan struct{})
	go func() {
		a.handleConsoleAttach(server)
		close(done)
	}()
	request := consoleAttachRequest{ProtocolVersion: 1, InstallationID: "default", Cluster: "Cluster_1", Shard: "Master", Owner: "operator", Writable: true, LeaseSeconds: 60}
	if err := json.NewEncoder(client).Encode(request); err != nil {
		t.Fatal(err)
	}
	var response consoleAttachResponse
	if err := json.NewDecoder(client).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if !response.Granted || !reflect.DeepEqual(response.Command, []string{"tmux", "attach-session", "-t", "=managed"}) {
		t.Fatalf("response=%#v", response)
	}
	if health := runtimeControl.dispatcher.Health("managed"); !health.Maintenance || health.MaintenanceOwner != "operator" || health.Accepting {
		t.Fatalf("health=%#v", health)
	}
	if err := json.NewEncoder(client).Encode(consoleAttachRequest{ProtocolVersion: 1, Release: true}); err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
	select {
	case err := <-runtimeControl.ended:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("maintenance lease was not released")
	}
	<-done
	if !runtimeControl.dispatcher.Health("managed").Accepting {
		t.Fatal("dispatcher did not resume")
	}
}

func TestNativeInstallationsReceiveStablePrivateSockets(t *testing.T) {
	root := t.TempDir()
	values, err := normalizeRuntimeInstallations([]RuntimeInstallation{
		{ID: "primary", SavePath: filepath.Join(root, "save-a"), ServerPath: filepath.Join(root, "server-a")},
		{ID: "secondary", SavePath: filepath.Join(root, "save-b"), ServerPath: filepath.Join(root, "server-b")},
	})
	if err != nil {
		t.Fatal(err)
	}
	stateFile := filepath.Join(root, "state", "runtime-state.json")
	values, err = configureNativeConsoleSockets(values, stateFile)
	if err != nil {
		t.Fatal(err)
	}
	if values[0].ConsoleSocket == values[1].ConsoleSocket || !filepath.IsAbs(values[0].ConsoleSocket) || !filepath.IsAbs(values[1].ConsoleSocket) {
		t.Fatalf("sockets=%#v", values)
	}
	for _, value := range values {
		want, pathErr := shards.NativeConsoleSocketPath(value.SavePath)
		if pathErr != nil || value.ConsoleSocket != want {
			t.Fatalf("socket=%q want=%q error=%v", value.ConsoleSocket, want, pathErr)
		}
		if len(value.LegacyConsoleSockets) != 1 {
			t.Fatalf("legacy sockets=%#v", value.LegacyConsoleSockets)
		}
	}
}

func TestRenamedNativeInstallationKeepsOwnershipSocket(t *testing.T) {
	root := t.TempDir()
	saveRoot := filepath.Join(root, "save")
	values := make([]RuntimeInstallation, 0, 2)
	for _, installation := range []RuntimeInstallation{
		{ID: "renamed-before", SavePath: saveRoot, ServerPath: filepath.Join(root, "server-a")},
		{ID: "renamed-after", SavePath: saveRoot, ServerPath: filepath.Join(root, "server-b")},
	} {
		normalized, err := normalizeRuntimeInstallations([]RuntimeInstallation{installation})
		if err != nil {
			t.Fatal(err)
		}
		normalized, err = configureNativeConsoleSockets(normalized, filepath.Join(root, "runtime-state.json"))
		if err != nil {
			t.Fatal(err)
		}
		values = append(values, normalized[0])
	}
	if values[0].ConsoleSocket == "" || values[0].ConsoleSocket != values[1].ConsoleSocket {
		t.Fatalf("sockets=%#v", values)
	}
}

func TestValidateAttachCommandRejectsShellAndNewlines(t *testing.T) {
	for _, command := range [][]string{
		{"sh", "-c", "tmux attach"},
		{"tmux", "attach-session", "-t", "=dst\nother", "extra"},
		{"tmux", "display-message", "attach-session", "-t", "=dst"},
		{"docker", "exec", "-it", "0123456789ab", "tmux", "-S", "/run/dst.sock", "attach-session", "-t", "=dst", "display-message"},
	} {
		if err := validateAttachCommand(command); err == nil {
			t.Fatalf("accepted command=%#v", command)
		}
	}
}

func TestValidateAttachCommandAcceptsFixedNativeAndContainerForms(t *testing.T) {
	for _, command := range [][]string{
		{"tmux", "-S", "/run/dst.sock", "attach-session", "-r", "-t", "=dst"},
		{"tmux", "attach-session", "-t", "=dst"},
		{"docker", "exec", "-it", "0123456789ab", "tmux", "-S", "/run/dst.sock", "attach-session", "-t", "=dst"},
	} {
		if err := validateAttachCommand(command); err != nil {
			t.Fatalf("rejected command=%#v: %v", command, err)
		}
	}
}
