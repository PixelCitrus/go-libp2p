# P2P Chat with Relay Support

这是一个基于 libp2p 实现的 P2P 聊天应用程序，支持以下特性：

- 同时支持 IPv4 和 IPv6 网络
- 支持 NAT 穿透
- 支持通过中继节点进行通信（当两个节点无法直接连接时）
- 支持本地节点自动发现（使用 mDNS）

## 构建

在 `go-libp2p/examples` 目录下运行：

```bash
cd chat-with-relay
go build -o chat
```

```
# 终端1（中继节点）
./chat -mode relay -port 6666

# 终端2（节点A）
./chat -port 6667 -relay /ip4/127.0.0.1/tcp/6666/p2p/QmRelayID

# 终端3（节点B）
./chat -port 6668 -relay /ip4/127.0.0.1/tcp/6666/p2p/QmRelayID
```

## 使用方法

### 1. 启动中继节点

```bash
./chat -mode relay -port 6666
```

### 2. 启动普通节点

```bash
# 节点 A
./chat -port 6667 -relay /ip4/127.0.0.1/tcp/6666/p2p/RELAY_PEER_ID

# 节点 B
./chat -port 6668 -relay /ip4/127.0.0.1/tcp/6666/p2p/RELAY_PEER_ID
```

### 参数说明

- `-mode`: 节点模式，可选值为 `peer`（默认）或 `relay`
- `-port`: 监听端口
- `-relay`: 中继节点的地址（当 mode 为 peer 时使用）
- `-rendezvous`: 用于节点发现的标识字符串，默认为 "chat-with-relay"

## 工作原理

1. 程序启动时，根据 mode 参数决定是作为中继节点还是普通节点运行
2. 中继节点会启用 Circuit Relay 功能，并等待其他节点连接
3. 普通节点会：
   - 尝试通过 mDNS 发现本地网络中的其他节点
   - 连接到指定的中继节点
   - 当需要与其他节点通信时：
     a. 首先尝试直接连接（包括 NAT 穿透）
     b. 如果直接连接失败，则通过中继节点建立连接
4. 所有节点之间使用加密通道进行通信，确保消息安全

## 注意事项

1. 在公网使用时，请确保中继节点有公网 IP 地址
2. 使用 IPv6 时，请确保系统和网络都已启用 IPv6 支持
3. 如果要跨网络使用，建议将中继节点部署在具有公网访问能力的服务器上