package aicontrol

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCurrentFactsCannotReusePriorConversationOrDiscovery(t *testing.T) {
	for _, question := range []string{"2服现在第几天、什么季节、天气，卡不卡", "重启后日志有没有错误", "用控制台读取 TheWorld.state.cycles"} {
		t.Run(question, func(t *testing.T) {
			service, store := fixture(t, nil)
			var count atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				count.Add(1)
				fmt.Fprint(w, `{"choices":[{"message":{"content":"刚刚检查了，第36天，没有错误，控制台执行成功。"}}]}`)
			}))
			defer server.Close()
			config, _ := store.Config()
			config.Endpoint = server.URL
			config.Model = "test"
			if _, err := store.SaveConfig(ConfigInput{Config: config}); err != nil {
				t.Fatal(err)
			}
			out, err := service.Chat(context.Background(), ChatInput{RoomID: "room-a", Messages: []Message{{Role: "assistant", Content: "旧记录第36天"}, {Role: "user", Content: question}}}, func(ChatEvent) {})
			if err != nil || strings.Contains(out.Content, "36") || !strings.Contains(out.Content, "本轮未取得") || count.Load() != 2 {
				t.Fatalf("unsupported answer escaped: %+v %v calls=%d", out, err, count.Load())
			}
		})
	}
}

func TestEvidenceRepairUsesActualCurrentTool(t *testing.T) {
	service, store := fixture(t, backendFunc(func(_ context.Context, name string, _ Arguments, _ string) (json.RawMessage, error) {
		if name != "world_state" {
			t.Fatalf("unexpected %s", name)
		}
		return json.RawMessage(`{"cycles":0,"season":"autumn"}`), nil
	}))
	var count atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch count.Add(1) {
		case 1:
			fmt.Fprint(w, `{"choices":[{"message":{"content":"现在第36天。"}}]}`)
		case 2:
			fmt.Fprint(w, `{"choices":[{"message":{"tool_calls":[{"id":"state","type":"function","function":{"name":"world_state","arguments":"{\"roomId\":\"room-a\",\"worldId\":\"master\"}"}}]}}]}`)
		default:
			fmt.Fprint(w, `{"choices":[{"message":{"content":"本次采集 cycles=0，第1天，秋季。"}}]}`)
		}
	}))
	defer server.Close()
	config, _ := store.Config()
	config.Endpoint = server.URL
	config.Model = "test"
	if _, err := store.SaveConfig(ConfigInput{Config: config}); err != nil {
		t.Fatal(err)
	}
	out, err := service.Chat(context.Background(), ChatInput{RoomID: "room-a", Messages: []Message{{Role: "user", Content: "现在第几天、什么季节"}}}, func(ChatEvent) {})
	if err != nil || !strings.Contains(out.Content, "第1天") || len(out.Calls) != 1 || count.Load() != 3 {
		t.Fatalf("repair failed: %+v %v", out, err)
	}
}

func TestEvidenceDomainsFailuresAndClarification(t *testing.T) {
	e := newTurnEvidence(ChatInput{Messages: []Message{{Role: "user", Content: "现在天数是多少"}}})
	for _, tool := range []string{"list_rooms", "find_api", "room_status", "call_result"} {
		e.record(tool, Invocation{Status: "succeeded"}, nil)
	}
	e.record("world_state", Invocation{Status: "failed"}, nil)
	if len(e.missing()) != 1 {
		t.Fatal("discovery or failed reads satisfied evidence")
	}
	if !e.permitsClarification(ChatInput{}, "你指的是哪个房间？") || e.permitsClarification(ChatInput{}, "当前第36天。你指的是哪个房间？") {
		t.Fatal("unsafe clarification bypass")
	}
	for _, q := range []string{"你好", "你可以做什么", "解释一下天数是什么意思", "how to configure backups"} {
		if len(newTurnEvidence(ChatInput{Messages: []Message{{Role: "user", Content: q}}}).required) != 0 {
			t.Fatalf("ordinary explanation blocked: %s", q)
		}
	}
	e.record("world_state", Invocation{Status: "succeeded"}, nil)
	if len(e.missing()) != 0 {
		t.Fatal("current observation ignored")
	}
}

func TestSingleWorldLifecycleCannotExpandToRoom(t *testing.T) {
	for _, tc := range []struct {
		question, tool, body string
		denied               bool
	}{
		{"重启 Caves", "room_control", `{}`, true},
		{"重启 Caves", "api_runRoomAction", `{"body":{}}`, true},
		{"重启 Caves", "api_runRoomAction", `{"body":{"worldIds":["caves","master"]}}`, true},
		{"重启 Caves", "api_runRoomAction", `{"body":{"worldIds":["caves"]}}`, false},
		{"重启整个房间，包括 Caves", "room_control", `{}`, false},
		{"读取 Caves 日志", "world_logs", `{}`, false},
	} {
		if err := checkChatWorldScope(tc.question, tc.tool, json.RawMessage(tc.body)); (err != nil) != tc.denied {
			t.Fatalf("%+v: %v", tc, err)
		}
	}
}

func TestEvidenceGuardAlsoCoversBudgetSummary(t *testing.T) {
	service, store := fixture(t, nil)
	var count atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if count.Add(1) == 1 {
			fmt.Fprint(w, `{"choices":[{"message":{"tool_calls":[{"id":"list","type":"function","function":{"name":"list_rooms","arguments":"{}"}}]}}]}`)
		} else {
			fmt.Fprint(w, `{"choices":[{"message":{"content":"刚查日志没有错误。"}}]}`)
		}
	}))
	defer server.Close()
	config, _ := store.Config()
	config.Endpoint = server.URL
	config.Model = "test"
	config.Limits = &ChatLimits{MaxToolCalls: 1, MaxRounds: 1}
	if _, err := store.SaveConfig(ConfigInput{Config: config}); err != nil {
		t.Fatal(err)
	}
	out, err := service.Chat(context.Background(), ChatInput{RoomID: "room-a", Messages: []Message{{Role: "user", Content: "看看最近日志"}}}, func(ChatEvent) {})
	if err != nil || !strings.Contains(out.Content, "本轮未取得") || !out.Limited {
		t.Fatalf("summary bypass: %+v %v", out, err)
	}
}
