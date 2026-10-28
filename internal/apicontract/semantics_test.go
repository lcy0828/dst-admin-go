package apicontract

import (
	"encoding/json"
	"strings"
	"testing"

	apidocs "dont/docs"
	"github.com/google/jsonschema-go/jsonschema"
)

func TestSemanticCatalogCoverageAndContractDrift(t *testing.T) {
	c, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range c.Operations() {
		if op.Callable() && op.Semantics() == nil {
			t.Errorf("missing %s", op.ID)
		}
	}
	var source map[string]any
	if err := json.Unmarshal(apidocs.AIToolSemantics, &source); err != nil {
		t.Fatal(err)
	}
	entries := source["operations"].(map[string]any)
	for _, kind := range []string{"missing", "unknown", "bad-type"} {
		t.Run(kind, func(t *testing.T) {
			copy := map[string]any{}
			for k, v := range entries {
				copy[k] = v
			}
			switch kind {
			case "missing":
				delete(copy, "getRoomPlayerInventory")
			case "unknown":
				copy["removedOperation"] = copy["getRoomPlayerInventory"]
			case "bad-type":
				copy["getRoomPlayerInventory"] = map[string]any{"purpose": 42}
			}
			raw, _ := json.Marshal(map[string]any{"schemaVersion": 1, "locale": "zh-CN", "operations": copy})
			fresh, err := Parse(apidocs.OpenAPI)
			if err != nil {
				t.Fatal(err)
			}
			if err := fresh.loadSemantics(raw); err == nil {
				t.Fatal("accepted stale or malformed metadata")
			}
		})
	}
}

func TestChineseDiscoveryScenarios(t *testing.T) {
	c, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	// Task-oriented questions, not copies of the contributor's benchmark.
	cases := []struct{ query, want string }{
		{"服务器刚崩溃，读一下日志末尾", "getWorldLog"},
		{"洞穴启动失败，看看退出事件", "listRoomRuntimeEvents"},
		{"我想每天凌晨定时备份这个房间", "configureRoomBackupSchedule"},
		{"暂停房间的自动备份计划", "configureRoomBackupSchedule"},
		{"这个房间现在设置了哪些备份计划", "listRoomBackupSchedules"},
		{"立即做一次手动备份", "createRoomBackup"},
		{"这次备份任务到底成功没有", "getJob"},
		{"找一下现有的备份存档", "listRoomBackups|listDistributedBackupSets"},
		{"查这个玩家背包和装备", "getRoomPlayerInventory"},
		{"这个玩家最近做过哪些事情", "listRoomPlayerActivity"},
		{"查看玩家去过的地方和移动轨迹", "getRoomPlayerTrajectory"},
		{"这个人参与打过哪些Boss", "getRoomPlayerCombat"},
		{"修改所有玩家的坐标采样频率", "updateRoomPlayerActivitySettings"},
		{"离线玩家身上的东西还能查吗", "getRoomPlayerInventory"},
		{"统计蜘蛛巢数量并返回数字", "executeRawRoomCommand"},
		{"读取刚才Lua的print输出", "getCommandRun|listCommandRuns|executeRawRoomCommand"},
		{"把在线玩家踢下线", "kickRoomPlayer"},
		{"查询游戏里之前的聊天", "listRoomChatLogs"},
		{"房间安装了哪些本地模组", "listRoomMods"},
		{"创意工坊按名字搜索模组", "searchMods"},
		{"修改洞穴模组的参数", "applyModConfiguration"},
		{"看看无人更新模组的设置", "getRoomModUpdate"},
		{"检查有哪些mod可以更新", "checkModUpdates"},
		{"给节点安装LuaJIT2兼容版本", "installLuaJITPackage"},
		{"安装DST游戏服务端", "installGameOnRuntime"},
		{"游戏文件校验之前看看受影响的世界", "previewGameInstallationMaintenance"},
		{"管理面板当前是什么版本", "getSoftwareStatus"},
		{"查询Agent有没有新版本", "checkAgentSoftware"},
		{"通知邮件没有收到，查投递记录", "listNotificationDeliveries"},
		{"机器人断线后补读新版通知", "readNotificationEvents"},
		{"查看世界最近一天的性能评分和报表", "getRoomPerformanceReport"},
		{"调整性能报表采样间隔和保留天数", "updateRoomPerformanceSettings"},
		{"把没发成功的Webhook通知再发送一次", "retryNotificationDelivery"},
		{"新建一个邮箱通知渠道", "createNotificationChannel"},
		{"只重新启动Caves这个世界", "runRoomAction"},
		{"采集模块升级失败想恢复模块备份", "rollbackWorldRuntime"},
		{"被误删的房间去哪里恢复", "listRoomRecoveries"},
		{"要把昨晚的备份恢复到房间", "restoreBackup"},
		{"导入存档前先上传分析一下", "uploadSaveImport"},
		{"看所有机器的CPU内存使用情况", "getNodeResources"},
		{"看看世界现在第几天什么季节", "listWorldStates"},
		{"哪些玩家有管理员权限", "getRoomAccessLists"},
		{"查看所有Agent连接状态", "listAgents"},
		{"在指定节点用Steam账号登录", "manageNodeSteamLogin"},
		{"列出自定义Lua指令", "listCommands"},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			found := false
			var ids []string
			for _, op := range c.Search(tc.query, 5) {
				ids = append(ids, op.ID)
				for _, acceptable := range strings.Split(tc.want, "|") {
					found = found || op.ID == acceptable
				}
			}
			if !found {
				t.Errorf("want %s in top 5, got %v", tc.want, ids)
			}
		})
	}
	for _, query := range []string{"getRoomPlayerInventory", "api_getRoomPlayerInventory"} {
		if got := c.Search(query, 1); len(got) != 1 || got[0].ID != "getRoomPlayerInventory" {
			t.Fatal("exact operation ranking", got)
		}
	}
	for _, op := range c.SearchFiltered("备份", 12, MetadataFilter{Access: "read"}) {
		if op.ToolMetadata().Access != "read" {
			t.Fatal("filter ignored", op.ID)
		}
	}
}

