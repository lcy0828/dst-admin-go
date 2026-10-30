package servicelogs

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"strings"
	"testing"

	"dont/shared"
)

const barrierCommandLog = `[TMUX] 已发送命令到服务器: dstserver_test, 命令: local r=rawget(_G,"DSTAdmin"); local b=r~=nil and r.Barriers or nil; if b==nil or b.Cancel==nil or b.Cancel("hot-test")~=true then print("[DST-ADMIN-RUNTIME ERROR] code=SNAPSHOT_BARRIER_Cancel_FAILED") end, 耗时: 16ms`

func TestLegacyLogSeverityIgnoresPayloadAndHonorsExplicitLabels(t *testing.T) {
	for _, tc := range []struct {
		name, message, level string
	}{
		{"barrier command sent", "2026/10/29 16:40:24 " + barrierCommandLog, "info"},
		{"command being sent", `[TMUX] 发送命令 会话名: test, 命令: print("ERROR WARN DEBUG 失败 错误")`, "info"},
		{"command with severity prefix", `[TMUX] 构建启动命令: ERROR_HANDLER=1 server`, "info"},
		{"room name", `[Rooms] 房间已保存: ERROR-失败房间`, "info"},
		{"path", `[Runtime] 使用日志目录: /opt/dst/ERROR/WARN/DEBUG`, "info"},
		{"output", `[Runtime] 查询完成: {"message":"error: not a service failure"}`, "info"},
		{"error identifier", `ERROR_HANDLER initialized`, "info"},
		{"warning identifier", `WARNINGS_ENABLED=false`, "info"},
		{"debug identifier", `[DEBUGGER] ready`, "info"},
		{"normal message mentions errors", `[Logs] 错误日志已清理: 10`, "info"},
		{"explicit info", `[INFO] command contains ERROR and 失败`, "info"},
		{"component info", `[TMUX INFO] 命令: print("ERROR")`, "info"},
		{"explicit warning with failure", `[TMUX][警告] 优雅停止服务器失败: ERROR timeout`, "warn"},
		{"english warning", `[TMUX][WARNING] stop failed: error`, "warn"},
		{"bare warning", `warning: retry after ERROR`, "warn"},
		{"chinese warning", `警告：检查服务器状态失败`, "warn"},
		{"explicit debug", `[TMUX][DEBUG] 命令: print("ERROR")`, "debug"},
		{"bare debug", `DEBUG command includes 失败`, "debug"},
		{"debug component error", `[DEBUG][错误] 初始化tmux失败: EOF`, "error"},
		{"level before component", `[ERROR][TMUX] 会话没有窗口: test`, "error"},
		{"actual tmux failure", `[TMUX][错误] 发送命令失败: exit status 1`, "error"},
		{"error without failure keyword", `[TMUX][错误] 会话没有窗口: test`, "error"},
		{"actual runtime error", `[DST-ADMIN-RUNTIME ERROR] code=SNAPSHOT_BARRIER_Cancel_FAILED`, "error"},
		{"english component error", `[Agent] ERROR: connection refused`, "error"},
		{"bare error", `error: connection refused`, "error"},
		{"unlabelled failure", `[Agent] 读取消息失败: EOF`, "error"},
		{"unlabelled failure with fallback", `读取配置文件失败，使用临时 Agent ID: test`, "error"},
		{"unlabelled error", `HTTP服务关闭错误: EOF`, "error"},
		{"successful configuration result", `[ModConfig][INFO] room_id=ERROR_ROOM error=<nil>`, "info"},
		{"failed configuration result", `[ModConfig][ERROR] room_id=test error=conflict`, "error"},
		{"http request failure", `[PlayersHTTP][ERROR] request_id=req-test path=/rooms/test error=EOF`, "error"},
		{"panic", `panic: runtime error`, "error"},
		{"fatal runtime error", `fatal error: concurrent map writes`, "fatal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entry := legacyEntry(tc.message)
			if entry.Level != tc.level {
				t.Fatalf("level = %q, want %q: %s", entry.Level, tc.level, entry.Message)
			}
			want := strings.TrimSpace(legacyTime.ReplaceAllString(tc.message, ""))
			if entry.Message != want {
				t.Fatalf("log content changed: %q", entry.Message)
			}
		})
	}
}

func TestStandardAndChildLogCapturePersistAccurateSeverity(t *testing.T) {
	root := t.TempDir()
	closeLogs := Start(Config{Root: root, Component: "controller", Console: io.Discard}, false)
	t.Cleanup(closeLogs)
	log.Print(barrierCommandLog)
	log.Print(`[TMUX][错误] 发送命令失败: exit status 1`)
	log.Print(`[TMUX][警告] 检查服务器状态失败: retrying`)

	child := NewChildWriter("agent", "test", "child-boot")
	_, _ = child.Write([]byte(barrierCommandLog + "\n[DST-ADMIN-RUNTIME ERROR] code=SNAPSHOT_BARRIER_Cancel_FAILED\n"))
	for _, entry := range []shared.ServiceLogEntry{
		{Level: "info", Code: "TEST_INFO", Message: "ERROR is a quoted command value"},
		{Level: "warn", Code: "TEST_WARNING", Message: "命令执行失败，等待重试"},
		{Level: "error", Code: "TEST_ERROR", Message: "Transport completed but execution failed"},
	} {
		data, err := json.Marshal(entry)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = child.Write(append(data, '\n'))
	}
	child.Flush()
	// Closing the writer flushes both ordinary logs and captured child output.
	closeDefault()

	page, err := Query(context.Background(), root, shared.ServiceLogQuery{})
	if err != nil {
		t.Fatal(err)
	}
	sent := map[string]bool{}
	levels := map[string]string{}
	for _, entry := range page.Items {
		if entry.Message == barrierCommandLog {
			if entry.Level != "info" {
				t.Fatalf("successful command persisted as %q", entry.Level)
			}
			sent[entry.Component] = true
		}
		if entry.Code != "" {
			levels[entry.Code] = entry.Level
		}
	}
	if !sent["controller"] || !sent["agent"] {
		t.Fatal("missing successful command capture", sent)
	}
	for code, want := range map[string]string{"TEST_INFO": "info", "TEST_WARNING": "warn", "TEST_ERROR": "error"} {
		if levels[code] != want {
			t.Fatalf("structured severity changed for %s: %q", code, levels[code])
		}
	}
	for level, want := range map[string]int{"error": 3, "warn": 2} {
		page, err := Query(context.Background(), root, shared.ServiceLogQuery{Level: level})
		if err != nil || len(page.Items) != want {
			t.Fatalf("%s filter: %d records, want %d: %v", level, len(page.Items), want, err)
		}
	}
}
