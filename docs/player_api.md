# 玩家信息 API 文档

本文档描述了玩家信息相关的 API 接口。

## 基础信息

- 基础路径: `/api/player`
- 所有接口返回 JSON 格式数据
- 所有接口的响应格式如下:

```json
{
  "code": 200,       // 状态码，200 表示成功，其他值表示失败
  "msg": "成功",      // 状态消息
  "data": { ... }    // 响应数据，具体格式根据接口不同而不同
}
```

## 接口列表

### 1. 获取在线玩家列表

获取当前在线的玩家列表。

**请求方式**: `GET`

**路径**: `/api/player/online`

**参数**:

| 参数名 | 类型 | 必需 | 描述 |
|-------|------|------|------|
| archive_name | string | 否 | 存档名称。如果提供，则只获取指定存档的在线玩家；如果不提供，则获取所有存档的在线玩家。 |

**响应示例**:

```json
{
  "code": 200,
  "msg": "成功",
  "data": [
    {
      "id": 1,
      "archive_name": "MyCluster",
      "user_id": "KU_F4GEnAsM",
      "player_name": "2068829893",
      "player_age": 52,
      "prefab": "webber",
      "status": "online",
      "first_seen": "2025-04-16T17:28:00Z",
      "last_seen": "2025-04-16T17:28:00Z",
      "status_change": "2025-04-16T17:28:00Z",
      "created_at": "2025-04-16T17:28:00Z",
      "updated_at": "2025-04-16T17:28:00Z"
    }
  ]
}
```

### 2. 获取所有玩家列表

获取所有玩家列表，支持分页。

**请求方式**: `GET`

**路径**: `/api/player/all`

**参数**:

| 参数名 | 类型 | 必需 | 描述 |
|-------|------|------|------|
| archive_name | string | 否 | 存档名称。如果提供，则只获取指定存档的玩家；如果不提供，则获取所有存档的玩家。 |
| page | int | 否 | 页码，默认为 1 |
| page_size | int | 否 | 每页数量，默认为 10，最大为 100 |

**响应示例**:

```json
{
  "code": 200,
  "msg": "成功",
  "data": [
    {
      "id": 1,
      "archive_name": "MyCluster",
      "user_id": "KU_F4GEnAsM",
      "player_name": "2068829893",
      "player_age": 52,
      "prefab": "webber",
      "status": "online",
      "first_seen": "2025-04-16T17:28:00Z",
      "last_seen": "2025-04-16T17:28:00Z",
      "status_change": "2025-04-16T17:28:00Z",
      "created_at": "2025-04-16T17:28:00Z",
      "updated_at": "2025-04-16T17:28:00Z"
    }
  ],
  "total": 1,
  "page": 1,
  "size": 10
}
```

### 3. 获取玩家统计信息

获取玩家统计信息，包括总玩家数、在线玩家数、离线玩家数和最近加入的玩家。

**请求方式**: `GET`

**路径**: `/api/player/stats`

**参数**:

| 参数名 | 类型 | 必需 | 描述 |
|-------|------|------|------|
| archive_name | string | 否 | 存档名称。如果提供，则只获取指定存档的统计信息；如果不提供，则获取所有存档的统计信息。 |

**响应示例**:

```json
{
  "code": 200,
  "msg": "成功",
  "data": {
    "total_count": 10,
    "online_count": 2,
    "offline_count": 8,
    "recent_players": [
      {
        "id": 1,
        "archive_name": "MyCluster",
        "user_id": "KU_F4GEnAsM",
        "player_name": "2068829893",
        "player_age": 52,
        "prefab": "webber",
        "status": "online",
        "first_seen": "2025-04-16T17:28:00Z",
        "last_seen": "2025-04-16T17:28:00Z",
        "status_change": "2025-04-16T17:28:00Z",
        "created_at": "2025-04-16T17:28:00Z",
        "updated_at": "2025-04-16T17:28:00Z"
      }
    ]
  }
}
```

### 4. 获取玩家详情

根据玩家 ID 获取玩家详情。

**请求方式**: `GET`

**路径**: `/api/player/detail/:id`

**参数**:

| 参数名 | 类型 | 必需 | 描述 |
|-------|------|------|------|
| id | int | 是 | 玩家 ID |

**响应示例**:

