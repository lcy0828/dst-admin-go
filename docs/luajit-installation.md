# LuaJIT2 安装

**简体中文（默认）** | [English](luajit-installation.en.md)

在 **游戏服务端管理 → LuaJIT2 加速** 中选择版本并安装。版本列表区分上游原版、
兼容构建和自行导入的包。默认由所选 Agent / 本机 Runtime 维护版本、下载、
缓存、校验和安装；控制端派发任务并展示进度。本机 Runtime 使用进程内安装器，
不会额外启动或连接本机 Agent。

“检查上游更新”由当前节点读取 GitHub 最新稳定版的 Linux 包元数据和 SHA-256，
不下载、不安装、不启动后台轮询。点击安装后，节点按所选版本下载并校验；未来
上游沿用当前结构和 VM 接口时，可直接跟随发布，无须增加私有能力文件。
若上游改变包结构、移除接口或提高系统依赖，仍需适配或选择相应兼容构建。

也可以提供 HTTPS 地址与 SHA-256，点击“由运行节点下载”。只有主动上传 ZIP
并选择“控制端传包 / 传送并安装”才由控制端传包；接收后保存在节点缓存。
节点离线或下载失败会明确报错，不会自动改用控制端或其他节点。

安装前停止同一份 DST 安装上的所有世界。安装后可在启动弹窗选择原版 Game Lua、
LuaJIT，或高级设置中的实验性分代 GC。启动参数仅使用上游的
`-lua_vm_type=game|jit|jit_gen`，JIT 编译行为遵循上游配置，不额外提供覆盖开关。
普通启动不下载、修复或替换资源。SteamCMD 更新若覆盖启动入口，可重新安装 / 修复。

当前支持 Linux x64、64 位 DST、Native Runtime（包含常规 All-in-One）。
Windows、macOS、Linux ARM 和独立分片容器暂不提供安装；远程 Agent 和控制端
需使用本次更新后的版本，支持 `runtime.luajit.v2` 和简化后的 `luajit` 启动模式。
旧的 JIT-on / JIT-off 请求会被拒绝，避免默默改变旧请求的含义。

## 校验与存档保护

- 必须明确 `targetId + installationId`，找不到远端不回退到本机。
- ZIP 最大 256 MiB，解压最大 1 GiB；拒绝目录穿越、符号链接、重复文件、
  非 Linux x64 ELF 和缺失运行文件。不要求私有的模式契约文件。
- 下载只接受 HTTPS 来源、HTTPS 重定向和指定 SHA-256；校验失败不发布到缓存。
- 安装时在目标节点检查 ELF 动态依赖，确认系统库版本满足要求后才替换运行文件。
  例如官方 v3.0.0 原包在 Debian 12 缺少较新的 GLIBC / GLIBCXX，需选择兼容构建。
- 控制端可选传包使用相对路径及短时单包凭据，凭据不发送给第三方下载来源。
- 游戏原程序保留为 `_1`；Mod、引导库、定位文件和启动入口整体备份，失败回滚。
  安装事务异常中断时，下次安装会先恢复；未成功恢复的备份不会被丢弃。
- 安装目录的共享 / 独占文件锁协调游戏进程、更新与安装，Master / Caves 可并行运行。
- 安装器不修改房间配置、Mod 列表、存档或令牌，不自动停止或重启世界。

`DST_ADMIN_LUAJIT_RELEASE_DIR` 可分别指定节点缓存目录。本机 Runtime 默认使用
系统用户配置目录的 `dst-admin/luajit-releases`；Agent 默认在操作状态文件旁的
`luajit-releases`。控制端可选上传仓库使用独立的 `controller-transfers/`。
内置兼容 ZIP 仅在显式安装时展开到节点缓存；读取列表仅读取小型元数据。
页面只获取所选节点的列表，只在任务未结束时轮询任务进度。

API：`GET /runtime-targets/luajit` 返回 `installations` 和可选 `transfers`；
`GET /runtime-targets/luajit/status?targetId=…&installationId=…` 读取所选安装。
追加 `refreshUpstream=true` 才由该节点请求 GitHub。包元数据可含 `channel`
（`upstream` / `compatibility`）和 `sourceUrl`。
下载提供 `targetId + installationId + url + sha256`；安装提供
`targetId + installationId + releaseId`，`source` 默认 `runtime`，显式
`controller` 才传包。

## 上游与兼容性

节点查询 [DontStarveLuaJIT2 上游发布](https://github.com/fesily/DontStarveLuaJIT2/releases)，在用户执行安装或更新时下载选定版本。下载、缓存、校验和应用在目标机器完成，也可选择由控制端提供包。

是否能直接使用取决于目标 Linux 的架构、动态加载器、GLIBC/GLIBCXX/CXXABI 和包内文件。版本更新不等于自动兼容：先在目标节点完成依赖检查，失败时保留现有安装并显示原因。较旧发行版可使用系统提供的兼容构建。

系统不要求上游包包含额外的启动模式声明，也不依赖私有启动开关。更新后仍需校验实际游戏启动；游戏本体更新可能覆盖已安装的 LuaJIT 文件，按页面状态重新安装即可。离线兼容包的来源和许可见[内置包说明](../internal/luajit/packages/README.md)。

## 国内网络与代理

默认在所选运行节点直连 GitHub；连接失败时，版本元数据改用 `gh-proxy.com`，安装包改用 `ghfast.top`。GHFast 不支持 GitHub API，因此两者分别处理。代理仅用于公开的 LuaJIT 上游地址，不接收 Agent 密钥、控制端传包凭据或自定义下载地址；下载后仍校验原有 SHA-256、包结构和系统依赖。

无需额外配置。需要固定网络方式时，可在实际运行节点设置可选环境变量 `DST_ADMIN_GITHUB_ACCESS=direct`（只直连）或 `proxy`（只代理），默认 `auto`。它不替代 SteamCMD 的下载网络。离线时仍可使用内置兼容包或主动上传经过校验的 ZIP。
