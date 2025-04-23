# 动态日志处理系统使用指南

## 系统概述

动态日志处理系统能够根据饥荒服务器的运行状态自动监控和解析日志文件。系统会检测服务器的启动和停止，并相应地开始或结束日志监控。它支持监控多个房间和多个世界（森林和洞穴）的日志文件，并根据房间名称和世界名称进行聚合汇总。

## 主要特性

- **动态监控**：根据服务器状态自动开始和停止日志监控
- **多世界支持**：同时监控多个房间和多个世界（森林和洞穴）的日志
- **实时推送**：通过WebSocket实时推送日志内容
- **智能解析**：根据配置的规则自动解析和分类日志
- **数据存储**：将解析后的日志存储到数据库中，支持查询和统计
- **自定义规则**：支持添加、修改和删除日志解析规则

## 启动参数

启动服务时可以使用以下命令行参数来配置日志处理系统：

```
--dynamic-log=true|false     # 启用或禁用动态日志监控，默认为true
--log-check-interval=5s      # 检查服务器状态的间隔，默认为5秒
--log-retention=30           # 日志保留天数，默认为30天
```

示例：
```
./dst-admin-go --dynamic-log=true --log-check-interval=10s --log-retention=60
```

## API接口

### 1. 日志查询接口

#### 获取解析后的日志
```
GET /api/v1/parser/logs
```
参数：
- `archive`: 存档名称（可选）
- `world`: 世界名称（可选）
- `type`: 日志类型（可选）
- `page`: 页码，默认为1
- `page_size`: 每页条数，默认为20

响应：
```json
{
  "status": 200,
  "msg": "获取日志成功",
  "data": [
    {
      "id": 1,
      "archive_name": "02_forest",
      "world_name": "Forest1",
      "log_type": "system",
      "content": "[forest] [00:00:00]: Starting Up",
      "raw_content": "Starting Up",
      "timestamp": "2023-07-01T12:34:56Z"
    }
  ],
  "meta": {
    "total": 100,
    "page": 1,
    "page_size": 20
  }
}
```

#### 获取日志类型统计
```
GET /api/v1/parser/log_types
```
参数：
- `archive`: 存档名称（可选）
- `world`: 世界名称（可选）

响应：
```json
{
  "status": 200,
  "msg": "获取日志类型统计成功",
  "data": {
    "system": 1200,
    "player": 450,
    "entity": 320,
    "error": 15
  }
}
```

#### 获取日志统计信息
```
GET /api/v1/parser/log_statistics
```
参数：
- `archive`: 存档名称（可选）
- `world`: 世界名称（可选）
- `days`: 统计天数，默认为7

响应：
```json
{
  "status": 200,
  "msg": "获取日志统计成功",
  "data": {
    "2023-07-01": {
      "system": 120,
      "player": 45,
      "entity": 32,
      "error": 2
    },
    "2023-07-02": {
      "system": 115,
      "player": 50,
      "entity": 30,
      "error": 1
    }
  }
}
```

#### 搜索日志
```
GET /api/v1/parser/search
```
参数：
- `keyword`: 搜索关键词（必填）
- `page`: 页码，默认为1
- `page_size`: 每页条数，默认为20

响应：
```json
{
  "status": 200,
  "msg": "搜索日志成功",
  "data": [
    {
      "id": 1,
      "archive_name": "02_forest",
      "world_name": "Forest1",
      "log_type": "player",
      "content": "[forest] [00:10:23]: Player 'username' joined the game",
      "raw_content": "Player 'username' joined the game",
      "timestamp": "2023-07-01T12:34:56Z"
    }
  ],
  "meta": {
    "total": 5,
    "page": 1,
    "page_size": 20
  }
}
```

### 2. 日志提取规则管理

#### 获取规则列表
```
GET /api/v1/parser/rules
```

响应：
```json
{
  "status": 200,
  "msg": "获取规则列表成功",
  "data": [
    {
      "id": 1,
      "name": "服务器启动",
      "description": "匹配服务器启动信息",
      "log_type": "system",
      "pattern": "\\[\\d{2}:\\d{2}:\\d{2}\\]: Starting Up",
      "is_regex": true,
      "is_enabled": true,
      "priority": 100
    }
  ]
}
```

#### 添加规则
```
POST /api/v1/parser/rules
```
请求体：
```json
{
  "name": "玩家加入",
  "description": "匹配玩家加入游戏信息",
  "log_type": "player",
  "pattern": "\\[\\d{2}:\\d{2}:\\d{2}\\]: Player .* joined the game",
  "is_regex": true,
  "is_enabled": true,
  "priority": 90
}
```

响应：
```json
{
  "status": 200,
  "msg": "添加规则成功",
  "data": {
    "id": 2
  }
}
```

#### 更新规则
```
PUT /api/v1/parser/rules/:id
```
请求体：
```json
{
  "name": "玩家加入",
  "description": "匹配玩家加入游戏信息（更新）",
  "log_type": "player",
  "pattern": "\\[\\d{2}:\\d{2}:\\d{2}\\]: Player .* joined the game",
  "is_regex": true,
  "is_enabled": true,
  "priority": 95
}
```

响应：
```json
{
  "status": 200,
  "msg": "更新规则成功"
}
```

#### 删除规则
```
DELETE /api/v1/parser/rules/:id
```

响应：
```json
{
  "status": 200,
  "msg": "删除规则成功"
}
```

### 3. 日志解析器状态管理

#### 获取当前运行中的解析器
```
GET /api/v1/parser/active
```

该接口返回当前所有正在运行的日志解析器的详细信息，包括存档名称、世界名称、服务器类型、启动时间、日志文件路径、已处理行数等信息。这些信息对于监控和调试日志解析系统非常有用。

