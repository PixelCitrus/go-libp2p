package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	mrand "math/rand"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	discovery "github.com/libp2p/go-libp2p/p2p/discovery/mdns"
	relayv2 "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
	ma "github.com/multiformats/go-multiaddr"
)

const (
	chatProtocol      = "/p2p-chat/1.0.0"
	relayInfoProtocol = "/p2p-relay-info/1.0.0" // 新增：中继提供活跃节点列表的协议
	relayMode         = "relay"
	peerMode          = "peer"
)

var (
	mode       = flag.String("mode", peerMode, "节点模式：relay/peer")
	port       = flag.Int("port", 6666, "监听端口")
	relayAddr  = flag.String("relay", "", "中继节点地址")
	rendezvous = flag.String("rendezvous", "chat-with-relay", "节点发现标识")
	debug      = flag.Bool("debug", false, "调试模式生成固定节点ID")

	// streams 维护持久化流，用于发送消息
	streams sync.Map
	// globalRelayPeerID 保存已连接的中继节点的PeerID
	globalRelayPeerID peer.ID

	// activePeers 存储连接到中继的活跃Peer ID，仅用于中继节点
	activePeers     = make(map[peer.ID]peer.AddrInfo)
	activePeersLock sync.Mutex
)

// discoveryNotifee 用于处理 mDNS 发现的对等节点
type discoveryNotifee struct {
	host host.Host
}

func (n *discoveryNotifee) HandlePeerFound(pi peer.AddrInfo) {
	fmt.Printf("mDNS 发现新节点: %s\n", pi.ID)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := n.host.Connect(ctx, pi); err != nil {
		fmt.Printf("连接发现的节点 %s 失败: %v\n", pi.ID, err)
	} else {
		fmt.Printf("成功连接到发现的节点: %s\n", pi.ID)
	}
}

// main 函数入口
func main() {
	flag.Parse()
	ctx := context.Background()

	opts := []libp2p.Option{
		libp2p.ListenAddrStrings(
			fmt.Sprintf("/ip4/0.0.0.0/tcp/%d", *port),
			fmt.Sprintf("/ip6/::/tcp/%d", *port)),
		libp2p.NATPortMap(),       // 启用NAT端口映射
		libp2p.EnableNATService(), // 启用NAT服务供其他节点发现
		libp2p.Ping(true),         // 启用ping服务
	}

	if *debug {
		opts = append(opts, libp2p.Identity(genDebugKey(*port)))
	}

	if *mode == relayMode {
		// 对于中继节点，不启用 EnableRelay 和 WithAutoRelay
		createRelayNode(ctx, opts)
	} else {
		// 对于普通节点，启用 EnableRelay 和 WithAutoRelay
		opts = append(opts, libp2p.EnableRelay()) // 允许节点拨号和监听通过中继
		// opts = append(opts, libp2p.WithAutoRelay()) // 自动管理中继连接以确保可达性
		opts = append(opts, libp2p.EnableRelay())
		createPeerNode(ctx, opts)
	}
}

// createRelayNode 创建并启动一个中继节点
func createRelayNode(ctx context.Context, opts []libp2p.Option) {
	h, err := libp2p.New(opts...)
	if err != nil {
		log.Fatal(err)
	}

	// 启动中继服务
	_, err = relayv2.New(h)
	if err != nil {
		log.Fatal("无法启动中继服务:", err)
	}

	// 设置连接事件处理，以维护活跃节点列表
	h.Network().Notify(&network.NotifyBundle{
		ConnectedF: func(_ network.Network, conn network.Conn) {
			peerID := conn.RemotePeer()
			// 避免中继节点将自己添加到活跃列表中
			if peerID == h.ID() {
				return
			}
			log.Printf("中继节点：新连接来自 %s", peerID)
			activePeersLock.Lock()
			activePeers[peerID] = h.Peerstore().PeerInfo(peerID)
			activePeersLock.Unlock()
		},
		DisconnectedF: func(_ network.Network, conn network.Conn) {
			peerID := conn.RemotePeer()
			log.Printf("中继节点：连接断开 %s", peerID)
			activePeersLock.Lock()
			delete(activePeers, peerID)
			activePeersLock.Unlock()
		},
	})

	// 设置处理请求活跃节点列表的协议
	h.SetStreamHandler(relayInfoProtocol, func(s network.Stream) {
		defer s.Close()
		fmt.Printf("\n收到来自 %s 的活跃节点列表请求\n> ", s.Conn().RemotePeer())

		activePeersLock.Lock()
		defer activePeersLock.Unlock()

		var sb strings.Builder
		sb.WriteString("活跃节点列表:\n")
		// 将 activePeers 按照 Peer ID 排序，以便输出稳定
		var sortedPeerIDs []peer.ID
		for id := range activePeers {
			sortedPeerIDs = append(sortedPeerIDs, id)
		}
		// peer.SortByID(sortedPeerIDs) // 使用 libp2p 提供的排序方法

		for _, id := range sortedPeerIDs {
			// 排除中继节点本身
			if id != h.ID() {
				sb.WriteString(fmt.Sprintf("  %s\n", id))
			}
		}
		if len(activePeers) <= 1 { // 如果只有中继自己或没有其他对等节点
			sb.WriteString("  目前没有活跃的对等节点。\n")
		}

		_, err := s.Write([]byte(sb.String() + "\n"))
		if err != nil {
			fmt.Printf("发送活跃节点列表失败: %v\n", err)
		}
	})

	fmt.Printf("中继节点已启动:\nID: %s\n地址:\n", h.ID())
	for _, addr := range h.Addrs() {
		fmt.Printf("  %s/p2p/%s\n", addr, h.ID())
	}

	// 保持中继节点持续运行
	select {}
}

