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