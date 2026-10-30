# AstrBot × DST Admin

官方伴随插件放在本目录，和面板一同维护；不需要游戏与机器人部署在同一台机器，也不需要 SSH、WSL、向量数据库或独立代理进程。支持 AstrBot 4.29.3 / Python 3.12。面板需包含 `resolveRoomPlayer` 和接入密钥用途/房间范围功能。

- `astrbot_plugin_dst_companion`：v0.2：无模型快捷指令、分页菜单与参数会话、动态能力目录、可自定义会话配置、按原请求恢复的管理动作。
- `astrbot_plugin_dst_broadcast`：可选的事件播报 v0.1.2，保留 v0.1.1 的游标、KV、进退确认和单一投递者，增加玩家对象关联、共享并发预算和运行中世界发现。

部署和权限说明见 [AstrBot 接入指南](../../docs/astrbot-companion.md)。源码安装后默认关闭，开发测试不会打开模型、发送群消息或重启游戏。

构建可导入 AstrBot 的压缩包：

```bash
python3 integrations/astrbot/package.py --output /tmp/dst-astrbot-packages
```

每个 ZIP 的根目录包含自己的 `main.py`、`metadata.yaml` 和配置 Schema，不包含部署配置、凭证、数据库或缓存。两个插件可以分别升级；不要同时启用旧版和改名后的重复播报实例。现有 hxyinan 管理插件仅作为交互参考，不需要同时启用其查询拦截器。

验证：

```bash
python3 -m unittest discover -s integrations/astrbot/tests -v
ruff check integrations/astrbot
```

框架适配测试需要在安装了 AstrBot 的 Python 环境运行；普通环境会明确跳过。测试的模型、面板写操作、QQ 发送全部由替身接管；真实群收信与游戏 RTT 采集不属于这些测试的证明范围。
