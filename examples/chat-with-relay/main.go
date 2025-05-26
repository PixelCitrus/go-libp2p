package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
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
	chatProtocol = "/p2p-chat/1.0.0"
	relayMode    = "relay"
	peerMode     = "peer"
)

var (
	mode       = flag.String("mode", peerMode, "节点模式：relay/peer")
	port       = flag.Int("port", 6666, "监听端口")
	relayAddr  = flag.String("relay", "", "中继节点地址")
	rendezvous = flag.String("rendezvous", "chat-with-relay", "节点发现标识")
	debug      = flag.Bool("debug", false, "调试模式生成固定节点ID")

	// streams 维护持久化流
	streams sync.Map
	// relayPeerID 保存已连接的中继节点的PeerID
	// 成功连接到中继后设置此值
	globalRelayPeerID peer.ID
)

type discoveryNotifee struct {
	host host.Host
}

func (n *discoveryNotifee) HandlePeerFound(pi peer.AddrInfo) {
	// 如果指定了中继地址，尝试将其添加到发现的节点中
	if *relayAddr != "" {
		_, err := ma.NewMultiaddr(*relayAddr)
		if err == nil {
			// 这里原本尝试将中继地址添加到发现节点的地址列表中
			// 但实际上应该直接通过中继节点建立连接
			// 现在保留空实现，因为libp2p的自动中继机制会处理中继连接
		}
	}

	fmt.Printf("发现新节点: %s\n", pi.ID)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := n.host.Connect(ctx, pi); err != nil {
		fmt.Printf("连接失败: %v\n", err)
	}
}

// 主函数入口
func main() {
	flag.Parse()
	ctx := context.Background()

	// 配置libp2p选项
	opts := []libp2p.Option{
		libp2p.ListenAddrStrings(
			fmt.Sprintf("/ip4/0.0.0.0/tcp/%d", *port),
			fmt.Sprintf("/ip6/::/tcp/%d", *port)),
		libp2p.EnableRelay(),      // 启用中继拨号和监听
		libp2p.NATPortMap(),       // 启用NAT端口映射
		libp2p.EnableNATService(), // 启用NAT服务供其他节点发现
		libp2p.Ping(true),         // 启用ping服务
	}

	// 调试模式下生成固定节点ID
	if *debug {
		opts = append(opts, libp2p.Identity(genDebugKey(*port)))
	}

	// 根据模式创建不同类型的节点
	if *mode == relayMode {
		createRelayNode(ctx, opts)
	} else {
		createPeerNode(ctx, opts)
	}
}

// 创建中继节点
func createRelayNode(ctx context.Context, opts []libp2p.Option) {
	// 对于中继节点，我们禁用客户端中继功能
	// 因为它本身提供中继服务
	opts = append(opts, libp2p.DisableRelay())
	h, err := libp2p.New(opts...)
	if err != nil {
		log.Fatal(err)
	}

	// 启动中继服务
	_, err = relayv2.New(h)
	if err != nil {
		log.Fatal("无法启动中继服务:", err)
	}

	fmt.Printf("中继节点已启动:\nID: %s\n地址:\n", h.ID())
	for _, addr := range h.Addrs() {
		fmt.Printf("  %s/p2p/%s\n", addr, h.ID())
	}

	// 保持中继节点持续运行
	select {}
}

// createPeerNode 创建并启动一个普通P2P节点
// 参数:
//
//	ctx: 上下文对象，用于控制goroutine生命周期
//	opts: libp2p配置选项，包括监听地址、传输协议等
func createPeerNode(ctx context.Context, opts []libp2p.Option) {
	// 使用提供的选项创建libp2p主机实例
	h, err := libp2p.New(opts...)
	if err != nil {
		log.Fatal(err)
	}

	// 设置节点发现服务(mDNS)
	setupDiscovery(h)
	// 连接到中继节点(如果配置了中继地址)
	// 连接成功后会将中继Peer ID存储到globalRelayPeerID
	connectToRelay(h)
	// 设置聊天协议处理器
	setupChatProtocol(h)

	// 打印节点启动信息
	fmt.Printf("普通节点已启动:\nID: %s\n监听地址:\n", h.ID())
	for _, addr := range h.Addrs() {
		fmt.Println(" ", addr)
	}

	// 启动连接监控goroutine
	go monitorConnections(h)
	// 启动控制台输入读取goroutine
	go readConsoleInput(h)
	// 阻塞主线程，保持节点运行
	select {}
}

