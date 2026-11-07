# capreg：机器人能力注册表

`internal/capreg` 是机器人能力的**语言与策略**唯一来源。`catalog.yaml` 通过
`go:embed` 编进二进制，启动时校验；校验失败视为构建缺陷（`Builtin()` 会 panic，测试会先失败）。

注册表**不包含可执行内容**：处理函数、后端操作、控制台脚本、参数结构仍然只在 Go 代码里
（`internal/aicontrol` 的 `BotCapabilities` 与 `internal/console` 的工作台模板）。
注册表里的字段不会进入 `SchemaRevision`，所以改别名、例句、回执文案不会使已冻结的表单失效。

## 条目结构

```yaml
slot_defaults:            # 常见参数名的默认槽位角色；条目内同名声明优先
  worldId: {role: world, ask: never}
  feedback: {role: presentation}

capabilities:
  - id: workbench.skip_days          # 与 BotCapabilities() 的 id 一一对应
    aliases: [跳过一天, 跳天, ...]    # 能叫出该能力的短语（>= 3 条）
    examples: ["跳过一天", ...]       # 完整自然语句（>= 6 条，QQ 口语、有无空格、中文数字）
    negatives: ["跳过这个话题"]       # 不能落到这里的说法（>= 1 条）
    tier: R2                          # R0 只读 | R1 单人可逆 | R2 影响全服/他人 | R3 破坏性
    confirm: once                     # none | once | typed
    prefer: room.reset                # 可选：规范能力。发现时规范能力排在前面
    slots:                            # 参数名 -> 角色
      days: {role: quantity, ask: if_missing}
    receipt: {done: "已跳过 {days} 天", pending: "已提交跳过 {days} 天，等待游戏确认"}
    actions:                          # 可选：带枚举子动作的能力，键为枚举值
      invincible: {aliases: [...], examples: [...], negatives: [...], tier: R1, confirm: none, prefer: player.god_mode}
```

- **槽位角色**：`target_player`、`other_player`、`world`、`item`、`entity`、`quantity`、`toggle`、
  `time_range`、`text`、`location`、`enum`、`presentation`、`internal`。
- **ask**：`if_missing`（无法从语句中抽取且没有默认值时才追问）或 `never`（使用默认值、省略或由服务端填写）。
- **model**：默认 `true`；`presentation` 与 `internal` 固定为 `ask: never`、`model: false`，
  它们仍保留在参数结构里（服务端需要），但不能追问用户，也不交给模型填写。
- **回执占位符**：`{player}`（目标玩家显示名）、`{world}`（世界显示名）和本能力的参数名。

## 风险等级与确认

| 等级 | 含义 | 允许的 confirm |
| --- | --- | --- |
| R0 | 只读（含以工作台实现的只读统计） | none |
| R1 | 只影响一个玩家且可逆（复活、发物品、无敌/制作模式、传送到已存地点） | none / once |
| R2 | 影响全服或其他玩家（跳天、改季节、重启、踢人、生成） | once / typed |
| R3 | 破坏性或不可逆（重置世界、回档、封禁、清背包、删除实体） | typed |

- 等级只决定**是否需要确认**，不决定可见性和权限；控制台自身的执行闸门不变。
- 带子动作的能力：父级取所有子动作的最高等级；子动作未写时继承父级。已知子动作时使用子动作策略。
- 非显而易见的分级在 `catalog.yaml` 中用注释说明理由。

## 新增一个能力

1. 在 Go 中实现配方（`BotCapabilities` 或控制台工作台模板）。
2. 在 `catalog.yaml` 中新增同 id 条目：别名、例句、反例、等级、确认、每个参数的槽位角色、回执。
   与已有能力重复的说法，用 `prefer` 指向规范能力。
3. 运行并更新快照：

   ```bash
   go test ./internal/capreg/... ./internal/aicontrol/...
   go test ./internal/capreg -run Snapshot -update   # 或 UPDATE_SNAPSHOT=1
   ```

覆盖测试会检查：每个能力和每个工作台定义/子动作都有条目且没有孤儿条目；质量门槛；
每个参数都有槽位角色；展示类参数有默认值；不同能力之间没有未声明 `prefer` 的重复别名或例句；
反例不会压制本能力自己的说法。

`testdata/bot_catalog_snapshot.json` 是插件实际收到的完整目录，插件测试应加载它而不是合成目录。
