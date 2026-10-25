# LuaJIT2 兼容包构建 / Compatibility build

日常安装使用页面内的兼容构建即可。这里提供维护内置包的构建配方，
来源为 [fesily/DontStarveLuaJIT2](https://github.com/fesily/DontStarveLuaJIT2)。
精确提交及子模块版本见 `source.json`，本地差异见两个 `.patch` 文件。

准备一个独立、干净的上游检出目录，切到 `source.json` 指定的提交，并初始化
`luajit`、`vcpkg`、`3rd/frida-gum-src` 子模块。需要 Docker、Git 和 Python 3.9+。

```bash
git clone https://github.com/fesily/DontStarveLuaJIT2.git luajit-source
git -C luajit-source checkout --detach 201ea9bb16ea23e6ead77f733cd1d92c386b2bb8
git -C luajit-source submodule update --init luajit vcpkg 3rd/frida-gum-src
bash deploy/luajit/build.sh ./luajit-source ./dist/dst-admin-luajit2-3.3.2-linux-amd64.zip
```

脚本核对源码版本、应用补丁，在 Debian 12 的 amd64 构建环境中编译并运行不依赖
游戏安装的上游测试。首次构建需要下载依赖并构建 Frida；缓存保留在该检出目录。
默认 4 个编译任务，可用 `CMAKE_BUILD_PARALLEL_LEVEL` 调整。
只打包 `Mod/` 的 Linux 资源与许可，不打包 `config/`、令牌或测试存档。

补丁处理 GCC 12 的 fmt 格式化兼容、ELF 内存范围初始化、虚拟 CPU 计时器、
插件列表的 Lua table 桥接，以及原生 / Lua 两侧对 `-lua_vm_type` 的一致优先级。
Linux 构建排除只实现了 Windows API 的网络模拟 / 延迟补偿插件。

输出 ZIP 和同名 `.manifest.json`。将 ZIP 放入 `internal/luajit/packages/`，
更新 `manifest.json` 后，运行 `go test ./internal/luajit ./agent`，并在独立游戏
安装上验证新建世界、读取存档、Master / Caves、遥测、命令回执和所有提供的 VM 模式。
确定性 ZIP 元数据不代表不同编译器、系统更新或依赖缓存会生成逐字节相同的 ELF。

## English

Use the panel's bundled compatibility build for normal installation. This recipe
is for maintainers. Check out the exact upstream commit in `source.json` in a
dedicated directory and initialize its three submodules, then run the commands
above from the DST Admin repository. Docker, Git and Python 3.9+ are required.

The script verifies source revisions, applies the reviewed patches, builds on
Debian 12 amd64, and runs upstream tests that need no installed game. Dependency
and Frida builds can make the first run slow. The default concurrency is four;
override it with `CMAKE_BUILD_PARALLEL_LEVEL`. Configuration, tokens and saves
are excluded from the package.

The patches cover GCC 12 formatting, ELF memory-range initialization, virtual CPU
timing, Lua table conversion for plugin discovery, and consistent process VM
selection across native and Lua configuration. Windows-only network simulation
and lag-compensation modules are excluded from the Linux build.

Install the generated ZIP and its manifest into `internal/luajit/packages`, run
the Go package and Agent tests, and validate actual world generation, save
loading, Master/Caves, telemetry, console receipts and every offered VM mode
using an isolated game installation. Deterministic ZIP metadata does not imply
identical ELF bytes across compiler, system or dependency-cache changes.
