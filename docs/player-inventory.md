# 玩家背包 / Player inventory

在首页或玩家管理中打开玩家操作菜单，选择「查看背包」。导入存档后还没有玩家记录时，可在玩家管理中选择「按玩家 ID 查询」，输入已知的 `KU_` ID。

- **自动选择**：确认玩家在线时读取当前分片；确认离线或世界停止时读取当前 Session 的玩家存档。位置不确定时会提示重试或选择来源。
- **在线实时**：读取装备、物品栏、鼠标手持、背包、包裹与嵌套标准容器。玩家正在迁移、加载或选择角色时，可能暂时无法读取。
- **存档快照**：读取最近落盘的玩家快照，无需启动世界。多个世界或目录均有记录时，请选择来源；文件时间不代表玩家下线时间。

查询只读，不会触发存盘、生成物品或修改存档。关闭弹窗后没有新增后台轮询。本机与 Agent 使用相同的数据格式；远程节点需更新到支持该功能的版本，在线读取还需要配套的游戏 Runtime。查询本身不会安装或更新 Runtime。

离线读取支持原生版本 3 玩家快照，包括末尾带有小地图等二进制附加数据的存档。仅解析长度声明内的玩家 Lua 数据，附加数据不会作为代码执行；不支持的格式、截断或超限会明确报错，不会当成空背包。

搜索会保留容器路径；容器可展开查看。物品图标复用已有资源，缺图不影响读取。模组使用标准物品、容器或包裹组件时可读取；地面箱子、跟随者、共享仓库及模组私有存储不在支持范围内。「完整」仅表示支持的组件已读取，不保证涵盖模组自定义数据。

读取达到大小、深度、节点数或采样时间边界时，会明确标为部分结果，缺失内容不能视为空。存档常不保存耐久等属性的最大值；页面只显示可确定的剩余值，不推测百分比。

API 与 AI/MCP 使用同一接口：

```http
GET /api/v2/rooms/{roomId}/players/{playerId}/inventory?source=auto
```

`source` 可选 `auto`、`live`、`save`；可用 `worldId` 和返回的 `sourceId` 选择来源。`refresh=true` 重新确认在线玩家的位置并跳过存档解析缓存，仍受并发与刷新频率限制。在线定位基于分片本地玩家实体，位置数据过期时在本次请求内按需刷新，无需先打开或刷新玩家列表。指定 `worldId` 会严格限制该世界，不会自动跨世界；位置不确定、采集失败或节点离线不会自动改读历史存档。完整参数和错误见 [OpenAPI](openapi-v2.yaml) 的 `getRoomPlayerInventory`。

## English

Open a player's action menu on the dashboard or player management page and choose **View inventory**. For imported saves without player history, use **Look up player ID** with a known `KU_` ID.

**Automatic** reads live data only after confirming the player's location; offline players and stopped worlds use the current Session save. **Live inventory** includes equipment, inventory, held items, backpacks, bundles and nested standard containers. **Saved snapshot** works without starting the game. Choose a source when multiple worlds or directories have records; file modification time is not logout time.

Queries do not save the game, create items or modify saves. There is no additional idle polling. Local execution and Agents share the same format. Remote nodes need a compatible Agent, and live queries require the matching game Runtime. A query never installs or updates Runtime.

Offline reads support native version 3 player snapshots, including binary classified/minimap data after the inventory payload. Only the declared Lua data string is parsed; the suffix is never executed as code. Unsupported, truncated or oversized data produces an explicit error rather than an empty inventory.

Search preserves container ancestry. Existing artwork is reused; missing images do not block results. Mods using standard components are supported. Ground chests, followers, shared storage and private Mod storage are excluded. A complete result covers supported components only. Limits produce an explicit partial result. Missing saved attribute maximums remain unknown rather than becoming zero or full percentages.

The page, AI and MCP share the authenticated `getRoomPlayerInventory` OpenAPI operation shown above. Use `source=auto|live|save`, optionally `worldId` and a returned `sourceId`. `refresh=true` reconfirms live player location and bypasses the save parsing cache while preserving concurrency and rate limits. Location uses shard-local entities; stale evidence is refreshed on demand without requiring a player-list refresh. An explicit `worldId` strictly restricts the lookup. Unknown locations, collection failures and offline nodes never silently fall back to saved items.
