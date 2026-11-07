# AstrBot 业务流程验收

按独立的业务风险验收，不以发送消息条数、近义词数量或测试函数数量衡量覆盖率。同一问题换不同 @ 位置属于输入兼容性，只在自动化测试中保留代表组合。

每次验收先记录前置状态、用户身份、完整消息流程、预期结果，再执行测试。通过必须同时核对回复、实际房间／玩家 ID、调用次数和副作用；仅出现“背包”“成功”等词语不算通过。

## 自动化与真实群各自验证什么

- 自动化使用已安装的 AstrBot 分发流程，替换 QQ、模型与面板 I/O。适合重名、多用户、角色变化、故障注入、并发及写操作，不向真实游戏写入数据。
- 真实群使用登录的用户客户端，验证菜单、选择、引用、普通昵称补充和可读性。观察接口只核对实际消息，不伪造用户输入。
- 模型测试单独记录模型、工具调用、答案来源和耗时。自动化工具调用测试不等于真实模型会正确选择工具。
- 通知链路必须有真实游戏事件才能做端到端验收。仅收到 QQ 回复或建立 SSE 连接，不能证明游戏聊天已经同步。

实群只保留代表流程，不把自动化中的所有排列再次发送。管理写入使用可隔离的测试世界和明确的执行范围；未具备条件时记录“未实测”，不冒充通过。

## 必须覆盖的流程

| 流程与风险 | 操作及验收结果 | 自动化证据 |
| --- | --- | --- |
| 新人从菜单查房 | 菜单 → 选服务器列表 → 选房间；每步接续，直接查询不调用模型 | `test_qq_pipeline` |
| 房间名称未匹配 | 请求带未知房间名 → 选择授权房间；继续原查询，不能猜列表序号代表哪个服 | `test_query_scope` |
| 重名玩家 | 查询重名 → 展示候选 KU → 选择；只能读取选中的玩家 | `test_business_journeys` |
| 多人同时查询 | 两人各自提出问题、补充昵称和追问；候选与最近玩家不串人 | `test_business_journeys` |
| 跨房间继续查询 | 查询甲房间玩家后明确改查乙房间；实际请求中的 roomId、playerId 必须对应乙房间 | `test_business_journeys` |
| 玩家改名或离线 | 引用之前已确认的玩家消息；按稳定 ID 重新读取，显示当前名称和存档来源 | `test_business_journeys` |
| 切换面板后的旧引用 | 数据源切换后引用旧卡片；不得用旧引用定位新面板中的同名 ID | `test_business_journeys`, `test_references` |
| 菜单过期、替换、取消 | 旧序号或引用不得消耗新菜单，不能误选上一个玩家 | `test_companion`, `test_qq_pipeline` |
| 自然取消 | 补充昵称或选择阶段回复“算了，不查了”；结束流程，之后普通昵称不再被当作回答 | `test_business_journeys` |
| 查询过程中取消 | 慢请求尚未结束，或回复已进入发送队列时取消；旧进度和旧结果均不再发出 | `test_business_journeys` |
| 断连后恢复 | 读取失败明确说明未确认；恢复后由新请求读取，失败不变成在线 0 人 | `test_business_journeys` |
| 部分数据读取失败 | 世界正常但玩家／模组读取失败；保留可用信息，缺失数量不能显示为 0 | `test_user_scenarios`, `test_message_display` |
| 房间网络与个人网络 | 查过玩家后问“谁卡”应查房间在线玩家；点名、代词、可信引用可查个人 | `test_query_scope` |
| 指标与背包真实性 | netScore 不换算为 Ping；离线背包保留存档时间，缺失内容不当作空 | `test_companion`, `test_query_scope` |
| AI 将读取误选为写入 | 房间列表、总览、帮助、备份设置等固定只读请求不能调用写工具，包括直接绕过展示层调用 | `test_business_journeys`, `test_task_tools` |
| 群角色权限 | 成员、管理员、群主／维护者分别使用能力；不以群成员自称管理员为准 | `test_group_permissions` |
| 授权在等待期间撤销 | 已排队的操作在实际调用前重新校验，撤权后不执行 | `test_companion`, `test_group_permissions` |
| 管理操作缺参与确认 | 先询问目标；必要确认只能来自用户，模型提供 confirmed=true 不算确认 | `test_workbench` |
| 写入回执丢失与重复投递 | 同一 QQ 请求重复到达、回执丢失后只查询原结果；写入次数仍为一次 | `test_item_grant`, `test_companion` |
| 插件恢复追踪 | 恢复原任务的进度与结果，不能重新创建操作或重复发送完成通知 | `test_companion`, `test_notification_recovery` |
| 通知慢读取隔离 | 玩家详情读取缓慢时，已排队聊天仍可发送 | `test_notification_latency` |
| SSE 重连与重复事件 | 重连、事件修订、重复记录只投递一次；保存游标失败不能假装已确认 | `test_chat_stream` |
| 通知投递结果不确定 | QQ 发送超时后不换另一发送入口重试，不造成双份通知 | `test_broadcast_bridge` |
| 隐私与订阅变化 | 私聊、未订阅房间不播报；授权变化后不继续推进或投递旧队列 | `test_chat_stream`, `test_unified_plugin` |
| 自定义模板 | 不丢失业务正文和失败说明；玩家文本不能成为 CQ 指令或再次展开模板 | `test_message_templates` |

