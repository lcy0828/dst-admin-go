# Mod 管理部署与兼容规范

## 1. 范围

`/api/v2` Mod 模块把 Workshop Catalog、运行机器内容和房间配置分开管理。Workshop 的“下载模组”只在用户选定的 Runtime 上执行 SteamCMD，不修改房间；“下载并添加到房间”先实时读取房间所在 Runtime，已有且为当前版本时跳过 SteamCMD，否则在该 Runtime 下载，然后直接更新所选世界的 `modoverrides.lua`。启停和配置保存同样直接合并 Runtime 当前文件并原子写回，不重新发布已有 Mod 内容。下载使用 Job 展示进度，短配置写入同步返回明确结果。

生命周期状态相互独立：

- `configured`：至少一个分片的 `modoverrides.lua` 包含该 Mod。
- `downloaded`：具体 Runtime 安装目录中存在安全可读且含有效 `modinfo.lua` 的内容；Workshop 页面按机器分别展示该状态。
- `installed`：目标机器的 `server/mods/workshop-ID` 本地入口能够读取模组；仅有下载文件、入口缺失时不能显示已就绪。
- `loaded`：运行中分片的日志确认已加载该 Mod。

不得将“SteamCMD 退出成功”等同于已安装或已加载。

作用域固定如下：

- `/mods/library` 是控制器共享的精确制品目录，不按节点或房间区分，也不继承手动 Runtime Target。
- `/rooms/{roomId}/mods` 只列出该房间 `modoverrides.lua` 实际引用的 Mod。
- `configuration_options` 属于单个房间的单个世界；不同房间以及同一房间的地面/洞穴都允许采用不同配置。
- `dedicated_server_mods_setup.lua` 是 DST 自带下载流程的登记文件，不决定房间是否启用。普通启动始终带 `-skip_update_server_mods`，不依靠该文件触发下载。本地加载入口在显式下载、添加或迁移时建立，已有人工模组目录不替换。

世界迁移和精确版本收敛仍使用两阶段发布：

1. `POST /rooms/{roomId}/mod-publications/preview` 读取同一批 Placement 和 `modoverrides.lua`，返回 `planHash`、拓扑版本、目标节点、空间和阻断项。
2. `POST /rooms/{roomId}/mod-publications` 必须回传同一个 `planHash` 作为 `confirmation`。服务端重新构建快照，任何拓扑、配置或内容漂移都会拒绝发布。
3. 创建接口返回持久 Job。客户端等待 Job 终态，再通过 Publication 的 `sourceJobId` 解析最终发布记录；不能把 Job 当成 Publication。`recovery_required` 等未终结状态会在原 Publication 上继续，重试成功后按原 ID 回读；只有 `failed`/`rolled_back` 的重新尝试会创建带新 `sourceJobId` 的 Publication。

正式 Router 启用 Placement 后，Workshop 的纯下载明确选择 Runtime；添加按所选世界的当前 Placement 在目标 Runtime 下载并直接写入配置，不按 `targetId == local` 分叉。启停和配置保存读取并原子写入目标世界，只修改各自拥有的字段；旧 revision 会在写入前被拒绝。迁移等需要跨节点精确版本一致性的流程继续使用 Publication。

普通房间或世界启动直接使用 Runtime 磁盘上的现有文件，不读取发布状态，不扫描或校验 Mod tree，也不自动修复、发布或覆盖 `modoverrides.lua`、`dedicated_server_mods_setup.lua`、`customcommands.lua`、受管 Lua Runtime 资产和房间配置。用户手工修改磁盘后的行为由 DST 本次启动结果决定；失败通过分片 Job 和日志明确展示。世界迁移仍在切换 Placement 后、恢复目标世界运行前执行精确 Mod 收敛，因为迁移本身就是显式的数据搬运事务。

Master 与 Caves 继续并行启动。SteamCMD 维护共享 Workshop 下载目录，同一下载根目录只使用一个可复用会话；世界使用 `<SavePath>/.dst-admin/runtime/workshop/<Room>/<World>` 保存各自的 Steam 状态，不共写 SteamCMD 的 `.acf` 和下载暂存区。`server/mods/workshop-ID` 链接到实际下载内容，不为每个世界复制模组。容器只读挂载模组内容；自定义内容路径也按原路径挂载，以保留链接目标。