// createPeerNode 创建并启动一个普通P2P节点
func createPeerNode(ctx context.Context, opts []libp2p.Option) {
	h, err := libp2p.New(opts...)
	if err != nil {
		log.Fatal(err)
	}

	setupDiscovery(h)
	connectToRelay(h) // 所有普通节点必须先连接到中继
	setupChatProtocol(h)

	fmt.Printf("普通节点已启动:\nID: %s\n监听地址:\n", h.ID())
	for _, addr := range h.Addrs() {
		fmt.Println(" ", addr)
	}

	go monitorConnections(h)
	go readConsoleInput(h)
	select {}
}

// setupDiscovery 设置mDNS节点发现服务 (主要用于本地调试或补充发现)
func setupDiscovery(h host.Host) {
	discoveryService := discovery.NewMdnsService(h, *rendezvous, &discoveryNotifee{host: h})
	if err := discoveryService.Start(); err != nil {
		log.Fatal("启动节点发现失败:", err)
	}
	fmt.Println("mDNS 发现服务已启动。")
}

// connectToRelay 连接到指定的中继节点
func connectToRelay(h host.Host) {
	if *relayAddr == "" {
		log.Fatal("普通节点必须指定中继节点地址 (-relay 参数)。")
	}

	maddr, err := ma.NewMultiaddr(*relayAddr)
	if err != nil {
		log.Fatalf("解析中继地址失败: %v", err)
	}

	relayInfo, err := peer.AddrInfoFromP2pAddr(maddr)
	if err != nil {
		log.Fatalf("获取中继信息失败: %v", err)
	}

	log.Printf("解析出的中继 Peer ID: %s, 地址: %v", relayInfo.ID, relayInfo.Addrs)

	h.Peerstore().AddAddrs(relayInfo.ID, relayInfo.Addrs, peerstore.PermanentAddrTTL)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second) // 增加超时时间以应对网络波动
	defer cancel()

	fmt.Printf("尝试连接中继节点: %s\n", relayInfo.ID)
	if err := h.Connect(ctx, *relayInfo); err != nil {
		log.Fatalf("连接中继失败: %v", err)
	}

	globalRelayPeerID = relayInfo.ID
	fmt.Printf("成功连接到中继节点: %s\n", globalRelayPeerID)
}

// setupChatProtocol 设置聊天协议的处理逻辑
func setupChatProtocol(h host.Host) {
	h.SetStreamHandler(chatProtocol, func(s network.Stream) {
		fmt.Printf("\n新聊天连接来自: %s\n> ", s.Conn().RemotePeer())
		streams.Store(s.Conn().RemotePeer(), s) // 存储新流
		go readStream(s)
	})
	fmt.Println("聊天协议处理程序已设置。")
}