```json
{
  "code": 200,
  "msg": "成功",
  "data": {
    "id": 1,
    "archive_name": "MyCluster",
    "user_id": "KU_F4GEnAsM",
    "player_name": "2068829893",
    "player_age": 52,
    "prefab": "webber",
    "status": "online",
    "first_seen": "2025-04-16T17:28:00Z",
    "last_seen": "2025-04-16T17:28:00Z",
    "status_change": "2025-04-16T17:28:00Z",
    "created_at": "2025-04-16T17:28:00Z",
    "updated_at": "2025-04-16T17:28:00Z"
  }
}
```

### 5. 获取玩家数据库中的存档列表

获取玩家数据库中存在的所有存档列表。

**请求方式**: `GET`

**路径**: `/api/player/archives`

**参数**: 无

**响应示例**:

```json
{
  "code": 200,
  "msg": "成功",
  "data": [
    "MyCluster",
    "AnotherCluster",
    "TestCluster"
  ]
}
```

### 6. 手动更新玩家信息

手动触发玩家信息更新。

**请求方式**: `POST`

**路径**: `/api/player/update`

**请求体**:

```json
{
  "session_name": "dstserver_MyCluster_Master"
}
```

| 参数名 | 类型 | 必需 | 描述 |
|-------|------|------|------|
| session_name | string | 是 | 会话名称，格式为 `dstserver_存档名_世界名` |

**响应示例**:

```json
{
  "code": 200,
  "msg": "玩家信息更新成功",
  "data": {
    "output": "玩家信息更新成功，存档: MyCluster，在线玩家: 1，总玩家: 2",
    "session_name": "dstserver_MyCluster_Master"
  }
}
```

### 7. 读取玩家配置文件

手动读取和解析玩家配置文件。

**请求方式**: `POST`

**路径**: `/api/player/config`

**请求体**:

```json
{
  "archive_name": "MyCluster",
  "world_name": "Master",
  "filter_mode": 0
}
```

| 参数名 | 类型 | 必需 | 描述 |
|-------|------|------|------|
| archive_name | string | 是 | 存档名称 |
| world_name | string | 是 | 世界名称 |
| filter_mode | int | 否 | 过滤模式，0=全部显示（默认），1=只显示主机，2=只显示玩家 |

**响应示例**:

```json
{
  "code": 200,
  "msg": "读取玩家配置文件成功",
  "data": {
    "output": "成功读取玩家配置文件",
    "archive_name": "MyCluster",
    "world_name": "Master",
    "config_path": "/path/to/Klei/DoNotStarveTogether/MyCluster/Master/save/mod_config_data/players",
    "player_count": 2,
    "detailed_info": [
      {
        "UserID": "KU_F4GEnAsM",
        "Name": "[Host]",
        "Admin": true,
        "EventLevel": 0,
        "Muted": false,
        "Friend": false,
        "PlayerAge": 52,
        "IsHost": true,
        "UserFlags": 0,
        "Performance": 0,
        "Prefab": "webber",
        "LobbyCharacter": "webber",
        "Colour": [0.5, 0.5, 0.5, 1],
        "BaseSkin": "webber_none",
        "NetID": "76561198812646511",
        "NetScore": 0,
        "SkillSelection": [0],
        "Vanity": {},
        "Equip": {}
      },
      {
        "UserID": "KU_HQp7BOVs",
        "Name": "lcy",
        "Admin": true,
        "EventLevel": 0,
        "Muted": false,
        "Friend": false,
        "PlayerAge": 2,
        "IsHost": false,
        "UserFlags": 8,
        "Performance": 0,
        "Prefab": "wormwood",
        "LobbyCharacter": "",
        "Colour": [1, 0.647, 0.31, 1],
        "BaseSkin": "wormwood_none",
        "NetID": "76561198812646511",
        "NetScore": 0,
        "SkillSelection": [0],
        "Vanity": {},
        "Equip": {}
      }
    ]
  }
}
```

## 错误码

| 错误码 | 描述 |
|-------|------|
| 200 | 成功 |
| 400 | 参数错误 |
| 500 | 服务器内部错误 |

## 数据结构

### PlayerInfo

| 字段名 | 类型 | 描述 |
|-------|------|------|
| id | int | 玩家 ID |
| archive_name | string | 存档名称 |
| user_id | string | 玩家 ID (KU_xxx格式) |
| player_name | string | 玩家名称 |
| player_age | int | 玩家年龄 |
| prefab | string | 玩家角色 |
| status | string | 玩家状态：online/offline |
| first_seen | string | 首次出现时间 |
| last_seen | string | 最后出现时间 |
| status_change | string | 状态变更时间 |
| created_at | string | 记录创建时间 |
| updated_at | string | 记录更新时间 |