func TestResultSchemasAreSelfContainedAndPreserveWireSemantics(t *testing.T) {
	c, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range c.Operations() {
		if !op.Callable() {
			continue
		}
		result, err := op.ResultSchema()
		if err != nil {
			t.Fatalf("%s: %v", op.ID, err)
		}
		for status, response := range result.Responses {
			if response.DataSchema == nil {
				continue
			}
			raw, _ := json.Marshal(response.DataSchema)
			if strings.Contains(string(raw), "#/components/") {
				t.Fatalf("unresolved %s", op.ID)
			}
			var schema jsonschema.Schema
			if err := json.Unmarshal(raw, &schema); err != nil {
				t.Fatal(err)
			}
			if _, err := schema.Resolve(nil); err != nil {
				t.Fatalf("%s %s: %v", op.ID, status, err)
			}
		}
	}
	op, _ := c.Operation("createRoomBackup")
	result, _ := op.ResultSchema()
	if result.DataPath != "data.data" || result.Responses["202"].DataSchema == nil || !strings.Contains(op.ResultSummary(), "202") {
		t.Fatal(result, op.ResultSummary())
	}
	op, _ = c.Operation("downloadBackup")
	result, _ = op.ResultSchema()
	if result.DataPath != "data.resource" || !strings.Contains(result.Notes, "fetched=false") {
		t.Fatal(result)
	}
}

func BenchmarkChineseDiscovery(b *testing.B) {
	c, err := Default()
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		c.Search("服务器刚崩溃，读一下日志末尾", 8)
	}
}

func TestModCompatibilityAnalysisIsDiscoverableAndReadOnlyWithoutLiveLua(t *testing.T) {
	c, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	op, ok := c.Operation("analyzeWorldModCompatibility")
	if !ok || !op.Callable() || op.Mutation() || op.ToolMetadata().Runtime != "any" {
		t.Fatal("diagnostics incorrectly needs a running game or write access", op)
	}
	if !strings.Contains(op.Semantics().Purpose, "不会禁用模组") {
		t.Fatal("read-only diagnostic semantics missing")
	}
}
