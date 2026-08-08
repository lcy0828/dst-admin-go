# DST Admin 外部地图渲染器契约

DST Admin 不在 API 进程内解析 Session。生产环境通过一个独立可执行文件渲染地图，API 负责鉴权、Session 路径校验、任务状态、输出验证、原子发布和保留策略。

## 配置

可通过环境变量配置可执行文件：

```text
DST_ADMIN_MAP_RENDERER_PATH=/opt/dst-admin/bin/dst-map-renderer
DST_ADMIN_MAP_PATH=/var/lib/dst-admin/maps
```

也可以在 `conf/app.conf` 中配置：

```ini
[map]
RENDERER_PATH = /opt/dst-admin/bin/dst-map-renderer

[paths]
DST_MAP_PATH = /var/lib/dst-admin/maps
```

`RENDERER_PATH` 可以是可执行文件，也可以是包含 `dst-map-renderer`（Windows 为 `dst-map-renderer.exe`）的目录。未配置时，服务会从 `PATH` 查找该名称。

## 命令行输入

API 使用参数数组启动渲染器，不经过 shell：

```text
dst-map-renderer --input <session-file> --output <staging-directory> --layers <comma-separated-layers>
```

- `--input` 是已验证、只读的 DST Session 文件绝对路径。
- `--output` 是地图根目录内新建的私有暂存目录。
- `--layers` 按请求顺序传入，地形层始终存在。
- 渲染器必须在收到取消信号后尽快退出。
- 标准输出和标准错误会被合并保存，最多保留 512 KiB。

支持的图层及固定输出文件如下：

| 图层值 | 输出文件 |
| --- | --- |
| `terrain` | `terrain.png` |
| `walrusCamps` | `walrus-camps.png` |
| `spawnPoints` | `spawn-points.png` |
| `players` | `players.png` |
| `worldState` | `world-state.png` |

渲染器只需要生成 `--layers` 指定的文件。所有输出必须是非空 PNG，宽高均为 `1-16384` 像素，单个文件不超过 64 MiB，并且各层尺寸完全一致。退出码必须为 `0`。

## 发布与故障处理

渲染成功后，API 会再次解码 PNG 头并校验全部图层，再将整个暂存目录原子重命名为不可变的地图 ID。任何渲染、校验或发布失败都不会覆盖上一次可用地图。

- 每个房间/世界保留最近 3 个成功版本。
- 每个房间/世界保留最近 20 条失败记录用于诊断。
- 服务启动时清理遗留的 `.map-render-*` 暂存项。
- 同一房间/世界同一时间只允许一个生成任务。
- 图片接口需要管理员会话，并使用不可变私有缓存响应。

## 部署检查

1. 使用运行 DST Admin 的系统账号执行渲染器并确认退出码为 `0`。
2. 确认可执行文件不可由低权限用户修改。
3. 确认地图目录仅服务账号可写，反向代理不能直接暴露该目录。
4. 在“部署检查”页面确认地图渲染器与地图目录均显示可用。
5. 用真实 Session 生成五个图层，确认页面尺寸、叠加和下载均正常。

测试环境可以在 `DST_ADMIN_ENV=test` 时设置 `DST_ADMIN_TEST_MAP=memory`。生产环境明确拒绝该适配器。