### 1.1 旧安装升级

升级到本地加载模式时，在更新 Controller/Agent 前，对每个既有安装显式执行一次迁移。先检查，再建立缺失入口；命令不下载、不改房间配置、不重启世界，重复执行不会覆盖人工目录：

原生发布包和管理容器镜像包含 `mod-local-setup`，源码部署也可以按以下命令编译。使用运行 DST 的同一用户执行，目录替换为该安装的实际配置。

```sh
go build -o mod-local-setup ./cmd/mod-local-setup
./mod-local-setup -server-path /opt/dst/server -workshop-content-path /opt/dst/workshop/steamapps/workshop/content/322330
./mod-local-setup -server-path /opt/dst/server -workshop-content-path /opt/dst/workshop/steamapps/workshop/content/322330 -apply
```

该操作属于管理程序升级，不在世界启动或后台轮询中执行。之后新增下载和缓存添加自动建立入口。运行中的旧世界仍保留原启动参数，待用户自行重启后才使用新的隔离目录。旧受管世界容器仅在已经停止且用户显式启动时重建运行配置，存档和模组仍使用原挂载目录；不会重建运行中的容器。

## 2. 生产配置

| 环境变量 | 说明 | 示例 |
| --- | --- | --- |
| `DST_ADMIN_SAVE_PATH` | DST 集群存档根目录 | `/opt/dst/.klei/DoNotStarveTogether` |
| `DST_ADMIN_SERVER_PATH` | DST 服务端根目录，包含 `mods/` | `/opt/dst/server` |
| `DST_ADMIN_UGC_PATH` | DST UGC 根目录 | `/opt/dst/server/ugc_mods` |
| `DST_ADMIN_STEAMCMD_PATH` | SteamCMD 可执行文件或安装目录 | `/opt/steamcmd/steamcmd.sh` |
| `DST_ADMIN_WORKSHOP_DOWNLOAD` | SteamCMD 下载根目录 | `/opt/dst/workshop` |
| `DST_ADMIN_WORKSHOP_CONTENT` | Workshop content 的 App 目录 | `/opt/dst/workshop/steamapps/workshop/content/322330` |
| `DST_ADMIN_STEAM_APP_ID` | Workshop consumer App ID | `322330` |
| `DST_ADMIN_STEAM_API_KEY` | Steam Web API Key；可选，配置后名称搜索可获得更完整的作者、标签和依赖元数据 | secret |
| `DST_ADMIN_LUA_BINARY` | 外部 Lua fallback 可执行文件；留空或使用默认 `lua` 时会继续自动发现版本化命令和标准安装目录 | `/usr/bin/lua` |
| `DST_ADMIN_PYTHON_BINARY` | 可选 Python/Lupa fallback；仅在外部 Lua 不可用或执行失败后使用 | `/opt/dst-admin/venv/bin/python3` |
| `DST_ADMIN_LUA_PATH` | 可选的 Lua/C 兼容模块搜索目录；fallback helper 已内嵌，不再要求 `modgetinfo.lua` | `/opt/dst-admin/lua-modules` |

`DST_ADMIN_TEST_MODS=memory` 只允许在 `DST_ADMIN_ENV=test` 中使用。生产环境设置该变量会导致启动失败，不能使用测试元数据或伪下载器替代 Steam。

`DST_ADMIN_WORKSHOP_DOWNLOAD` 与 `DST_ADMIN_WORKSHOP_CONTENT` 必须指向同一个 SteamCMD 库。后者必须等于前者下的 `steamapps/workshop/content/{APP_ID}`；配置不一致时服务拒绝启动，避免 SteamCMD 下载成功后到另一个目录校验。

Agent 的节点间精确制品服务默认关闭。只有希望同一可信网络中的 Agent 互相复用已经校验的精确版本时，才在 Agent 配置中同时设置：

```ini
[agent]
MOD_PEER_LISTEN_ADDR = :18081
MOD_PEER_ADVERTISE_URL = http://192.168.2.23:18081
```

