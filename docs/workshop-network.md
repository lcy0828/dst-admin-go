# 创意工坊网络设置

**简体中文（默认）** | [English](workshop-network.en.md)

打开 **系统设置 → 创意工坊**（`/system/settings?tab=workshop`），配置管理服务
获取模组名称、搜索结果、详情和图片的网络。模组搜索失败时也可点击“网络设置”进入。
保存后立即生效，无需停止房间、重启管理服务或断开 Agent。

## 选择访问方式

| 方式 | 行为 |
| --- | --- |
| 跟随系统代理（默认） | 使用管理服务进程的 `HTTP_PROXY`、`HTTPS_PROXY` 和 `NO_PROXY` |
| 直连 | 忽略系统代理，直接访问配置的地址 |
| 指定代理 | 仅此功能使用填写的 HTTP、HTTPS 或 `socks5h` 代理 |

指定代理示例：`http://proxy.example.com:7890`、
`socks5h://proxy.example.com:1080`。`socks5h` 由代理解析目标域名。
支持 URL 中的代理用户名和密码；特殊字符需进行 URL 编码。代理地址按密钥处理，
不会返回明文，留空保留已有值。停用时切换为“直连”或“跟随系统代理”。

容器中的 `127.0.0.1` / `localhost` 指向容器自身。代理运行在宿主机或其他机器时，
请填写管理服务容器能够访问的地址，并确认代理允许该连接。

## 可选 API Key 和反向代理

Steam Web API Key 可在 [Steam 页面](https://steamcommunity.com/dev/apikey) 申请。
已保存的 Key 不再显示明文，留空保留，点击“清除”后保存可移除。

| 条件 | 名称搜索 | 模组 ID / 工坊链接查询 |
| --- | --- | --- |
| 有 Key，Steam API 可达 | 使用 API | 使用 API |
| 无 Key，Steam 社区可达 | 使用公开工坊网页 | 使用免 Key 的 API |
| 无 Key，社区不可达但 API 可达 | 暂不可用 | 仍可查询 |

API Key 不能解决所有网络问题。Steam API、社区网页和图片 CDN 是不同入口，
API 可用不代表图片或社区补充资料可用。

“高级设置”支持分别设置 **Steam API 地址**和 **Steam 社区地址**，可填写管理员
维护的反向代理地址，并支持路径前缀，例如 `https://proxy.example.com/steam-api`。
只填写服务基础地址，不包含 API 方法、Key、查询参数或用户名密码。
默认使用官方 HTTPS，保留证书校验；显式填写 HTTP API 地址时，Key 可能明文传输。
Key 只发送到所配置的 API 地址。

## 检测与搜索

“检测连接”测试当前表单，**不保存配置**，分别显示“名称搜索”“ID 查询”“图片”
的结果和耗时。如果名称搜索失败，可在高级设置填写一个已知模组 ID，单独检测后两项。
没有可用样本时显示“未检测”；检测成功后仍需点击“保存”。

搜索框支持名称、纯数字 ID 和标准 Steam 工坊详情链接。链接只用于提取 ID，
实际查询仍使用已配置的入口。搜索失败时保留上次结果；有同条件缓存时标明资料时间。
可选择“尝试公开工坊”，从第一页重新搜索。翻页保持同一来源，网络设置改变后需重新搜索。

作者和社区描述按需补充，不阻塞主要搜索结果。图片通过面板同源接口加载，
上游不可达时使用占位图。已知模组资料不会因一次网络失败被清空；历史资料不能作为
“模组已是最新版本”的证据。上游限流时需等待后重试。

## 配置文件与环境变量

也可在管理服务的 `conf/app.conf` 的 `[mod]` 分组中配置：

| 配置项 | 环境变量 | 默认值 |
| --- | --- | --- |
| `STEAM_WEB_API_KEY` | `DST_ADMIN_STEAM_API_KEY` | 空 |
| `STEAM_API_BASE` | `DST_ADMIN_STEAM_API_BASE` | `https://api.steampowered.com` |
| `STEAM_COMMUNITY_BASE` | `DST_ADMIN_STEAM_COMMUNITY_BASE` | `https://steamcommunity.com` |
| `STEAM_PROXY_MODE` | `DST_ADMIN_STEAM_PROXY_MODE` | `system`，另可选 `direct` / `custom` |
| `STEAM_HTTP_PROXY` | `DST_ADMIN_STEAM_HTTP_PROXY` | 空；`custom` 模式必填 |

环境变量优先，覆盖的字段在页面只读。页面保存可即时应用；手动编辑配置文件或
修改进程环境变量后需重启管理服务以加载。以上设置均属于管理服务，不需在 Agent 重复配置。

## 模组资料与文件下载

本页只配置公开工坊资料访问。游戏文件、模组安装和更新继续由真实运行节点上的
SteamCMD 执行，**不使用此专用代理设置**。资料能搜索到不代表 SteamCMD 一定能下载；
下载问题请在对应节点检查网络、磁盘、登录和实际任务结果，参见
[游戏服务端管理](game-installation-management.md)。

搜索、资料和缩略图按需请求，空闲时不新增探测。共享并发有上限；图片使用受限的
磁盘缓存，缓存目录不可写时仍可转发。缓存只涉及专用资料目录，不修改游戏存档。

配置、检测、资料补充和缩略图接口已纳入 [OpenAPI](openapi-v2.yaml)，供页面和 AI 共用。