// readStream 从流中读取数据
func readStream(s network.Stream) {
	r := bufio.NewReader(s)
	for {
		msg, err := r.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				fmt.Printf("\n连接 %s 已优雅关闭\n> ", s.Conn().RemotePeer())
			} else {
				fmt.Printf("\n连接 %s 读取错误: %v\n> ", s.Conn().RemotePeer(), err)
			}
			streams.Delete(s.Conn().RemotePeer()) // 连接断开时从map中移除流
			return
		}
		// 判断接收到的消息是通过直接连接还是中继
		conn := s.Conn()
		isRelayed := false
		// 检查连接的远程地址是否包含 p2p-circuit 段
		// 或者检查连接的传输协议是否是 circuitv2
		// RemoteMultiaddr() 会给出实际使用的传输地址
		remoteAddr := conn.RemoteMultiaddr()
		if remoteAddr != nil && strings.Contains(remoteAddr.String(), "/p2p-circuit") {
			isRelayed = true
		}

		if isRelayed {
			fmt.Printf("\n[通过中继 | 来自 %s] %s> ", s.Conn().RemotePeer(), msg)
		} else {
			fmt.Printf("\n[直接连接 | 来自 %s] %s> ", s.Conn().RemotePeer(), msg)
		}
	}
}

// readConsoleInput 读取控制台输入并执行命令
func readConsoleInput(h host.Host) {
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("> ")
		input, _ := reader.ReadString('\n')
		input = strings.TrimSpace(input)

		parts := strings.Fields(input)
		if len(parts) == 0 {
			continue
		}

		command := parts[0]
		switch command {
		case "/list_active":
			// 请求中继节点给出活跃节点列表
			requestActivePeersFromRelay(h)
		case "/send":
			if len(parts) < 3 {
				fmt.Println("用法: /send <目标PeerID> <消息>")
				continue
			}
			targetPeerIDStr := parts[1]
			message := strings.Join(parts[2:], " ")

			targetPeerID, err := peer.Decode(targetPeerIDStr)
			if err != nil {
				fmt.Printf("无效的Peer ID: %v\n", err)
				continue
			}
			sendMessage(h, targetPeerID, message)
		case "/connect": // 允许用户主动连接其他对等节点（可能通过它们的multiaddr）
			if len(parts) < 2 {
				fmt.Println("用法: /connect <multiaddr>")
				continue
			}
			connectToPeer(h, parts[1])
		case "/peers": // 显示本地已连接的对等节点
			fmt.Println("本地已连接的活跃节点:")
			foundPeers := false
			for _, p := range h.Network().Peers() {
				if p != h.ID() && p != globalRelayPeerID { // 排除自身和中继
					// 额外判断是直接连接还是通过中继的连接
					conn := h.Network().ConnsToPeer(p)
					if len(conn) > 0 {
						isRelayed := false
						remoteAddr := conn[0].RemoteMultiaddr()
						if remoteAddr != nil && strings.Contains(remoteAddr.String(), "/p2p-circuit") {
							isRelayed = true
						}
						if isRelayed {
							fmt.Printf("  %s (通过中继)\n", p)
						} else {
							fmt.Printf("  %s (直接连接)\n", p)
						}
						foundPeers = true
					}
				}
			}
			if !foundPeers {
				fmt.Println("  目前没有直接连接到其他对等节点。")
			}
		case "/exit":
			fmt.Println("正在退出...")
			h.Close()
			os.Exit(0)
		default:
			fmt.Println("未知命令。可用命令: /list_active, /send, /connect, /peers, /exit")
		}
	}
}

// requestActivePeersFromRelay 向中继节点请求活跃节点列表
func requestActivePeersFromRelay(h host.Host) {
	if globalRelayPeerID == "" {
		fmt.Println("未连接到中继节点，无法获取活跃节点列表。")
		return
	}

	fmt.Printf("正在向中继节点 %s 请求活跃节点列表...\n", globalRelayPeerID)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	s, err := h.NewStream(ctx, globalRelayPeerID, relayInfoProtocol)
	if err != nil {
		fmt.Printf("无法创建流到中继节点以获取列表: %v\n", err)
		return
	}
	defer s.Close() // 确保流关闭

	reader := bufio.NewReader(s)
	response, err := reader.ReadString('\n')
	if err != nil {
		fmt.Printf("从中继节点读取响应失败: %v\n", err)
		return
	}
	fmt.Print(response) // 打印中继节点返回的活跃节点列表
}

