# 世界性能与报表

入口为 **服务器管理 → 性能与报表**。首页保留世界名称旁的性能信号格，鼠标悬停或键盘聚焦时显示数字评分与采样状态。报表中可选择房间、世界与时间范围，查看评分、CPU 和进程内存曲线、统计及关联运行事件，或导出当前显示的 CSV。

## 评分含义

评分使用 `GetTime()` 的模拟推进量 ÷ 同一 Runtime 输出的采样时间推进量 ÷ `TheSim:GetTimeScale()` × 100，上限 100。例如正常速度下 30 秒真实时间推进了 24 秒模拟时间，得到 80 分。采样时间使用节点的 `os.time()`，并以管理服务的单调观测间隔检查时间跳变；不使用部分游戏构建中随活动执行时间推进的 `GetTimeRealSeconds()`。这是采样窗口内的模拟速度比例，不是 FPS、帧耗时或对卡顿原因的诊断。采样时间精度为秒，短窗口会有少量量化误差。

暂停、停止、休眠、缺少时钟、重启、Session 或运行节点变化及时间不连续的窗口不计分，页面显示对应状态。原游戏红黄绿状态作为原始数据保留。CPU 独立采用同一进程累计 CPU 时间的差分，100% 表示一个逻辑核；内存使用进程 RSS。高 CPU 不直接扣分，也不据此判断游戏卡顿。

## 历史与采集设置

后台默认每 30 秒读取受管世界现有的 Runtime 快照，页面关闭仍持续；不扫描实体、读取完整存档、发控制台查询或刷新全局运行清单。本机直接读取所选世界状态，每个活动房间批次只采集一次进程计数，不依赖页面的运行清单；远程节点只刷新房间实际使用的安装，共享已有缓存和进行中的请求。停止或离线世界不读取游戏快照，暂停期间仍记录可用的 CPU 与内存指标。采集设置按房间保存并动态生效：间隔 15–300 秒，明细保留 1–30 天（默认 7 天），5 分钟汇总保留至多 180 天（默认 90 天，不能短于明细）。关闭采集后保留仍在期限内的历史。

页面在筛选区提供房间、世界与时间范围选择，切换房间保留时间范围。每个世界显示一张统计卡；评分按当前算法的有效时间加权，保留最低评分与低于 80 分的时长，CPU 峰值、内存峰值和记录时长则保留旧算法时期的资源记录。只有历史记录、暂停、停止、休眠或缺少有效采样窗口时说明暂无评分的原因，不补为零分。采样覆盖率表示有记录的时间占查询范围的比例，不表示性能好坏。

不同进程、Session、节点及评分算法的曲线不会跨段连接；评分曲线只展示当前算法，CPU 与内存曲线保留全部资源历史。API 仍按算法版本返回统计，便于外部客户端区分评分口径。长时间查询使用汇总；图表有点数上限，超过时保留最近点，整段汇总不受图表截断影响。查询范围会裁剪到保留期限，实际发生裁剪时页面会提示。

关联事件最多展示所选时间范围最近 100 条运行事件，图中标注其中最近 20 条。规则分析只指出记录到的低速、CPU 接近单核、覆盖不足或非预期退出；不能由时间关联认定某个模组是原因。没有记录的时间不会补造。

新版本 Runtime 会在线更新采集模块，不需要重启世界。旧 Runtime 或旧 Agent 尚未提供模拟时间、游戏速度或累计 CPU 时，相应指标保持未知；原有资源与运行状态仍可使用。首页悬停提示仅在首次采样及连续窗口建立期间显示“待采样”；计时异常、缺少采集字段和休眠分别显示对应状态。

## AI / MCP

`getRoomPerformanceReport`、`getRoomPerformanceSettings`、`updateRoomPerformanceSettings` 共享页面使用的接口。先用 `find_api` 的 `includeSchema=true` 获取完整参数，再传 `path: { roomId: "真实房间ID" }`；查询条件放在 `query`。AI 建议 `maxPoints=60`，优先使用 `worlds` 的汇总与覆盖率。

## English

Open **Server management → Performance reports**. Hover over or focus a world's performance bars on the dashboard to see its numeric score and sampling state. Scores measure simulation progress relative to the producer's wall-clock capture timestamps and game speed, capped at 100; they are not FPS. The controller's monotonic observation intervals guard clock jumps. Capture times have second precision; short windows have some quantization error. The game's active-execution counter is not used as elapsed wall time. Paused, missing and discontinuous intervals have no score. CPU is measured from cumulative process-time differences; 100% is one logical core. RSS is process memory.

The report filters include room, world and time range. Changing rooms preserves the time range. Each world has one summary card: scores use valid intervals from the current algorithm, while CPU, memory and recorded duration retain older resource history. Unscored worlds explain why no score is available. Sample coverage measures recorded time, not performance. Score charts use the current algorithm; resource charts retain all versions. API summaries remain separated by scoring version.

Sampling continues without an open browser, defaults to 30 seconds, and reads existing runtime snapshots only. Per-room settings allow 15–300 seconds, 1–30 days of detail and up to 180 days of 5-minute aggregates. Disabling collection preserves retained history. Summaries cover the retained range even if the plotted rows are limited. Restarts, sessions, placements and scoring algorithms remain separate. Runtime event correlation and deterministic findings do not prove a faulty mod. CSV exports the report's sampled records with their scoring versions.
