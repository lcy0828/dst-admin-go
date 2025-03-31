# DST Admin Go

这是一个使用Go语言开发的《饥荒联机版》(Don't Starve Together)服务器管理工具。

## 新增功能：Agent-Server通信系统

现在DST Admin Go增加了一个基于WebSocket的Agent-Server通信系统，支持远程管理和监控功能。

### 特点

- 基于WebSocket的实时通信
- 端到端加密（使用NaCl Box密码学）
- 通信安全密钥认证（支持多团队使用不同密钥）
- 断线自动重连
- 主动/被动数据上报
- 远程命令执行（Shell命令和脚本）
- 跨平台支持

### 使用方法

#### 启动带Agent Server的DST Admin

```bash
# 启用Agent Server功能
./dont-admin --agent-server --agent-listen :8081

# 指定通信密钥文件
./dont-admin --agent-server --agent-listen :8081 --key-file /path/to/keys.json

# 指定TLS证书（推荐用于生产环境）
./dont-admin --agent-server --agent-listen :8081 --cert /path/to/cert.pem --key /path/to/key.pem
```

#### 启动Agent客户端

```bash
# 连接到Agent Server
./agent --server ws://your-server-address:8081/agent

# 使用通信密钥连接
./agent --server ws://your-server-address:8081/agent --key YourSecurityKey

# 指定Agent ID
./agent --id myserver1

# 设置主动上报间隔
./agent --report 1m
```

### 通过API管理Agent

Agent Server功能集成了以下API接口：

- `GET /api/agent/list` - 获取所有已连接的Agent
- `POST /api/agent/command` - 向Agent发送命令
- `POST /api/agent/report` - 请求Agent上报信息

所有API接口都需要JWT认证。

### 通信安全密钥管理

安全密钥用于验证Agent与Server之间的通信，确保只有授权的Agent可以连接到Server。

系统提供了以下API接口管理通信安全密钥：

- `GET /api/agent/security/key` - 获取当前的通信安全密钥
- `POST /api/agent/security/key/generate` - 生成新的随机通信安全密钥
- `POST /api/agent/security/key/update` - 更新通信安全密钥

#### 获取当前密钥示例

```
GET /api/agent/security/key
Authorization: Bearer <your-jwt-token>
```

#### 生成新的随机密钥示例

```
POST /api/agent/security/key/generate
Authorization: Bearer <your-jwt-token>
```

#### 更新通信密钥示例

```
POST /api/agent/security/key/update
Authorization: Bearer <your-jwt-token>
Content-Type: application/json

{
  "key": "YourNewSecurityKey"
}
```

### Agent Server API示例

#### 获取所有Agent
```
GET /api/agent/list
Authorization: Bearer <your-jwt-token>
```

#### 发送命令到Agent
```
POST /api/agent/command
Authorization: Bearer <your-jwt-token>
Content-Type: application/json

{
  "agent_id": "server1",
  "type": "shell",
  "content": "ls -la",
  "timeout": 30
}
```

#### 请求数据上报
```
POST /api/agent/report
Authorization: Bearer <your-jwt-token>
Content-Type: application/json

{
  "agent_id": "server1",
  "report_type": "system_info",
  "params": {}
}
```

## 原项目说明

# Go Agent-Server 通信系统

这是一个基于Golang的Agent-Server通信系统，支持安全的远程管理和监控功能。

## 特点

- 基于WebSocket的实时通信
- 端到端加密（使用NaCl Box密码学）
- 断线自动重连
- 主动/被动数据上报
- 远程命令执行（Shell命令和脚本）
- 跨平台支持

## 项目结构

```
.
├── agent/          # Agent端代码
│   └── cmd/agent/  # Agent主程序
├── server/         # Server端代码
│   └── cmd/server/ # Server主程序
└── shared/         # 共享代码（加密、协议等）
```

## 编译

### 使用脚本编译

#### Windows

```bash
build.bat
```

#### Linux/macOS

```bash
chmod +x build.sh
./build.sh
```

### 手动编译

#### 编译Agent

```bash
# Windows
go build -o agent.exe ./agent/cmd/agent

# Linux/macOS
go build -o agent ./agent/cmd/agent
```

#### 编译Server

```bash
# Windows
go build -o server.exe ./server/cmd/server

# Linux/macOS
go build -o server ./server/cmd/server
```

## 使用方法

### 启动Server

```bash
# 基本启动
./server

# 指定监听地址
./server -listen :8443

# 使用TLS
./server -listen :8443 -cert /path/to/cert.pem -key /path/to/key.pem
```

### 启动Agent

```bash
# 基本启动（连接本地服务器）
./agent

# 指定服务器地址
./agent -server ws://server-address:8080/agent

# 指定Agent ID
./agent -id myagent1

# 设置主动上报间隔
./agent -report 1m
```

## Server控制台命令

Server提供了一个简单的命令行界面，可以使用以下命令：

- `list` - 列出所有已连接的Agent
- `info <agent_id>` - 显示指定Agent的详细信息
- `shell <agent_id> <command>` - 在指定Agent上执行shell命令
- `script <agent_id> <script>` - 在指定Agent上执行脚本
- `report <agent_id> <type>` - 请求Agent进行被动上报
- `exit` - 退出服务器

## 安全说明

通信使用NaCl Box (curve25519, XSalsa20和Poly1305)进行端到端加密保护。每个Agent和Server都有自己的公钥/私钥对，确保通信过程的安全性。

## 许可证

MIT 