// sendMessage 向指定目标节点发送消息
func sendMessage(h host.Host, targetPeerID peer.ID, msg string) {
	// 如果目标是自己，则不发送
	if targetPeerID == h.ID() {
		fmt.Println("不能给自己发送消息。")
		return
	}
	// 如果目标是中继节点，则不发送聊天消息（中继仅用于转发）
	if targetPeerID == globalRelayPeerID {
		fmt.Println("不能直接向中继节点发送聊天消息。中继节点仅用于转发。")
		return
	}

	// 尝试加载现有流
	sVal, ok := streams.Load(targetPeerID)
	var s network.Stream
	var err error

	if ok {
		s, ok = sVal.(network.Stream)
		if !ok {
			// 类型断言失败，存储的值有问题。删除并重新创建。
			streams.Delete(targetPeerID)
			ok = false // 强制流重建
		}
	}

	if !ok || s == nil { // 如果没有现有流或类型断言失败
		// 尝试创建到目标节点的新流
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second) // 增加超时时间
		s, err = h.NewStream(ctx, targetPeerID, chatProtocol)
		cancel()

		if err != nil {
			fmt.Printf("无法创建流到 %s: %v\n", targetPeerID, err)
			return // 失败就直接返回，避免无限循环尝试
		}
		streams.Store(targetPeerID, s) // 存储新流
		go readStream(s)               // 开始从这个新流读取
		fmt.Printf("已建立新流到 %s\n", targetPeerID)
	}

	// 发送消息
	_, err = s.Write([]byte(msg + "\n"))
	if err != nil {
		fmt.Printf("发送失败到 %s: %v\n", targetPeerID, err)
		streams.Delete(targetPeerID) // 如果写入失败则删除流
		return
	}

	// === 新增：判断发送路径并记录日志 ===
	conn := s.Conn()
	isRelayed := false
	// 检查连接的远程地址是否包含 p2p-circuit 段
	remoteAddr := conn.RemoteMultiaddr()
	if remoteAddr != nil && strings.Contains(remoteAddr.String(), "/p2p-circuit") {
		isRelayed = true
	}

	if isRelayed {
		fmt.Printf("消息已通过中继发送到 %s\n", targetPeerID)
	} else {
		fmt.Printf("消息已直接发送到 %s\n", targetPeerID)
	}
}

func connectToPeer(h host.Host, addr string) {
	maddr, err := ma.NewMultiaddr(addr)
	if err != nil {
		fmt.Printf("地址解析错误: %v\n", err)
		return
	}

	peerInfo, err := peer.AddrInfoFromP2pAddr(maddr)
	if err != nil {
		fmt.Printf("节点信息错误: %v\n", err)
		return
	}

	h.Peerstore().AddAddrs(peerInfo.ID, peerInfo.Addrs, peerstore.PermanentAddrTTL)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	fmt.Printf("尝试连接到 %s\n", peerInfo.ID)
	if err := h.Connect(ctx, *peerInfo); err != nil {
		fmt.Printf("连接失败到 %s: %v\n", peerInfo.ID, err)
		return
	}
	fmt.Printf("成功连接到 %s\n", peerInfo.ID)
}

// genDebugKey 生成用于调试的固定私钥
func genDebugKey(port int) crypto.PrivKey {
	priv, _, err := crypto.GenerateKeyPairWithReader(
		crypto.RSA,
		2048,
		mrand.New(mrand.NewSource(int64(port))), // 使用端口作为种子以获得一致的调试ID
	)
	if err != nil {
		panic(err)
	}
	return priv
}

// monitorConnections 监控并打印当前连接状态
func monitorConnections(h host.Host) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		fmt.Println("\n当前连接状态:")
		if globalRelayPeerID != "" {
			fmt.Printf("已连接中继节点: %s\n", globalRelayPeerID)
		} else {
			fmt.Println("未连接到中继节点。")
		}

		fmt.Println("本地活跃的对等节点:")
		foundChatPeers := false
		for _, p := range h.Network().Peers() {
			if p != h.ID() && p != globalRelayPeerID { // 排除自身和中继节点
				// 额外判断是直接连接还是通过中继的连接
				conns := h.Network().ConnsToPeer(p)
				if len(conns) > 0 {
					isRelayed := false
					remoteAddr := conns[0].RemoteMultiaddr()
					if remoteAddr != nil && strings.Contains(remoteAddr.String(), "/p2p-circuit") {
						isRelayed = true
					}
					if isRelayed {
						fmt.Printf("  %s (通过中继)\n", p)
					} else {
						fmt.Printf("  %s (直接连接)\n", p)
					}
					foundChatPeers = true
				}
			}
		}
		if !foundChatPeers {
			fmt.Println("  目前没有连接到其他对等节点。")
		}
		fmt.Print("> ")
	}
}