`MOD_PEER_ADVERTISE_URL` 必须是其他运行节点可访问的无路径 HTTP(S) 地址，不能写容器内部地址。也可使用 `DST_ADMIN_AGENT_MOD_PEER_LISTEN_ADDR` 和 `DST_ADMIN_AGENT_MOD_PEER_ADVERTISE_URL` 覆盖。Docker 部署必须显式发布对应 TCP 端口；防火墙应只允许可信 Controller/Agent 网络访问，不能直接开放到公网。监听地址和公布地址缺少任意一个都会拒绝启动，避免上报一个实际不可用的 Peer 能力。

## 3. Steam 与依赖

- 名称搜索在未配置 Key 时使用公开 Workshop 搜索页，并通过 `ISteamRemoteStorage/GetPublishedFileDetails` 补全结果。
- Workshop ID 搜索可直接使用公开 `ISteamRemoteStorage/GetPublishedFileDetails`。
- 配置 API Key 后，详情使用 `IPublishedFileService/GetDetails` 并显式请求 children、tags 和 votes；名称搜索使用 `QueryFiles`。
- 依赖按图递归展开，去重并限制为最多 100 个节点，循环引用不会无限递归。
- HTTP 客户端有 12 秒超时、16 MiB 响应上限，并拒绝尾随 JSON。
- SteamCMD 仅通过参数数组调用，不拼接 shell 命令。每次成功返回后仍校验目录与安全的 `modinfo.lua`。

## 4. Lua 兼容策略

默认使用嵌入式 gopher-lua：

- 不开放 `os`、`io`、网络、`dofile` 和 `loadfile`。
- `require`/`modimport` 只能读取当前 Mod 目录内的安全普通 Lua 文件。
- 单次执行 3 秒超时，限制调用栈、注册表、Lua 深度和文件大小。
- 提供 `locale`、`folder_name`、`ChooseTranslationTable` 等常见 `modinfo.lua` 全局量。
- 空结果、循环表、无法无损映射的表键和重复 JSON 键都视为主解析失败并进入 fallback，不能静默丢字段。
- `package.loaded`、循环 `require` 和符号链接逃逸均有明确处理；模块路径越出当前 Mod 目录会被拒绝。

Go 解析失败时才进入兼容 fallback，顺序固定为外部 Lua、Python/Lupa：

- fallback helper 通过 `go:embed` 随 Go 二进制发布，并从标准输入交给配置的 Lua 解释器执行；部署不再依赖外置 helper 脚本。
- `DST_ADMIN_LUA_BINARY` 显式路径优先；否则自动发现 `lua`、`lua5.5` 至 `lua5.1`、`luajit`，并检查 macOS Homebrew 等标准安装目录，不硬编码用户主目录。
- 外部 Lua 不存在或执行失败时，可使用 `DST_ADMIN_PYTHON_BINARY` 指向已安装 `lupa` 的独立 Python 环境；Python 适配器同样内嵌，不复用参考项目中包含 Steam 网络访问的脚本。
- 使用参数数组启动 Lua 二进制；Mod 路径和 ID 通过固定环境变量传入，不拼接 shell。
- 固定最小环境变量和受控 `LUA_PATH`/`LUA_CPATH`。
- 可选 `DST_ADMIN_LUA_PATH` 只扩展 Lua/C 模块搜索目录，不决定 helper 来源。
- helper 提供 `locale`、`folder_name`、`ChooseTranslationTable` 和 `modimport`，使用纯 Lua 生成 JSON。
- Python/Lupa 适配器遇到循环表、超过 100 层、不可表示的键或键转换冲突时会明确失败，不会静默丢字段后宣称兼容。
- 8 秒超时；stdout 上限 8 MiB，stderr 独立限制为 16 KiB；只接受唯一、非空 JSON 对象。
- 返回前会脱敏本地 Mod 路径及疑似 key、token、password、secret 内容。
- API 返回 `parser`、`fallbackUsed`、`fallbackReason` 和 `warnings`，前端明确显示兼容路径。

外部 Lua 提供最高兼容性，但仍会执行第三方 Mod 代码。生产部署应使用低权限专用系统用户，并限制该用户对存档、服务端和网络的权限。gopher-lua 是安全优先的主路径，外部 Lua 是兼容性 fallback，不应调换顺序。