响应：
```json
{
  "status": 200,
  "msg": "获取运行中的解析器成功",
  "data": [
    {
      "id": "dstserver_02_Forest1",
      "archive_name": "02",
      "world_name": "Forest1",
      "server_type": "forest",
      "start_time": "2023-07-01T12:34:56Z",
      "log_file": "/path/to/Klei/DoNotStarveTogether/02/Forest1/server_log.txt",
      "status": "running",
      "processed_lines": 12345,
      "last_activity": "2023-07-01T13:45:23Z",
      "client_count": 2
    },
    {
      "id": "dstserver_02_Cave1",
      "archive_name": "02",
      "world_name": "Cave1",
      "server_type": "cave",
      "start_time": "2023-07-01T12:35:12Z",
      "log_file": "/path/to/Klei/DoNotStarveTogether/02/Cave1/server_log.txt",
      "status": "running",
      "processed_lines": 8765,
      "last_activity": "2023-07-01T13:44:56Z",
      "client_count": 1
    }
  ]
}
```

字段说明：
- `id`: 解析器唯一标识符，通常是会话名称
- `archive_name`: 存档名称
- `world_name`: 世界名称
- `server_type`: 服务器类型，可能的值有 "forest"(森林世界) 或 "cave"(洞穴世界)
- `start_time`: 解析器启动时间
- `log_file`: 日志文件路径
- `status`: 当前状态，通常为 "running"
- `processed_lines`: 已处理的日志行数
- `last_activity`: 最后活动时间
- `client_count`: 当前连接到该解析器的WebSocket客户端数量

#### 切换日志解析器状态
```
POST /api/v1/parser/toggle
```
请求体：
```json
{
  "enabled": true
}
```

响应：
```json
{
  "status": 200,
  "msg": "日志解析器已启用"
}
```

### 4. WebSocket实时日志接口

#### 实时获取日志
```
WebSocket /api/v1/game/log/ws
```
查询参数：
- `archive`: 存档名称（必填）
- `world`: 世界名称（必填）
- `use_rules`: 是否使用规则过滤，值为"true"或"false"（可选，默认为"false"）

客户端连接后，将实时接收日志消息：
```json
{
  "type": "log",
  "content": "[00:12:34]: Player 'username' joined the game",
  "highlight": false,
  "color": "",
  "timestamp": 1625145274
}
```

客户端可以发送以下命令来控制规则：
```json
{
  "action": "toggle_rules",
  "enabled": true
}
```

## 使用示例

### 1. 启动服务并启用动态日志监控

```bash
./dst-admin-go --dynamic-log=true --log-check-interval=5s --log-retention=30
```

### 2. 通过API获取日志

```bash
# 获取最近的日志
curl "http://localhost:8080/api/v1/parser/logs?archive=02&world=Forest1&page=1&page_size=20"

# 搜索包含特定关键词的日志
curl "http://localhost:8080/api/v1/parser/search?keyword=joined&page=1&page_size=20"

# 获取日志类型统计
curl "http://localhost:8080/api/v1/parser/log_types?archive=02&world=Forest1"
```

### 3. 通过WebSocket实时获取日志

使用WebSocket客户端连接：
```
ws://localhost:8080/api/v1/game/log/ws?archive=02&world=Forest1&use_rules=true
```

## 工作原理

1. 系统启动时，动态日志监控服务会定期（默认5秒）检查服务器状态
2. 当检测到服务器启动时，会自动创建对应的日志监控器并开始监控日志文件
3. 日志监控器会实时读取日志文件的变化，并通过WebSocket推送给客户端
4. 同时，日志解析器会根据配置的规则解析日志内容，并存储到数据库中
5. 当检测到服务器停止时，会等待5秒（确保解析完所有日志）后停止监控
6. 系统会自动处理多个房间和多个世界（森林和洞穴）的日志文件，并根据房间名称和世界名称进行聚合

## 日志类型

系统默认支持以下日志类型：

- `system`: 系统相关日志，如服务器启动、关闭等
- `player`: 玩家相关日志，如玩家加入、离开、聊天等
- `entity`: 实体相关日志，如生物生成、死亡等
- `error`: 错误日志，如服务器错误、崩溃等
- `unknown`: 未分类日志

## 注意事项

1. 日志文件路径会根据服务器配置自动确定，无需手动设置
2. 系统会自动识别森林世界和洞穴世界，并在日志中添加对应标记
3. 默认保留30天的日志数据，可通过`--log-retention`参数调整
4. 如果需要自定义日志解析规则，可以通过API接口添加、修改或删除规则
5. 当服务器重启时，系统会自动检测并重置日志解析器状态，确保日志解析的连续性

## 故障排除

### 问题：日志监控服务无法启动

可能原因：
- 配置文件中的路径设置不正确
- 没有足够的权限访问日志文件

解决方法：
- 检查配置文件中的`DST_SAVE_PATH`设置
- 确保程序有权限访问日志文件所在目录

### 问题：没有收到实时日志

可能原因：
- WebSocket连接断开
- 服务器未运行
- 日志文件路径不正确

解决方法：
- 检查WebSocket连接状态
- 确认服务器是否正在运行
- 验证日志文件路径是否正确

### 问题：日志解析不正确

可能原因：
- 解析规则不匹配
- 日志格式发生变化

解决方法：
- 检查并更新解析规则
- 添加新的规则以适应变化的日志格式

## 结语

动态日志处理系统为饥荒服务器管理提供了强大的日志监控和分析功能。通过自动化的日志收集、解析和存储，管理员可以轻松地监控服务器状态、玩家活动和潜在问题，无需手动处理日志文件。系统的动态特性确保了它能够适应不同的服务器配置和运行状态，为多个房间和多个世界提供统一的日志管理体验。