// setupDiscovery 设置mDNS节点发现服务
// 参数:
//
//	h: libp2p主机实例
func setupDiscovery(h host.Host) {
	// 创建mDNS发现服务实例
	// rendezvous参数用于标识发现组，同一组的节点才能互相发现
	discoveryService := discovery.NewMdnsService(h, *rendezvous, &discoveryNotifee{host: h})
	// 启动发现服务
	if err := discoveryService.Start(); err != nil {
		log.Fatal("启动节点发现失败:", err)
	}
	fmt.Println("mDNS 发现服务已启动。")
}

func connectToRelay(h host.Host) {
	if *relayAddr == "" {
		fmt.Println("未指定中继节点地址，跳过中继连接。")
		return
	}

	maddr, err := ma.NewMultiaddr(*relayAddr)
	if err != nil {
		log.Fatalf("解析中继地址失败: %v", err)
	}

	// 从multiaddr中解析出Peer ID和地址信息
	// AddrInfoFromP2pAddr专门用于处理包含/p2p/PeerID的multiaddr
	relayInfo, err := peer.AddrInfoFromP2pAddr(maddr)
	if err != nil {
		log.Fatalf("获取中继信息失败: %v", err)
	}

	log.Printf("解析出的中继 Peer ID: %s, 地址: %v", relayInfo.ID, relayInfo.Addrs)

	// 将中继节点的地址添加到peerstore中，以便libp2p知道如何连接
	h.Peerstore().AddAddrs(relayInfo.ID, relayInfo.Addrs, peerstore.PermanentAddrTTL)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	fmt.Printf("尝试连接中继节点: %s\n", relayInfo.ID)
	if err := h.Connect(ctx, *relayInfo); err != nil {
		log.Fatalf("连接中继失败: %v", err)
	}

	// 连接成功后，将中继节点的Peer ID存储到全局变量中
	globalRelayPeerID = relayInfo.ID
	fmt.Printf("成功连接到中继节点: %s\n", globalRelayPeerID)
}

// setupChatProtocol 设置聊天协议的处理逻辑
// 参数:
//
//	h: libp2p主机实例
func setupChatProtocol(h host.Host) {
	// 为聊天协议设置流处理器
	h.SetStreamHandler(chatProtocol, func(s network.Stream) {
		fmt.Printf("\n新连接来自: %s\n> ", s.Conn().RemotePeer())
		// 存储新建立的流以便后续使用(虽然readStream会处理流的生命周期)
		// 这样可以确保我们需要时能够通过这个流进行回复
		streams.Store(s.Conn().RemotePeer(), s)
		// 启动goroutine读取流中的数据
		go readStream(s)
	})
	fmt.Println("聊天协议处理程序已设置。")
}

// 读取流数据
func readStream(s network.Stream) {
	// 流应由发起关闭的一方关闭
	// 如果远端节点关闭了它们那端的流，ReadString会返回EOF错误
	// 我们只在我们完成发送或发生不可恢复的错误时关闭我们这端的流
	// defer s.Close() // 已移除以保持流开放用于发送消息

	r := bufio.NewReader(s)
	for {
		msg, err := r.ReadString('\n')
		if err != nil {
			fmt.Printf("\n连接 %s 已关闭 (%v)\n> ", s.Conn().RemotePeer(), err)
			streams.Delete(s.Conn().RemotePeer()) // 断开连接时从map中移除流
			return
		}
		fmt.Printf("\n[来自 %s] %s> ", s.Conn().RemotePeer(), msg)
	}
}

