# 页面软件更新 / Online software updates

在系统设置 → 软件更新查看管理服务版本；独立 Agent 在机器管理中查看和更新。管理服务的更新包包含前端。下载和完整校验完成后才会替换程序，存档、配置和游戏服务端不属于更新清理范围。

## 下载设置

- 来源：自动、GitHub 官方源、加速镜像（默认 GHFast）。旧 API 的 `source=proxy` 仍表示加速镜像，新客户端使用 `mirror`。
- **GitHub 加速前缀**可以在下载设置中修改，默认 `https://ghfast.top/`。例如填写 `https://mirror.example/gh/`，请求会变为 `https://mirror.example/gh/https://github.com/lcy0828/dst-admin-go/releases/...`。需要支持完整 GitHub URL 前缀的 HTTPS 服务；不接受账号、查询参数或 URL 片段，留空恢复默认。它用于版本索引、校验文件和更新包；网络代理仍独立配置。
- 网络代理与来源分别设置。管理服务默认继承工坊代理；Agent 默认使用所在机器的系统环境代理，可设置自己的 HTTP/HTTPS/SOCKS 代理。Agent 的 `127.0.0.1` 指 Agent 所在机器。
- 自动模式依次尝试已配置代理、GitHub 直连、国内镜像；固定来源不会自动换源。
- 保存后动态生效，新请求使用新设置，正在下载的任务保留原设置。API 不传 `source` 时使用保存值，传值只覆盖本次请求。
- 下载时限默认 60 分钟，可设置 10～240 分钟。连接、首响应和连续无数据分别有超时，失败后有界重试。页面显示速度、下载量、实际来源和是否续传。

中断或服务重启后可点击“继续下载”接着下载原版本。服务器不支持 Range 时会从头下载；字节范围、长度或校验不匹配的文件不会用于更新。安装前验证整个包的 SHA-256、manifest 和程序。

通过 Docker 镜像、安装包等方式升级后，健康运行版本如果已达到旧任务的目标，旧的待安装、失败或中断任务会标为“目标版本已被当前版本覆盖”（API：`superseded`）。这表示无需再次安装，不表示旧任务安装成功。页面继续提供更新版本的入口。仍高于当前版本的有效待安装包会保留；包缺失或校验失败时可重新下载，无需删除整个更新目录。

Agent 默认自行下载。选择控制端传包后，由控制端先下载并校验官方包，再通过任务凭据提供可续传文件；此时外网下载使用控制端网络设置。旧 Agent 保留原更新入口，升级后才能保存新的节点设置及使用新续传能力。

更新缓存位于软件更新目录的 `downloads` 子目录，中断文件可保留 72 小时；后续下载按 1 GiB 容量清理旧缓存。此清理不会访问游戏或存档目录。系统依赖和基础镜像变更仍需更新镜像。

## 后台检查与提醒

通知页中的后台检查默认开启，每 30 分钟一次；已保存的开关和间隔继续生效。检查开关决定是否定期查询，通知订阅只决定发送给谁。关闭浏览器或没有通知渠道不会停止已启用的检查。管理服务和已接入 Agent 复用版本查询缓存；检查失败保留上次成功结果并显示警告，不把失败当成已是最新版。

[邮箱、Webhook 与 MCP 游标通知](notifications.md)使用同一套版本事件。发现新版不会自动安装。

## English

Use **Settings → Software updates** for the Controller and **Machines** for standalone Agents. Controller packages include the web UI. Game files, settings and saves are preserved.

Save the download source separately from the proxy. Sources are automatic, GitHub and the China mirror; the legacy API value `proxy` remains an alias for `mirror`. The Controller inherits Workshop proxy settings by default. Agents use their own system proxy, with optional per-node overrides. `127.0.0.1` always refers to the machine performing the download.

The **GitHub mirror prefix** defaults to `https://ghfast.top/`. Set a compatible HTTPS prefix, such as `https://mirror.example/gh/`, to prepend it to official GitHub release URLs for metadata, checksums and archives. Credentials, query strings and fragments are rejected; blank restores the default. Each downloading machine has its own setting. Controller relay uses the Controller’s settings.

Automatic mode tries the configured proxy, direct GitHub and the mirror. A fixed source does not switch. Saved settings affect new operations; omitting `source` in the API uses the saved preference. Download deadlines default to 60 minutes and can be set to 10–240 minutes.

Interrupted downloads can resume, including after service restart. Servers that ignore Range cause a clean restart. Full SHA-256, manifest and executable checks are required before installation. Optional Controller relay caches and verifies the archive first, then serves authenticated ranges. Older Agents retain the previous update workflow until upgraded.

After an external image/package upgrade, old prepared, failed or interrupted targets already covered by the healthy running version become `superseded`. This is not a claim that the old online operation succeeded. Newer releases remain accessible, and missing or invalid prepared bundles can be downloaded again. Valid future prepared bundles are retained. No game restart or update-directory deletion is required.

Expired partial files are pruned during later downloads, with a 72-hour retention window and a 1 GiB cache limit. Only update caches are removed. Base-image and system dependency changes still require an image update.

Background checks default to every 30 minutes and remain independent of notification subscriptions. Saved intervals and disabled settings are preserved. Closing the UI does not stop enabled checks; a detected release never installs itself. Failed checks retain the last successful observation with a warning.
