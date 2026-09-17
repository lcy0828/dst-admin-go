# DST Admin 地图渲染器 v1

`dst-map-renderer` 是随项目发布的独立 Go 二进制。API 进程负责 Session 定位、任务编排、不可变快照、产物校验、原子发布、鉴权和保留策略；渲染进程只负责解析一个只读快照并输出固定的 v1 产物。两者保持独立故障域，异常存档或 MOD 不会把解析状态带入 API 进程。

## 构建与发现

```bash
go build -trimpath -ldflags "-X main.version=$(git describe --tags --always)" \
  -o dist/dst-map-renderer ./cmd/dst-map-renderer
```

推荐把 `dst-admin` 和 `dst-map-renderer` 放在同一目录。API 按以下顺序发现渲染器：

1. `DST_ADMIN_MAP_RENDERER_PATH` 或 `[map] RENDERER_PATH` 指定的文件/目录；显式路径无效时失败关闭，不回退到其他二进制。
2. `dst-admin` 可执行文件所在目录。
3. 当前 `PATH` 中的 `dst-map-renderer`。

地图目录通过 `DST_ADMIN_MAP_PATH` 或 `[paths] DST_MAP_PATH` 配置。服务账号必须能创建该目录及其中的暂存目录。
渲染器还会从 `DST_SERVER_PATH` 定位当前安装版本的官方 `data` 目录；它接受 data 目录、游戏根目录、macOS `.app` 或其上级 `Don't Starve Together` 目录。官方资源只在本机读取，不会复制到地图产物或项目发行包。

```ini
[map]
RENDERER_PATH = /opt/dst-admin/bin/dst-map-renderer

[paths]
DST_MAP_PATH = /var/lib/dst-admin/maps
```

## 能力握手

API 在启用地图功能前执行一个 3 秒、有大小上限的探针：

```text
dst-map-renderer --probe --assets <dst-installation-or-data-directory>
```

Renderer v1 必须返回 JSON，并声明 `protocolVersion: "1"`、非空版本号以及四个固定产物。允许新增字段，以便兼容后续能力扩展。

```json
{
  "protocolVersion": "1",
  "rendererVersion": "v1.0.0",
  "capabilities": {
    "inputFormats": ["lua-session", "klei-text-v1", "klei-base64-deflate-v1"],
    "artifacts": ["terrain.png", "icons.png", "manifest.json", "features.json"],
    "maxInputSize": 134217728
  }
}
```

部署检查和地图列表使用同一握手结论，不会把“文件存在但协议不兼容”误报为可用。

## 生成协议

API 不经 shell，以参数数组启动进程：

```text
dst-map-renderer \
  --input <read-only-session-snapshot> \
  --output <empty-private-directory> \
  --assets <dst-installation-or-data-directory> \
  --layers terrain,features,worldState \
  --timeout <duration>
```

- Session 先复制为 `0440` 的不可变快照，并计算 SHA-256；渲染期间原存档变化不会污染产物。
- 子进程只继承运行所需的最小环境变量，不继承应用 Token、密码等环境。
- API 默认强制 90 秒超时，最高允许配置为 10 分钟；取消任务会终止子进程。
- 标准输出与错误最多保存 512 KiB，路径、常见 secret 和终端控制字符在持久化前会脱敏。
- v1 总是生成完整结构化产物，只接受 `terrain/features/worldState` 三个逻辑层；其他图层值直接拒绝。

固定产物：

| 文件 | 用途 | API |
| --- | --- | --- |
| `terrain.png` | 地形底图 | `GET /api/v2/maps/{mapId}/images/terrain` |
| `icons.png` | 本次地图使用的官方小地图图标精灵 | `GET /api/v2/maps/{mapId}/images/icons` |
| `manifest.json` | 尺寸、坐标变换、世界状态、统计和 warning | `GET /api/v2/maps/{mapId}/manifest` |
| `features.json` | 玩家、出生点、资源、建筑及未知 MOD prefab | `GET /api/v2/maps/{mapId}/features` |

`manifest.sourceSha256` 必须等于 API 快照哈希。四个文件必须恰好存在、是普通非链接文件并满足大小上限；PNG 尺寸、Feature 数量、图标坐标、协议版本和图层描述必须互相一致。校验完成前不会发布任何文件。

## Session 与 MOD 兼容

内置解析器支持普通 Lua Session、`KLEI     1` 文本以及 `KLEI0001 + Base64 + 16-byte header + raw Deflate`。Lua 运行在受限 `gopher-lua` 环境中，带执行超时、输入/解压/JSON 上限以及属性深度和数量限制。

Renderer 从当前游戏的 `scripts/tiledefs.lua` 读取官方 Tile 顺序，并使用 Session 的 `world_tile_map` 解释实际 ID；地形由官方 KTEX 纹理、`map_edge` 遮罩和道路纹理生成。未知 MOD prefab 不会被丢弃，原始 prefab、坐标、分类和受限属性会写入 `features.json`；能匹配官方图标的实体还会携带 `icons.png` 精灵坐标。未知 MOD Tile 使用由 ID 稳定派生的备用色并写入 warning。Renderer 不依赖图鉴数据库，因此 `独立图鉴目录` 或未来 Catalog 服务不可用时仍能生成地图，但缺少当前 DST 官方资源时会明确失败。

## 任务状态与故障处理

地图记录依次进入 `snapshot -> renderer -> validate -> publish -> complete`。服务重启会把残留 `running` 记录标记为 `interrupted`，启动时清理 `.map-render-*` 暂存项。

- 同一房间/世界同时只允许一个生成任务。
- 每个房间/世界默认保留最近 3 个成功版本和 20 条失败诊断。
- 发布使用目录原子重命名；任何失败都不会覆盖上一个可用版本。
- 成功记录保存图片尺寸、Feature/warning 数量、快照 SHA-256 和 Renderer 版本。
- 产物接口需要管理员会话，使用私有不可变缓存，不允许反向代理直接暴露地图目录。

## 部署验证

1. 使用服务账号执行 `dst-map-renderer --probe --assets <DST_SERVER_PATH>`，确认协议为 `1`。
2. 在“部署检查”确认 `mapRenderer.available` 和 `mapGeneration` 均为真。
3. 用真实 Session 生成地图，核对 Manifest 中的 Tile/图片尺寸、Feature 数量和 `sourceSha256`。
4. 检查地标方向：世界 X 正向向右，Z 正向向上，图片 Y 轴向下，因此只翻转 Y。
5. 检查未知 MOD prefab 仍可搜索和开关，未知 Tile 只产生 warning 而不让任务失败。

测试环境可在 `DST_ADMIN_ENV=test` 时设置 `DST_ADMIN_TEST_MAP=memory`。生产环境明确拒绝该适配器。
