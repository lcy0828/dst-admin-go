# Mod 管理部署与兼容规范

## 1. 范围

`/api/v2` Mod 模块把控制器 Workshop 内容库、节点安装和房间配置分开管理。控制器内容库负责元数据、下载和更新；房间级能力负责选择使用哪些 Mod、分片启停、UGC/加载状态和 `modoverrides.lua` 配置编辑；Placement 发布把内容与配置原子分发到本机或远程节点。所有修改动作通过 Job 执行，终态由认证 SSE 发布。

生命周期状态相互独立：

- `configured`：至少一个分片的 `modoverrides.lua` 包含该 Mod。
- `downloaded`：Steam Workshop content 目录存在且安全可读。
- `installed`：至少一个目标 UGC 目录存在。
- `loaded`：运行中分片的日志确认已加载该 Mod。

不得将“SteamCMD 退出成功”等同于已安装或已加载。

作用域固定如下：

- `/mods/library` 是控制器共享的已下载内容库，不按节点或房间区分，也不继承手动 Runtime Target。
- `/rooms/{roomId}/mods` 只列出该房间 `modoverrides.lua` 实际引用的 Mod。
- `configuration_options` 属于单个房间的单个世界；不同房间以及同一房间的地面/洞穴都允许采用不同配置。
- `dedicated_server_mods_setup.lua` 属于节点上的 DST 安装目录，保存所有受管房间引用 Mod 的并集；它负责启动时下载，不决定房间是否启用。

房间写操作统一使用两阶段发布：

1. `POST /rooms/{roomId}/mod-publications/preview` 读取同一批 Placement 和 `modoverrides.lua`，返回 `planHash`、拓扑版本、目标节点、空间和阻断项。
2. `POST /rooms/{roomId}/mod-publications` 必须回传同一个 `planHash` 作为 `confirmation`。服务端重新构建快照，任何拓扑、配置或内容漂移都会拒绝发布。
3. 创建接口返回持久 Job。客户端等待 Job 终态，再通过 Publication 的 `sourceJobId` 解析最终发布记录；不能把 Job 当成 Publication。`recovery_required` 等未终结状态会在原 Publication 上继续，重试成功后按原 ID 回读；只有 `failed`/`rolled_back` 的重新尝试会创建带新 `sourceJobId` 的 Publication。

正式 Router 启用 Placement 读取后，旧的房间 add/install/update/enable/repair/uninstall/configuration-apply 写接口返回 `MOD_PUBLICATION_REQUIRED`，防止远程目标失败时退回控制器本地写入。内容库 download/update 接口继续独立可用。

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

- `dedicated_server_mods_setup.lua` 只修改 DST Admin managed block，人工内容保持原样。
- `modoverrides.lua` 使用语法树更新，未知顶层字段、未知配置项和未知嵌套值无损往返。
- 配置读取与写入始终包含明确的 `roomId` 和 `worldId`，修改一个世界不会覆盖其他房间或其他世界。
- 预览和应用使用同一规范化渲染结果与 revision；过期 revision 返回冲突，绝不静默覆盖。
- 配置修改先创建保护备份，再原子写入；多文件写入失败会回滚已写文件。
- 无语义变化的启停、配置或卸载返回 `NO_CHANGES`，不制造空保护备份。
- 房间级锁串行化列表与修改，页面不会观察到修复或更新中的暂存状态。
- 一次发布按 `targetId + installationId` 取得独立持久 lease；共享 Installation 的不同房间不会产生相互覆盖的 setup 文件。
- 共享 Installation 上的全部受管房间都会进入同一个期望状态快照和保护备份范围，避免只发布当前房间时删除其他房间依赖。
- 全部目标 Prepare 成功后才 Publish；commit 前失败逆序回滚，commit decision 持久化后只允许向前恢复，不执行危险的跨节点反向回滚。
- 发布完成只标记 `restartRequired`。系统不会为应用 Mod 自动重启 DST；操作员可在核对在线玩家后使用房间/分片控制。

## 6. 分发、恢复与可观察性

- 本地与远程使用同一计划语义。本地由 `moddistribution.Manager` 执行，远程由 Agent 的 `runtime.mods.v1` typed protocol 执行。
- cache bundle 与 release plan 都以最多 256 KiB 的块传输，支持 offset 续传；传输包校验大小与 SHA256，节点导入后校验 tree SHA、总大小和文件数。tree SHA 覆盖路径、类型、规范化 mode、大小和逐文件 SHA，是跨节点内容身份；每个节点仍独立校验自己的 manifest SHA，但 v1 manifest 含本地 `CreatedAt`/元数据，不能把它误作跨节点相等条件。
- Agent 只接受本地登记的 Installation ID。服务端、存档、cache/state 路径必须是绝对受信目录，符号链接、tar 路径逃逸、硬链接和特殊文件都会被拒绝。
- 控制面重启后自动恢复 `committed`、`completing` 和 `recovery_required` 发布；失败/已回滚记录可从原完整世界配置生成一个新的发布。
- Publication 持久记录目标阶段、保护备份、fence、commit decision、错误和 `sourceJobId`。前端展示节点/Installation 结果，并允许重试可恢复状态。
- 配置读取、配置预览和房间 Mod 列表均按当前 Placement 读取真实节点文件，不再假定所有世界都位于控制器本机。

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
