# Mod 管理部署与兼容规范

## 1. 范围

`/api/v2` Mod 模块负责 Steam Workshop 元数据、下载、分片配置、UGC/加载状态、更新检查、修复、卸载和 `modoverrides.lua` 配置编辑。所有修改动作通过 Job 执行，终态由认证 SSE 发布。

生命周期状态相互独立：

- `configured`：至少一个分片的 `modoverrides.lua` 包含该 Mod。
- `downloaded`：Steam Workshop content 目录存在且安全可读。
- `installed`：至少一个目标 UGC 目录存在。
- `loaded`：运行中分片的日志确认已加载该 Mod。

不得将“SteamCMD 退出成功”等同于已安装或已加载。

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
- 预览和应用使用同一规范化渲染结果与 revision；过期 revision 返回冲突，绝不静默覆盖。
- 配置修改先创建保护备份，再原子写入；多文件写入失败会回滚已写文件。
- 无语义变化的启停、配置或卸载返回 `NO_CHANGES`，不制造空保护备份。
- 房间级锁串行化列表与修改，页面不会观察到修复或更新中的暂存状态。

## 6. 下载、修复与卸载恢复

- 安装、更新和修复会先把已有 Workshop 缓存原子移动到同文件系统暂存目录。
- 下载失败、取消、缺少目录或缺少安全 `modinfo.lua` 时，删除半成品并恢复原缓存。
- 修复同时暂存目标世界的 UGC 缓存；成功后丢弃旧 UGC，提示重启分片重新安装。
- 卸载先计算真实配置 Diff 和文件变化，再决定是否创建保护备份。
- 只要其他世界或人工 `ServerModSetup` 仍引用 Mod，就保留 Workshop/UGC 文件。
- 删除前验证目标必须位于配置根目录内，且必须是非符号链接目录。

## 7. 验收

后端门禁：

```bash
go test -race ./internal/mods ./internal/httpapi ./routers -count=1
```

前端门禁：

```bash
npm run lint
npm run test
npm run build
```

当前仓库尚未提供浏览器 E2E 脚本。发布前人工浏览器验收需覆盖普通 Go parser Mod、递归依赖、配置预览/应用/回读、Lua fallback 原因，以及 390/768/1024/1440 四档无横向溢出。生命周期自动测试覆盖下载失败恢复、半成品清理、更新并发隔离、人工 setup 保留和无变化卸载。
