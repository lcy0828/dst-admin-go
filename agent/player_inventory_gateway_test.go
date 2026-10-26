package agent_test

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"dont/agent"
	"dont/internal/agents"
	"dont/internal/jobs"
	legacyserver "dont/server"
	"dont/shared"

	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

// Exercise the production Agent, encrypted WebSocket transport, capability
// discovery, trusted installation selection and actual read-only save parser.
// No fake command handler or running game is needed for a saved inventory.
func TestPlayerInventoryProductionGatewayMultiNodeAndInstallation(t *testing.T) {
	root := t.TempDir()
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	gateway, err := legacyserver.NewServer(&legacyserver.Config{KeyFile: filepath.Join(root, "gateway.conf"), SecurityKey: key})
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Stop()
	httpServer := httptest.NewServer(gateway.Handler())
	defer httpServer.Close()
	url := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/agent"
	t.Setenv("DST_ADMIN_AGENT_SERVER_URL", url)

	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.LogMode(false)
	db.DB().SetMaxOpenConns(1)
	store := agents.NewStore(db, "inventory_gateway_")
	jobStore := jobs.NewStore(db, "inventory_gateway_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := jobStore.Migrate(); err != nil {
		t.Fatal(err)
	}
	jobService, err := jobs.NewService(jobStore, jobs.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	transport := agents.NewLegacyTransport(func() *legacyserver.Server { return gateway })
	service, err := agents.NewService(store, jobService, transport)
	if err != nil {
		t.Fatal(err)
	}

	const player = "KU_01234567"
	files := map[string]string{}
	workers := make([]func(), 0, 2)
	defer func() {
		for _, stop := range workers {
			stop()
		}
	}()
	ids := []string{}
	for _, node := range []string{"forest", "caves"} {
		dir := filepath.Join(root, node)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		configPath := filepath.Join(dir, "agent.conf")
		if err := os.WriteFile(configPath, []byte("[agent]\nSECURITY_KEY = "+key+"\nSERVER_URL = "+url+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		installations := []agent.RuntimeInstallation{}
		for _, installation := range []string{"default", "second"} {
			saveRoot := filepath.Join(dir, installation, "saves")
			serverRoot := filepath.Join(dir, installation, "server")
			if err := os.MkdirAll(serverRoot, 0700); err != nil {
				t.Fatal(err)
			}
			for _, world := range []string{"Master", "Caves"} {
				path := filepath.Join(saveRoot, "Room", world, "save", "session", "0123456789ABCDEF", player+"_")
				if err := os.MkdirAll(path, 0700); err != nil {
					t.Fatal(err)
				}
				file := filepath.Join(path, "0000000003")
				source := `return {prefab="wilson",data={inventory={items={{prefab="` + node + "_" + installation + "_" + world + `"}},equip={}}}}`
				if err := os.WriteFile(file, []byte(source), 0400); err != nil {
					t.Fatal(err)
				}
				files[file] = source
				worldRoot := filepath.Join(saveRoot, "Room", world)
				for file, content := range map[string]string{
					filepath.Join(saveRoot, "Room", "cluster.ini"): "[NETWORK]\ncluster_name=inventory-test\n",
					filepath.Join(worldRoot, "server.ini"):         "[SHARD]\nis_master=true\nid=1\n",
				} {
					if err := os.WriteFile(file, []byte(content), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			installations = append(installations, agent.RuntimeInstallation{ID: installation, SavePath: saveRoot, ServerPath: serverRoot, ServerMode: "64"})
		}
		worker, err := agent.NewAgent(&agent.Config{ServerURL: url, SecurityKey: key, KeyFile: configPath, OperationStateFile: filepath.Join(dir, "operations.json"), RuntimeInstallations: installations})
		if err != nil {
			t.Fatal(err)
		}
		if err := worker.Start(); err != nil {
			t.Fatal(err)
		}
		workers = append(workers, sync.OnceFunc(worker.Stop))
		var id string
		deadline := time.Now().Add(8 * time.Second)
		for id == "" && time.Now().Before(deadline) {
			items, _, err := service.Agents()
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range items {
				if len(ids) > 0 && item.ID == ids[0] {
					continue
				}
				if item.Status == agents.StatusOnline && item.InstallationRegistrySupported && len(item.Installations) == 2 {
					id = item.ID
				}
			}
			if id == "" {
				time.Sleep(20 * time.Millisecond)
			}
		}
		if id == "" {
			t.Fatal("production Agent did not advertise its installations")
		}
		ids = append(ids, id)
	}
	request := shared.RuntimeOperationRequest{ProtocolVersion: shared.RuntimeOperationProtocolVersion, Action: shared.RuntimeActionPlayerInventory, Cluster: "Room", TopologyRevision: "test-revision", PlayerInventory: &shared.PlayerInventoryRequest{PlayerID: player, Source: "save"}}
	for index, node := range []string{"forest", "caves"} {
		for _, installation := range []string{"default", "second"} {
			for _, world := range []string{"Master", "Caves"} {
				request.OperationID = shared.GenerateUUID()
				request.InstallationID, request.Shard = installation, world
				result, err := service.ExecuteRuntime(context.Background(), "agent:"+ids[index], request, 18)
				if err != nil {
					t.Fatal(err)
				}
				value := result.Result.PlayerInventory
				want := node + "_" + installation + "_" + world
				if value == nil || value.Code != "" || value.Source != "save" || len(value.Inventory) != 1 || value.Inventory[0].Prefab != want || result.Result.InstallationID != installation {
					t.Fatalf("wrong node, installation or world: want=%s result=%+v", want, value)
				}
			}
		}
	}
	request.OperationID = shared.GenerateUUID()
	request.InstallationID = "unknown"
	if _, err := service.ExecuteRuntime(context.Background(), "agent:"+ids[0], request, 18); !errors.Is(err, agents.ErrRuntimeInstallationNotRegistered) {
		t.Fatalf("unregistered installation accepted: %v", err)
	}
	request.InstallationID = "second"
	request.PlayerInventory.PlayerID = "KU_76543210"
	result, err := service.ExecuteRuntime(context.Background(), "agent:"+ids[0], request, 18)
	if err != nil || result.Result.PlayerInventory == nil || result.Result.PlayerInventory.Code != "PLAYER_SNAPSHOT_NOT_FOUND" {
		t.Fatalf("typed parser error lost in transport: %+v %v", result, err)
	}
	workers[1]()
	deadline := time.Now().Add(5 * time.Second)
	for {
		item, err := service.Agent(ids[1])
		if err != nil {
			t.Fatal(err)
		}
		if item.Status == agents.StatusOffline {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("disconnected Agent still online")
		}
		time.Sleep(20 * time.Millisecond)
	}
	request.OperationID = shared.GenerateUUID()
	if _, err := service.ExecuteRuntime(context.Background(), "agent:"+ids[1], request, 18); !errors.Is(err, agents.ErrAgentOffline) {
		t.Fatalf("disconnected read did not fail closed: %v", err)
	}
	for file, source := range files {
		after, err := os.ReadFile(file)
		if err != nil || string(after) != source {
			t.Fatalf("save changed by inventory read: %s %v", file, err)
		}
	}
}
