# 旧版兼容：DST Admin 事件播报

v0.1.2 保留 v0.1.1 的插件名称、配置、KV 游标及投递状态，增加与伴随助手的玩家对象关联、共享并发额度和运行中世界发现。只保留一个启用的播报实例，升级时不要清空状态。

配置说明：[接入指南](https://github.com/lcy0828/dst-admin-go/blob/master/docs/astrbot-companion.md)。本插件仍可独立运行；伴随助手缺失时沿用原来的单一发送路径。

新部署请只安装 `astrbot_plugin_dst_companion.zip`（v0.3 起显示为 DST Admin 饥荒助手），已合并播报并支持群内自动接入。旧目录只保留迁移与显式旧包构建，统一插件确认保存状态后关闭旧播报器；无需同时启用两个插件。