部署检查会返回实际选中的 fallback 类型、绝对路径、发现来源、版本和缺失原因。macOS 未发现运行时时会明确提示 `brew install lua`；系统不会自动安装 Lua、Python 包或修改全局环境。`DST_ADMIN_LUA_PATH` 只是可选模块搜索目录，缺失时会单独告警，但内嵌 helper 不依赖它。

### 4.1 真实服务器兼容矩阵

2026-08-08 在 `192.168.2.12` 已安装的 46 个 Workshop Mod 上执行同一批样本：

| 路径 | 通过 | 结论 |
| --- | ---: | --- |
| gopher-lua 主解析 | 46/46 | 当前样本全部通过 |
| 旧外置 `modgetinfo.lua` 强制 fallback | 37/46 | 旧 helper 不能作为兼容基线 |
| 新内嵌 helper 强制 fallback | 46/46 | 当前样本全部通过 |

该矩阵证明当前真实样本在两条现行解析路径均通过，但不能数学保证未来所有未知 Mod。发布门禁是：保留双路径、未知字段语义无损、失败明确可见，并持续把新增失败样本加入回归集；不得用“100% 兼容”掩盖未测试的未来 Lua 行为。

## 5. 配置与数据保护

- “添加到房间”会先确认目标 Runtime 的实际文件状态；当前版本直接复用，缺失、损坏或过期时才下载，然后更新所选世界的 `modoverrides.lua`。普通启动不会补写或校验该清单。
- 存档导入或远程内容分发确实需要 DST 启动下载清单时，`dedicated_server_mods_setup.lua` 只修改 DST Admin managed block，人工内容保持原样。
- Runtime 磁盘上的 `modoverrides.lua` 是配置真实状态；配置页和配置保存每次按当前 Placement 重新读取，不使用控制器旧副本替代。
- 配置声明也从同一个 Runtime 的 `modinfo.lua` 解析，优先使用手工安装的 `mods/workshop-ID` 目录，否则读取该安装的 Workshop 内容。远程 Agent 通过只读能力 `runtime.mods.schema.v1` 返回解析结果，读取、预览和保存均不要求 Controller 下载副本，也不触发下载或 Publication。
- `modoverrides.lua` 使用语法树更新，未知顶层字段、未知配置项和未知嵌套值按 Lua 值语义保留；文件发生实际修改时会重新序列化 table，不承诺保留注释、空白和原始排版。
- 配置读取与写入始终包含明确的 `roomId` 和 `worldId`；房间级默认配置只写入请求列出的世界，不覆盖其他房间。
- 保存时重新读取 Runtime 当前文件并核对每个世界的 revision；页面读取后若用户又直接修改磁盘文件，过期 revision 返回可展示的冲突，绝不静默覆盖人工内容。
- 本机添加和启停不创建存档备份、不停止或重启世界；配置在该世界下次启动时生效。
- 添加、启停和配置修改按当前 Placement 直接写入目标节点，避免写错机器；只有确实改变存档 generation 的迁移和恢复流程才创建存档级保护。
- 多世界配置按世界原子写入；任一目标失败会明确返回对应世界，已经完成的世界保持已写入状态。
- 普通配置保存与启停每个世界只提交一次 `runtime.configuration.mod.write`（本机直接调用同一文件写入逻辑），不生成配置发布 archive、staging 或 journal。Agent 在写入机器核对文件 SHA256 revision，原子替换后返回结果；需要 Agent 能力 `runtime.configuration.mod-write.v1`。
- 房间列表在同一次响应中附带由当前文件派生的 `profile`，前端复用世界信息并并发读取各机器库存。列表仅解析 `modinfo.lua` 的字面元数据，完整 Lua 配置解析留在编辑器；保存成功后直接采用返回的各世界 revision，不重复读取整页。
- 首页概览和房间模组页使用 `GET /rooms/:roomId/mods?view=facts` 先展示配置和 Runtime 文件状态，再以 `GET /mods/metadata?ids=...` 批量补充 Steam 名称、图片和版本信息，每批最多 100 个 ID。机器库存的 `view=facts` 同时省去跨房间引用扫描。默认完整接口和后台更新检查保持原有语义，不新增缓存或定时任务；Steam 失败显示独立警告，不隐藏列表，也不把未知版本标为最新。切换房间、卸载组件或配置保存后，旧元数据响应不会覆盖新状态。
- 机器模组更新完成后，关联房间的更新检查使用现有独立 Job，不拖延内容更新结果。检查未能提交时保留成功结果并显示警告；检查执行失败由该检查任务单独报告。
- 无语义变化的添加、启停、配置或卸载返回 `NO_CHANGES`，不制造空任务或空保护备份。
- 房间级锁串行化列表与修改，页面不会观察到修复或更新中的暂存状态。
- 一次发布按 `targetId + installationId` 取得独立持久 lease；共享 Installation 的不同房间不会产生相互覆盖的 setup 文件。
- 共享 Installation 上的全部受管房间都会进入同一个期望状态快照；用户主动发布的保护备份范围与该快照一致，避免只发布当前房间时删除其他房间依赖。
- 全部目标 Prepare 成功后才 Publish；commit 前失败逆序回滚，commit decision 持久化后只允许向前恢复，不执行危险的跨节点反向回滚。
- 用户主动发布完成后按所选激活策略处理 `restartRequired`。迁移中的一致性修复不额外重启世界，而是让原迁移流程在同步成功后继续；普通启动不执行一致性修复。

