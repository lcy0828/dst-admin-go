package server

import (
	"reflect"
	"testing"
	"time"

	"dont/shared"
)

func TestOnlySystemReportsAdvanceSystemResourceRefresh(t *testing.T) {
	server := &Server{}
	agent := &AgentConnection{AgentID: "worker", Info: map[string]interface{}{}}
	report := func(reportType string, active bool) {
		t.Helper()
		message, err := shared.CreateMessage(shared.TypePassiveReport, "worker", shared.ReportDataPayload{
			ReportID: "test", ReportType: reportType, Data: map[string]interface{}{},
		})
		if err != nil {
			t.Fatal(err)
		}
		if active {
			server.handleActiveReport(agent, message)
		} else {
			server.handlePassiveReport(agent, message)
		}
	}
	report("system_info", false)
	first, ok := agent.Info["_last_system_report_at"].(int64)
	if !ok || first == 0 {
		t.Fatal("system report did not signal completion")
	}
	report("dst_runtime_inventory", false)
	report("process_list", false)
	if agent.Info["_last_system_report_at"] != first {
		t.Fatal("an unrelated report falsely completed system resource refresh")
	}
	report("system_info", true)
	if next := agent.Info["_last_system_report_at"].(int64); next <= first {
		t.Fatal("system report completion must distinguish reports within the same second")
	}
}

func TestRuntimeInventoryReceiptSurvivesUnrelatedReports(t *testing.T) {
	for _, active := range []bool{false, true} {
		for _, reportType := range []string{"system_info", "process_list"} {
			t.Run(reportType+map[bool]string{false: "/passive", true: "/active"}[active], func(t *testing.T) {
				server := &Server{}
				agent := &AgentConnection{AgentID: "worker", Info: map[string]interface{}{}}
				send := func(kind string, active bool, data map[string]interface{}) {
					t.Helper()
					message, err := shared.CreateMessage(shared.TypePassiveReport, agent.AgentID, shared.ReportDataPayload{
						ReportID: "test", ReportType: kind, Data: data,
					})
					if err != nil {
						t.Fatal(err)
					}
					if active {
						server.handleActiveReport(agent, message)
					} else {
						server.handlePassiveReport(agent, message)
					}
				}
				var before int64
				for _, data := range []map[string]interface{}{
					{"error": "inventory unavailable"},
					{"inventory": map[string]interface{}{"protocol_version": float64(1)}},
					{"error": "inventory unavailable again"},
				} {
					send("dst_runtime_inventory", false, data)
					receivedAt, ok := agent.Info["_last_inventory_report_at"].(int64)
					if !ok || receivedAt <= before {
						t.Fatal("each inventory response must advance its own completion marker")
					}
					send(reportType, active, map[string]interface{}{"error": "unrelated error", "inventory": "unrelated value"})
					if agent.Info["_last_inventory_report_at"] != receivedAt {
						t.Fatal("unrelated report changed the inventory completion marker")
					}
					if got := agent.Info["_last_inventory_report"]; !reflect.DeepEqual(got, data) {
						t.Fatalf("inventory result changed: got=%#v want=%#v", got, data)
					}
					before = receivedAt
				}
			})
		}
	}
}

func TestGetAgentInfoMatchesFleetReaderWithoutSharingTopLevelMap(t *testing.T) {
	server := &Server{agents: map[string]*AgentConnection{
		"worker": {LastHeartbeat: time.Unix(123, 0), Info: map[string]interface{}{"hostname": "worker-host"}},
	}}
	info, exists := server.GetAgentInfo("worker")
	if !exists || !reflect.DeepEqual(info, server.GetAllAgentInfo()["worker"]) {
		t.Fatalf("targeted read must preserve fleet reader fields: %#v", info)
	}
	if info["agent_uuid"] != "worker" || info["last_heartbeat"] != int64(123) || info["connected"] != false {
		t.Fatalf("missing connection metadata: %#v", info)
	}
	info["hostname"] = "changed"
	again, _ := server.GetAgentInfo("worker")
	if again["hostname"] != "worker-host" {
		t.Fatal("caller changed stored agent info")
	}
	if info, exists := server.GetAgentInfo("missing"); exists || info != nil {
		t.Fatalf("missing agent returned data: %#v", info)
	}
}

func TestGetAgentInfoDoesNotWaitForOtherAgents(t *testing.T) {
	other := &AgentConnection{Info: map[string]interface{}{}}
	server := &Server{agents: map[string]*AgentConnection{
		"worker": {Info: map[string]interface{}{"hostname": "worker-host"}}, "other": other,
	}}
	other.Mutex.Lock()
	defer other.Mutex.Unlock()
	done := make(chan bool, 1)
	go func() {
		_, exists := server.GetAgentInfo("worker")
		done <- exists
	}()
	select {
	case exists := <-done:
		if !exists {
			t.Fatal("target agent not found")
		}
	case <-time.After(time.Second):
		t.Fatal("targeted read waited for an unrelated agent")
	}
}