func readConsoleInput(h host.Host) {
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("> ")
		msg, _ := reader.ReadString('\n')
		msg = strings.TrimSpace(msg)

		if strings.HasPrefix(msg, "/connect") {
			targetAddr := strings.TrimSpace(msg[8:])
			if targetAddr != "" {
				connectToPeer(h, targetAddr)
			} else {
				fmt.Println("Usage: /connect <multiaddr>")
			}
			continue
		} else if strings.HasPrefix(msg, "/peers") {
			fmt.Println("已连接的活跃节点:")
			for _, p := range h.Network().Peers() {
				// Don't show self or the relay in the regular chat peers list
				if p != h.ID() && (globalRelayPeerID == "" || p != globalRelayPeerID) {
					fmt.Printf("  %s\n", p)
				}
			}
			continue
		} else if msg == "/exit" {
			fmt.Println("正在退出...")
			h.Close()
			os.Exit(0)
		}

		broadcastMessage(h, msg)
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

func genDebugKey(port int) crypto.PrivKey {
	priv, _, err := crypto.GenerateKeyPairWithReader(
		crypto.RSA,
		2048,
		mrand.New(mrand.NewSource(int64(port))), // Use port as seed for consistent debug IDs
	)
	if err != nil {
		panic(err)
	}
	return priv
}

// broadcastMessage 向所有连接的聊天节点广播消息
// 参数:
//
//	h: libp2p主机实例
//	msg: 要发送的消息内容
func broadcastMessage(h host.Host, msg string) {
	// 获取当前连接的所有节点
	peers := h.Network().Peers()

	// 过滤出可用于聊天的节点（排除中继节点和自身）
	var chatPeers []peer.ID
	for _, p := range peers {
		// 排除自身中和继节点
		if p != h.ID() && (globalRelayPeerID == "" || p != globalRelayPeerID) {
			chatPeers = append(chatPeers, p)
		}
	}

	// 如果没有可聊天的节点，直接返回
	if len(chatPeers) == 0 {
		fmt.Println("没有其他可聊天的节点。")
		return
	}

	// 遍历所有聊天节点发送消息
	for _, p := range chatPeers {
		// 尝试加载已有的流
		sVal, ok := streams.Load(p)
		var s network.Stream
		var err error

		if ok {
			s, ok = sVal.(network.Stream)
			if !ok {
				// 类型断言失败，存储的值有问题
				// 删除并重新创建流
				streams.Delete(p)
				ok = false // 强制重新创建流
			}
		}

		if !ok || s == nil { // 如果没有现有流或类型断言失败
			// 创建到对等节点的新流
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			s, err = h.NewStream(ctx, p, chatProtocol)
			cancel() // 释放与上下文关联的资源

			if err != nil {
				fmt.Printf("无法创建流到 %s: %v\n", p, err)
				// 不存储无效流，继续下一个节点
				continue
			}
			streams.Store(p, s) // 存储新流
			go readStream(s)    // 开始从这个新流读取
			fmt.Printf("已建立新流到 %s\n", p)
		}

		// 发送消息
		_, err = s.Write([]byte(msg + "\n"))
		if err != nil {
			fmt.Printf("发送失败到 %s: %v\n", p, err)
			streams.Delete(p) // 如果写入失败则删除流
			// 可以考虑在下一次广播时重新建立流
		}

		// 记录消息是直接发送还是通过中继发送
		if globalRelayPeerID != "" && p == globalRelayPeerID {
			fmt.Printf("消息通过中继发送到 %s\n", p)
		} else {
			fmt.Printf("消息直接发送到 %s\n", p)
		}
	}
}

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

		fmt.Println("活跃的聊天节点:")
		foundChatPeers := false
		for _, p := range h.Network().Peers() {
			// Exclude self and the designated relay peer
			if p != h.ID() && (globalRelayPeerID == "" || p != globalRelayPeerID) {
				fmt.Printf("  %s\n", p)
				foundChatPeers = true
			}
		}
		if !foundChatPeers {
			fmt.Println("  没有其他活跃的聊天节点。")
		}
		fmt.Print("> ")
	}
}