## 6. 分发、恢复与可观察性

- 本地与远程使用同一计划语义。本地由 `moddistribution.Manager` 执行，远程由 Agent 的 `runtime.mods.v1` typed protocol 执行；`runtime.mods.state.v1` 仅用于读取已提交发布状态，`runtime.mods.files.v1` 不依赖发布历史并按需读取实际安装文件，`runtime.mods.fetch.v2` 用于节点本地获取精确内容。
- `EnsureCache` 的已实现顺序是：目标 cache tree SHA 命中时零传输；支持 fetch v2 的 Agent 在隔离暂存目录通过 SteamCMD 下载；失败后尝试最多两个持有相同 tree SHA 的在线 Peer；再从 Controller 精确制品 HTTP Range 端点下载；仅旧 Agent 或所有 HTTP 来源失败时才使用兼容分块上传。
- 节点 Steam 下载不会直接改写运行世界当前使用的 Mod 目录。下载完成后必须校验 tree SHA、总大小和文件数，之后才允许进入两阶段发布；Steam 当前版本与迁移固定版本不同不会被当成成功，也不会在迁移中顺带升级。
- Peer 与 Controller 精确制品都使用临时 Bearer 授权和 HTTP Range，Agent 在自己的 state 目录保留 `.part` 断点文件；连接、响应头和总下载时间均有上限，且拒绝跨主机重定向。授权默认有效 10 分钟，用完由 Controller 主动撤销；token 不写普通日志。
- 旧 Runtime 的 cache bundle 与小型 release plan 仍可使用最多 256 KiB 的 offset 分块协议。请求中的二进制 `data` 不再保留在控制器 7 天命令结果中，避免大 Mod 污染内存。所有来源都必须先校验 bundle 大小与 SHA256，节点导入后再校验 tree SHA、总大小和文件数。
- tree SHA 覆盖路径、类型、规范化 mode、大小和逐文件 SHA，是跨节点内容身份；每个节点仍独立校验自己的 manifest SHA，但 v1 manifest 含本地 `CreatedAt`/元数据，不能把它误作跨节点相等条件。
- 并发获取按 `targetId + installationId + workshopId + treeSHA256` 合并；Master、Caves 共用 Installation 时只准备一次内容。每个安装实例的实际传输仍串行，避免 2C4G 节点同时解包和哈希多个大型 Mod。
- Agent 只接受本地登记的 Installation ID。服务端、存档、cache/state 路径必须是绝对受信目录，符号链接、tar 路径逃逸、硬链接和特殊文件都会被拒绝。
- 控制面重启后自动恢复 `committed`、`completing` 和 `recovery_required` 发布；失败/已回滚记录可从原完整世界配置生成一个新的发布。
- Publication 持久记录目标阶段、保护备份、fence、commit decision、错误和 `sourceJobId`。前端展示节点/Installation 结果，并允许重试可恢复状态。
- 配置读取、配置预览和房间 Mod 列表均按当前 Placement 读取真实节点文件，不再假定所有世界都位于控制器本机。

### 6.1 Peer 直传与迁移边界

Agent 间 Mod 直传已经实现，并保持按需启用：

