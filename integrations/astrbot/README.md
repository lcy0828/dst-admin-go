# AstrBot 集成

官方插件为 **DST Admin 饥荒助手**（技术 ID：`astrbot_plugin_dst_companion`），查询、AI 和事件播报已合并。安装后在目标群发送 `/dst 启用本群`，自动发现房间，无需填写 roomId 或群号。接入方式、权限和升级说明见 [AstrBot 指南](../../docs/astrbot-companion.md)。

构建一个可安装 ZIP：

```bash
python integrations/astrbot/package.py --output /tmp/dst-astrbot
```

默认仅生成 `astrbot_plugin_dst_companion.zip`，正式发布也仅提供这个统一插件。`astrbot_plugin_dst_broadcast` 源码作为旧版本迁移兼容入口保留；仅维护旧部署时才使用 `--plugin broadcast` 显式构建，不要新装两个播报者。独立旧包由构建器嵌入同一份播报引擎。

无需 AstrBot 框架的核心测试：

```bash
python -m unittest discover -s integrations/astrbot/tests -v
```

安装 AstrBot 4.29.3 的 Python 环境可同时运行真实框架的唤醒、分发、模型路由及权限适配测试。测试会用临时数据目录，建议在独立工作目录运行；QQ 消息、模型请求和游戏写入由替身接管。

常见问法、反例、多轮选择与模型回复格式的覆盖边界见[测试说明](tests/README.md)。框架测试被跳过时，只能说明核心测试通过，不能作为完整消息管线验收。
