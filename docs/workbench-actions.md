# 工作台查询、预览与确认

工作台、MCP 和官方 AstrBot 插件使用同一组服务端固定操作。可以按“闪电”“月雹”“隐身”“踏水”“阿比盖尔三级”“冻结附近生物”等具体动作查找，不必记住父命令名称。新增说明不会增加顶层任务工具的数量，也不会自动扩大群权限。

## 查找操作

MCP 任务视图使用 `dst_find`：

```json
{"query":"冻结附近生物"}
```

返回至多三个已授权候选。其中 `workbench.act_nearby_entities` 会提示 `action: "freeze"`，并列出还需提供的玩家和半径。目录中的 `actions` 仅包含名称、搜索词、说明和固定枚举建议，不含可下载执行的代码。服务端执行时重新校验固定命令、参数与权限，不信任调用者声称的风险或只读属性。

支持新版目录的插件可把明确指令交给固定操作处理；否定、条件、讨论、多动作等复杂表达继续交给 AI。AI 也不能代替用户完成危险操作确认。旧插件可继续使用父能力及其参数，新子动作的自动预填需要更新插件。

## 查询玩家模式

调用 `dst_world`，数据仅来自指定世界的本次观测：

```json
{"room":"示例房间","world":"地面","view":"player_modes"}
```

原生工具为 `api_queryRoomWorld`：

```json
{"path":{"roomId":"ROOM_FROM_LIST","worldId":"WORLD_FROM_LIST","queryId":"player_modes"}}
```

成功结果的 `data` 示意：

```json
{"kind":"player_modes","players":[{"id":"KU_EXAMPLE","invincible":"enabled","creative":"disabled","panelGod":"disabled","panelInvincible":"enabled"}],"count":1}
```

- `invincible`、`creative`：游戏当前可观测的免伤和自由制作状态。
- `panelGod`、`panelInvincible`：当前角色上由面板请求的综合无敌和普通免伤。
- `enabled` / `disabled` / `unknown`：已开 / 已关 / 无法确认。没有字段、旧记录或未知来源不能当成关闭。

应一并检查外层 `status`、`complete` 和 `observedAt`。只读查询不切换模式、不保存世界、不启动维护任务。玩家迁移中不推断其所在世界。

综合无敌沿用原入口 `api_setRoomPlayerGodMode`：免伤、持续补满三维、体温湿度保护和移动保护。普通免伤使用 `workbench.set_player_invincibility`，参数为 `player_id` 和明确的 `enabled`，不补满属性、不改变碰撞。两者均属于游戏管理操作。

关闭时仅解除本面板持有的效果；原本已无敌、原本已自由制作，或模组随后替换的方法与碰撞范围会被保留。已补满的属性、已经制作的物品不会撤回。发现来源冲突或没有可靠的开启记录时返回 `MODE_OWNERSHIP_UNCERTAIN`，请核对当前状态，不能声称已恢复原状。其他模组直接写入与面板相同的布尔值，无法可靠区分来源；隐身的外观等历史开关也不保证能恢复第三方状态。当前不自动跨登录、换角色或换世界重新开启。

## 附近操作先预览

首批支持灭火 `extinguish`、冻结 `freeze`、催眠 `sleep`、平息仇恨 `pacify`、删除 `delete` 和击杀 `kill`。这些操作即使省略 `phase`，也只做预览。预览仍要求游戏管理权限，不向普通查询用户开放。

开发者使用 `api_executeBuiltinRoomCommand` 预览：

```json
{
  "path":{"roomId":"ROOM_FROM_LIST"},
  "body":{"commandId":"act_nearby_entities","worldId":"WORLD_FROM_LIST","arguments":{"player_id":"KU_EXAMPLE","action":"freeze","radius":10,"prefab":"spider","phase":"preview"}},
  "requestId":"preview-example-001"
}
```

确认回执包含 `batch.state: "preview"`、`previewId`、中心坐标、符合条件的对象数量，以及 `expiresIn: 30`。此时没有修改游戏。

面板显示“确认本次预览”，插件要求用户在30秒内直接回复“确认”。可信客户端执行时使用相同参数，将 `phase` 改为 `execute`，传入原 `preview_id`，外层 `confirmed: true`，并为执行步骤分配独立的请求编号。原生任务 MCP 对危险执行要求转到面板或可信插件确认，不能通过模型填写 `confirmed` 绕过。

预览绑定接入身份、房间、世界、玩家实例、原中心与目标对象。玩家移动不会改变中心；后来进入范围的对象不加入旧预览。换世界、重启、对象失效或保护条件变化会使执行拒绝或跳过。相同预览最多消费一次，部分执行也不能重放。机器人共用密钥时由插件额外绑定群和用户。

取消使用同一参数及 `phase: "cancel"`；插件回复“取消”会清除确认表单，游戏端的只读预览在30秒内过期。批次为短时同步执行，不能打断正在运行的游戏组件方法，也不承诺撤销已发生的效果。

执行结果区分：

| `batch.state` | 含义 |
| --- | --- |
| `preview` | 只预览，尚未修改 |
| `cancelled` | 已取消预览 |
| `completed` | 全部目标效果已确认 |
| `partial` | 已结束，但有跳过、失败或未确认效果 |

`matched` 是预览合格目标数；`attempted` 是实际尝试数；`verified`、`unconfirmed`、`skipped`、`failed` 分别表示已确认、效果未确认、跳过和失败。例如添加寒冷但尚未冻住，会计入 `unconfirmed`，不会称为已冻住。没有有效输出、输出截断、只有发送成功，都不能当成完成；查询原回执，不自动重发。

## 资源限制与兼容

预览最多保留16份，每份30秒；一次空间查询最多接受512个候选、40个合格目标；预览和执行均有4毫秒协作式 Lua 预算。不在后台常驻扫描。游戏引擎的同步空间查询、单次模组组件方法不能被该预算强行中断，因此这些上限不等于实际游戏延迟保证。

其他复杂附近动作保留原有管理员入口和保守确认，没有宣称支持预览；传入 `phase` 会拒绝。它们增加遍历、生成、嵌套查询与耗时上限，达到预算后可能已部分执行，应先检查实际状态。材料按总量提前校验，最多生成40个材料实体；整组物品另限制实际单位数。组件内部的连带掉落不等于面板直接生成数。

本轮模式归属与范围预览需要 Runtime 2.4.21；旧 Runtime 不支持时返回 `WORKBENCH_UPDATE_REQUIRED`，不会降级为无保护 Lua。面板安装更新不会把本说明中的新模式自动应用到正在运行的玩家；已有旧模式缺少归属记录时保守报告无法确认，需要操作者核对。

接口保留原名称和父能力 ID。新增目录字段是可选字段，不写入旧插件严格解析的参数 Schema。行为修订变化会使尚未执行的准备任务失效；已提交的任务仍在当前权限下读取原记录。修改说明文字不会让准备任务失效。面板更新不自动下载安装插件代码。

## 保存地点与传送

新增“地点”页、玩家操作入口和地图选点；同世界传送使用60秒预检与一次确认。完整用法、机器人指令、权限及兼容说明见[保存地点与玩家传送](locations-and-teleport.md)。