1. Replica Store 只把 `cached=true` 且 Workshop ID、tree SHA 完全一致的在线节点列为候选，并排除目标节点。
2. Controller 向最多两个候选 Agent 请求仅针对目标节点的 10 分钟授权，目标 Agent 按候选顺序自动回退。
3. Peer 服务从该 Installation 的不可变 cache 生成/复用 bundle，不读取运行中世界目录；HTTP Range 只暴露已授权的 Workshop ID 与 tree SHA。
4. Peer 未配置、不可达或传输失败不会阻断正确性，目标继续尝试 Controller Range 和旧分块兼容路径。
5. 不增加发现轮询或常驻同步任务；添加 Mod、显式发布和迁移预取才会触发。

迁移事务固定 Room revision、拓扑 revision 和精确 Mod tree SHA，在停止源世界之前先完成目标 Mod Prepare；目标存档、配置与 Mod 发布成功并切换 Placement 后才恢复目标世界。目标启动失败会停止目标、回切 Placement、回滚未提交发布，并在源世界原先运行时恢复源世界。

可变存档在源、目标 Agent 都声明 `runtime.migration.peer.v1` 时复用同一 Runtime Peer 端口直传：源节点只暴露 `PrepareExport` 生成的不可变 ZIP，临时 Bearer 同时绑定目标 Agent、Installation、Migration ID、大小、SHA256 和过期时间；目标直接写入现有 import staging，短连接保留已写 offset 并通过 HTTP Range 续传，完整 SHA256 校验后才允许 `CommitImport`。Controller 每 30 秒续期房间和 Mod 事务 lease，不承载制品字节。Peer 未配置、授权失败、不可达或节点明确报告下载失败时，目标先用新幂等操作重新初始化 staging，再回退原有 Controller 分块 relay；若命令已经派发但结果不确定则停止事务并走既有回滚，不并发启动第二条写入链路。单机、本机与旧 Agent 路径没有新增常驻任务或轮询。

UI 固定为三个同级作用域：Workshop 表达发现、目标机器实时下载状态以及“只下载/下载并添加”两种明确操作；机器模组读取所选 Runtime 的实际安装目录；房间模组始终跟随 Placement，按世界显示目标机器和配置状态。全局机器切换不改变房间模组的事实范围。

## 7. 下载、修复与卸载恢复

- 下载、更新和修复会复制已有 Workshop 缓存作为保护快照，同时保留原目录供 SteamCMD 与 ACF 清单核对。
- 下载失败、取消、缺少目录或缺少安全 `modinfo.lua` 时，删除半成品并恢复原缓存。
- 修复同时暂存目标世界的 UGC 缓存；成功后丢弃旧 UGC，提示重启分片重新安装。
- 卸载先计算真实配置 Diff 和文件变化，再决定是否创建保护备份。
- 从房间移除只修改所选世界，不删除节点下载文件；只要其他房间、其他世界或人工 `ServerModSetup` 仍引用 Mod，就保留 setup 记录与 Workshop/UGC 文件。
- 删除前验证目标必须位于配置根目录内，且必须是非符号链接目录。

## 8. 验收

后端门禁：

```bash
go test -race ./internal/mods ./internal/moddistribution ./internal/modpublication ./internal/modcontrol ./internal/httpapi ./routers -count=1
```

前端门禁：

```bash
npm run lint
npm run test
npm run build
```

当前仓库尚未提供浏览器 E2E 脚本。发布前人工浏览器验收需覆盖普通 Go parser Mod、递归依赖、配置预览/应用/回读、Lua fallback 原因，以及 390/768/1024/1440 四档无横向溢出。生命周期自动测试覆盖下载失败恢复、半成品清理、更新并发隔离、人工 setup 保留和无变化卸载。

Debian 12 实机验收已覆盖一个迁移到容器 Agent、控制器本机不再保留 Shard 目录的房间。Workshop `1392778117` 的 110 MB/1433 文件内容完成跨节点发布后，房间列表能返回真实作者、版本、评分与 Placement 状态，配置接口能解析完整中文 schema。随后禁用、启用、配置 `AutoStackedLoot=true` 和移除均取得 `succeeded/full` Job 与 Publication，并以目标 `modoverrides.lua`、setup 托管段和不可变 cache 回读作为完成证据。
