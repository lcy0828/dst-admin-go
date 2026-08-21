# 游戏通知功能说明

## 页面用途

“游戏通知”只管理真实发送到 DST 游戏内的消息，不再维护没有投递能力的后台公告。

- 即时通知：向所选房间当前运行中的每个分片执行 `c_announce`。
- 操作前提醒：停止、重启、游戏更新和 Mod 重启生效前按房间策略倒计时。
- 自动化通知：定时任务使用 `notification.send`，并进入同一发送历史。
- 发送历史：保存消息来源、Job、房间及逐分片成功、失败、跳过、取消和目标节点。

## 倒计时行为

- 默认开启，默认提前 60 秒，可配置 10-600 秒。
- 在起始时间、30 秒和 10 秒节点中发送适用的提醒。
- 仅检测到在线玩家时等待；没有运行中分片时立即继续操作。
- 多房间游戏更新使用一条统一时间线，不会按房间串行累加等待时间。
- 投递失败会记录但不会阻止维护；取消 Job 会同时取消倒计时和后续操作。

## 部署与分片

投递通过 Placement 感知的 Runtime Driver 完成，因此本机、Agent 和混合部署使用同一行为。已停止的分片不会收到命令，会在历史中标记为“跳过”。

## API

```text
GET  /api/v2/game-notifications
POST /api/v2/game-notifications
GET  /api/v2/rooms/:roomId/game-notification-policy
PUT  /api/v2/rooms/:roomId/game-notification-policy
```

旧 `/api/v2/announcements` 路由不再注册。旧公告代码和表仅暂留用于识别历史数据，不参与新功能。
