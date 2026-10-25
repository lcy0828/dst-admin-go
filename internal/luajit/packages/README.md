本目录为本机 Runtime 和 Agent 内置同一份 Linux x64 兼容包，默认优先选择兼容构建。
LuaJIT2 来自 [fesily/DontStarveLuaJIT2](https://github.com/fesily/DontStarveLuaJIT2)，
原作者为 [fesily](https://github.com/fesily)。

当前兼容包为 **3.3.2 / `201ea9b-debian12`**，基于上游预发布提交
[`201ea9b`](https://github.com/fesily/DontStarveLuaJIT2/commit/201ea9bb16ea23e6ead77f733cd1d92c386b2bb8)，
在 Debian 12 上构建；它不是上游正式稳定版。上游稳定版目录为 **v3.2.0**。
“检查上游更新”仍只读取正式稳定版，不会自动采用后续预发布代码。

兼容构建保留上游 VM 参数，补充 GCC 12 构建兼容、确定的构建时间、内存范围初始化、
虚拟 CPU 的通用 Tracy 计时器、插件列表的 Lua table 桥接，并排除仅实现 Windows API 的原生插件。
显式 `-lua_vm_type` 选择同时应用到原生和 Lua 配置，避免 Mod 默认值在启动时切回 `jit`。
包内 `dst_admin_build.json` 记录上游及子模块提交、补丁摘要；`licenses/` 包含原项目、
LuaJIT、Frida、Lua 5.1 和依赖许可。

构建配方和可复现打包工具见 [`deploy/luajit`](../../../deploy/luajit)。生成 ZIP 后，
将同名 `.manifest.json` 的内容写入本目录 `manifest.json`。CI 会检查内置库不超过
Debian 12 的 GLIBC / GLIBCXX / CXXABI 基线，并拒绝要求可执行栈的内置库。

读取版本目录只读元数据，不展开、下载或安装。安装时由所选节点校验 SHA-256、包结构
和动态依赖，然后事务性替换运行文件；不执行上游安装脚本，不修改用户存档。
控制端传包是独立的可选入口。

English: the local Runtime and Agent embed the same Debian 12 compatibility
build, pinned to upstream preview 3.3.2 at `201ea9b`; stable upstream discovery
remains separate. See the [English installation guide](../../../docs/luajit-installation.en.md)
for supported platforms, installation and save protection.
