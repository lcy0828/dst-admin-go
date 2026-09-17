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

## 上游与兼容构建

上游：<https://github.com/fesily/DontStarveLuaJIT2>。
2026-09-16 节点实查最新稳定版为 v3.0.0，上游提交
`2d40d7bb1a00a0bf7ed8cc5bd8b4433d58b1ee89`。原包 `linux_Mod.zip`
为 45,159,683 字节，SHA-256：
`58374ccd18e6e13225e1a3c980a92fb06bfa26a17e7338fe4c78fc7a5ec9b1f3`。

本地合并提交 `a621a55`，Linux 启动兼容修复 `bfcc646`，取消自定义 JIT 参数与
私有契约的提交 `fcdff97`。兼容构建保留 Linux 自动签名修复、原子签名写入、
Debian 12 运行依赖及必要的插件资源。仅在 Frida 对 libc `chdir` 的快速替换
返回 `GUM_REPLACE_WRONG_SIGNATURE` 时回退到普通替换；此条件在测试机原生
Linux 上已复现，普通替换成功，因此保留该修复。

嵌入的兼容 ZIP 位于 `internal/luajit/packages/`，版本 3.0.0，源码 `fcdff97`，
12,534,607 字节，SHA-256：
`49a6278e35db7caaf535f239a7154398e4c6a16aab955a7d8848fdd121a7a6e0`。
使用源码仓库的 `tools/linux/build-debian12.sh` 构建，Frida `COPYING` 放入
`Mod/licenses/frida/`，再用 `tools/linux/package-admin.py` 打包；同步更新
`packages/manifest.json`。ZIP 包含来源和第三方许可，不包含测试 shim 或令牌。

## 2026-09-16 验证

- CMake 53 项测试通过；前端 669 项测试与生产构建通过，相关 Go 包测试通过。
- 浏览器验证节点选择、检查上游、节点下载、默认安装、可选控制端传包、移动端布局，
  以及移除 JIT 开关后的启动选择。
- `192.168.2.23`（原生 Debian 12 amd64）实测官方原包安装在依赖检查处失败，
  原游戏程序摘要不变；新兼容包成功安装到独立游戏副本。
- 同机独立离线测试世界（DST 747465）使用 `-lua_vm_type=jit` 启动，就绪后
  连续运行 **180.8 秒**，定期查询世界状态均正常，退出码 0。另一次短启动调用
  上游 `GameInjector.DS_LUAJIT_get_vm_type_name(0)` 确认当前 VM 为 `jit`。
- 新版管理服务在本地隔离环境启动并观察 180.3 秒，会话接口和 LuaJIT 版本列表正常，退出码 0。
- 同机 Agent 测试覆盖节点下载、SHA 失败、缓存安装、幂等重试和可选控制端传包。
  测试夹具可通过 `DST_ADMIN_TEST_STEAM_API` 指向游戏 Steam 库，或使用 C 编译器
  构造不执行的测试库；真实游戏启动验证使用实际游戏依赖。
- 测试根目录 `/opt/dst-admin-luajit-review-20260916`；原线上 Agent、原房间及
  `/opt/dst/saves` 未替换、未清理。原存档备份为其下的
  `preserved/original-saves.tar`，157,962,240 字节，SHA-256：
  `902702567956fb28e5eb430cb27955a72c812937230d9b3e0b4d7109459fc1c1`。

静态检测的 `ready` 表示文件布局和版本等安装检查通过，实际 VM 启动以进程、
控制台查询和运行日志为证。本次测试范围是独立世界的启动与三分钟运行，未声明
验证用户原存档的全部 MOD、长期运行或性能收益。
