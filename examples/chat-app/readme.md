

安装依赖:
go mod tidy
go install


编译:
go build -o chat-app main.go


运行中继节点:

go run main.go -relay -port 6666
（记下中继节点的 Peer ID 和 Multiaddr，例如：/ip4/127.0.0.1/tcp/4001/p2p/Qm...）

运行普通节点 A:

go run main.go -port 0 -relay-addr "/ip4/127.0.0.1/tcp/4001/p2p/<中继节点的Peer ID>"
（-port 0 会让 libp2p 随机选择一个可用端口。记下普通节点 A 的 Peer ID。）

运行普通节点 B:
go run main.go -port 0 -relay-addr "/ip4/127.0.0.1/tcp/4001/p2p/<中继节点的Peer ID>"
（记下普通节点 B 的 Peer ID。）

功能测试:

中继节点启动: 确保中继节点正常启动并显示其 Peer ID 和监听地址。
普通节点连接中继: 普通节点启动后，会尝试连接到指定的中继节点。你应该会看到连接成功的提示。
普通节点发现其他节点: 等待一段时间，普通节点会通过 DHT 发现其他普通节点。你可以在节点 A 上使用 /list_direct 命令查看直接连接的节点。
查看连接到中继的列表: 在普通节点 A 或 B 上使用 /list_relay_peers 命令，它会向中继节点请求并显示当前连接到中继的普通节点列表。


发送消息:
直接发送: 如果节点 A 和 B 之间建立了直接连接（通过 DHT 发现并连接），你可以尝试直接发送：
/send <节点B的Peer ID> Hello from A directly!
通过中继转发: 如果节点 A 和 B 之间没有直接连接（或者你强制关闭了直接连接），消息会尝试通过中继转发：
/send <节点B的Peer ID> Hello from A via relay!
你应该能看到中继节点打印出转发消息的日志，以及接收节点收到消息。


代码解释和关键点:
common 目录: 存放共享的常量、工具函数和消息结构体。
relay/relay_node.go:
NewRelayNode: 创建一个 libp2p.Host 并启用 libp2p.EnableRelay() 和 libp2p.EnableAutoRelay()。circuit.NewRelay() 注册了中继服务。
handleRelayStream: 这是中继节点处理传入转发请求的关键。它会解码消息，尝试将消息转发到目标节点。如果目标节点直接连接到中继，它会打开一个新的 stream 并转发；否则，它会通知发送者转发失败。
GetConnectedPeers: 维护一个连接到中继节点的 map，并提供一个方法来获取这些节点的 Peer ID 列表，供普通节点查询。


peer/peer_node.go:
NewPeerNode: 创建 libp2p.Host，并初始化 Kademlia DHT (dht.New) 用于节点发现。
ConnectToRelay: 节点启动时连接到指定的中继节点。
DiscoverPeers:
使用 dutil.Advertise(ctx, pn.Discovery, common.ChatProtocolID) 让当前节点在 DHT 中宣布自己支持聊天协议，这样其他节点就能发现它。
使用 pn.Discovery.FindPeers 在 DHT 中查找其他支持 ChatProtocolID 的节点。
当发现新节点时，尝试建立连接。关键是这里处理中继地址的构造：p.Addrs = append(p.Addrs, ma.StringCast(fmt.Sprintf("/p2p/%s/p2p-circuit/p2p/%s", pn.RelayPeerID.Pretty(), p.ID.Pretty()))) 这行代码很重要，它告诉 libp2p 尝试通过中继节点连接到目标节点。
ListConnectedPeers: 列出当前节点直接连接的对等节点。
GetPeersFromRelay: 普通节点向中继节点发送 /listpeers 消息，请求中继节点连接的对等节点列表。
SendMessage:
首先尝试直接连接发送消息。
如果直接发送失败或者没有直接连接，则尝试通过中继转发。这里有两种通过中继发送的方式：
p2p-circuit 转发: pn.Host.NewStream(ctx, targetID, common.ChatProtocolID) 如果 targetID 的 AddrInfo 包含了 p2p-circuit 地址（在 DiscoverPeers 中添加），libp2p 会自动尝试通过中继建立一个 "circuit" 流。
显式转发: 如果 p2p-circuit 无法建立，会回退到显式地将消息发送给中继节点，由中继节点处理转发逻辑 (handleRelayStream 中的 RelayProtocolID 部分)。这是为了确保即使 p2p-circuit 路由不通，中继仍能提供消息转发服务。
StartConsoleInput: 处理用户输入的命令，如 /send、/list_direct 和 /list_relay_peers。



peer/chat_handler.go: 简单地处理传入的聊天消息并打印到控制台。
main.go: 处理命令行参数，根据参数启动中继节点或普通节点。




NAT 穿越: 对于大多数普通节点，可能存在 NAT 穿越问题。libp2p 提供了 AutoRelay 和 NATPortMap 等功能来帮助解决，但在复杂的网络环境下仍可能需要手动配置端口映射或使用更高级的 NAT 穿越技术。