权限、错误目标、重复写入、将未知结果报告为成功属于阻断项。任何一项失败都需要修复并复测，不能靠其他问法通过来抵消。

## 执行与记录

使用 Python 3.12 和项目支持的 AstrBot 版本安装测试依赖后，在仓库根目录运行：

```bash
python -m unittest discover -s integrations/astrbot/tests -p test_business_journeys.py -v
python -m unittest discover -s integrations/astrbot/tests -v
```

未安装 AstrBot 时可能跳过框架用例，不能据此宣称完整回归通过。测试数据使用示例身份，故障注入只作用于替身。

现场记录放在被忽略的 `.local-docs/`。按“已通过自动化／已通过实群／失败／受条件限制未测”分别记录；附请求、回执关联和限制。真实玩家资料、群记录、机器地址及凭证不放入公开文档或测试。

## 独立对话场景库

`dialogue_scenarios.json` 包含 306 个合成场景（原有 300 个场景加上 6 个身份追问接续场景），覆盖房间发现、运行状态、玩家身份、背包、网络、活动、世界查询、管理、权限、多轮对话、故障、通知设置、自然语言分析与版本兼容。各场景有独立目的与断言，不以同一句话的标点或 @ 位置变体凑数。

`scenario_lab.py` 使用真实 AstrBot 分发与插件工具，把面板、持久化和 QQ 发送隔离为替身；复杂问题通过显式配置的模型桥接进程调用真实模型。管理操作只修改合成数据，所有输入均直接使用自然语言。模型桥接通过 stdin 接收 `messages`、`tools`，通过 stdout 返回 `ok`、`message`、`model`、`provider`、`usage`；凭据由桥接进程管理，不进入场景或测试记录。

```bash
python integrations/astrbot/tests/scenario_lab.py \
  --catalog integrations/astrbot/tests/dialogue_scenarios.json \
  --output .local-docs/dialogue-acceptance/results.jsonl \
  --model-command 'python /path/to/private_model_bridge.py'
```

每次执行保存场景哈希、原始对话、工具参数、实际 API 调用和写入台账。需要真实模型的场景未调用模型即失败；管理请求未等到用户确认、查询错误目标、未知结果冒充成功均为阻断项。验收规则误判只能在保留原结果、说明修改依据后修正，不能通过删掉失败场景或不断重试碰巧成功来验收。

此测试不向真实 QQ 群发送 300 条消息，也不证明真实游戏事件的通知链路；真实群收发和通知仍按上面的独立要求核对。

## 原始接口通用入口验收

`native_dialogue_scenarios.json` 补充 45 个新入口场景，重点覆盖跨房间比较、活动和战斗筛选、网络与性能、实体计数、未知回执、权限、模组配置及常用管理操作。这些场景与旧场景库有交集，用于验证不同协议，不能相加宣称有 351 个不同业务目的。

先启动仅监听回环地址的 Go 测试服务，再把其写入的 URL 传给对话驱动：

```sh
DST_SCENARIO_READY=/tmp/dst-dialogue-url go test ./internal/aicontrol -run '^TestCompanionDialogueServer$' -count=1 -timeout=85m
```

在另一终端读取 `/tmp/dst-dialogue-url`，运行：

```sh
python integrations/astrbot/tests/scenario_lab.py \
  --catalog integrations/astrbot/tests/native_dialogue_scenarios.json \
  --native-gateway http://127.0.0.1:PORT \
  --output .local-docs/dialogue-acceptance/native.jsonl \
  --model-command 'python /path/to/private_model_bridge.py'
```

将 `PORT` 替换为 URL 文件中的端口。结束后请求同一地址的 `/stop` 关闭服务；测试服务也有总时限。

新通用路径使用真实 Go 检索、鉴权、参数校验、确认和回执持久化，以及真实 AstrBot 适配器；只有业务 API 数据和 QQ 投递使用合成替身。旧快捷路径继续使用合成的旧协议传输，协议协商由独立 HTTP 集成测试验证。测试记录同时检查原 API 参数（包括 `query.userId`）和实际写入调用次数，不依赖固定写接口名单；明确的预览单独计数。新旧场景均保留初次失败及修复后结果，不能删除失败记录冒充首次通过。
