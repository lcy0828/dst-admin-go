# DST Admin 伴随助手

常用游戏查询不调用模型，支持昵称消歧、引用后续查询、固定管理动作及真实任务回执。

配置和权限说明：[接入指南](https://github.com/lcy0828/dst-admin-go/blob/master/docs/astrbot-companion.md)。默认关闭；先配置允许的会话、房间和独立的读取/管理 MCP 连接，再开启。适用 AstrBot 4.29.3 / Python 3.12。


v0.2 使用面板 `/mcp?view=bot`（14 个依赖入口，按密钥权限过滤）。新安装推荐配置模式 2 和 shortcut；旧配置模式 1 保留 mixed 行为。支持“菜单 / 有哪些服 / 下一页 / 取消”，能力目录按需更新。写操作仍需原生管理员、独立管理连接和明确 writeAllowlist；中断后只读原请求。

模板、会话覆盖和 API 示例见接入指南。源码包不包含凭证、部署数据或缓存；真实 QQ 收发仍需在自己的测试会话验收。
