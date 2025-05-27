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
	relayInfoProtocol = "/p2p-relay-info/1.0.0" // 中继提供活跃节点列表的协议
	connReqProtocol   = "/p2p-conn-req/1.0.0"   // 连接请求协议
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

	// stdinReader 共享的 bufio.Reader
	stdinReader *bufio.Reader
	// inputMtx 用于同步对 stdinReader 的访问
	inputMtx sync.Mutex

	// currentChatTarget 存储当前正在一对一聊天的目标Peer ID
	currentChatTarget    peer.ID
	currentChatTargetMtx sync.Mutex
)

// discoveryNotifee 用于处理 mDNS 发现的对等节点
type discoveryNotifee struct {
	host host.Host
}

func (n *discoveryNotifee) HandlePeerFound(pi peer.AddrInfo) {
	fmt.Printf("mDNS 发现新节点: %s\n", pi.ID)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 尝试连接发现的节点
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

	// 初始化共享的 stdinReader
	stdinReader = bufio.NewReader(os.Stdin)

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
		createRelayNode(ctx, opts)
	} else {
		// 普通节点默认启用中继客户端功能，以便通过中继连接
		opts = append(opts, libp2p.EnableRelay())
		createPeerNode(ctx, opts)
	}
}

// --- Relay 节点功能 ---

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

	// 监听网络连接事件，更新活跃节点列表
	h.Network().Notify(&network.NotifyBundle{
		ConnectedF: func(_ network.Network, conn network.Conn) {
			peerID := conn.RemotePeer()
			if peerID == h.ID() {
				return // 忽略连接到自己的情况
			}
			log.Printf("中继节点：新连接来自 %s", peerID)
			activePeersLock.Lock()
			// 只要建立连接就添加到活跃列表
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

	// 设置中继信息协议处理程序，响应活跃节点列表请求
	h.SetStreamHandler(relayInfoProtocol, func(s network.Stream) {
		// 不在这里 defer s.Close()，让它在发送完所有数据后自行关闭，或者明确在写入后关闭
		fmt.Printf("\n收到来自 %s 的活跃节点列表请求\n> ", s.Conn().RemotePeer())

		activePeersLock.Lock()
		defer activePeersLock.Unlock()

		var sb strings.Builder
		// 明确说明这个列表的含义
		sb.WriteString("活跃节点列表 (与本中继有直接连接的其他对等节点):\n")
		var sortedPeerIDs []peer.ID
		for id := range activePeers {
			sortedPeerIDs = append(sortedPeerIDs, id)
		}
		// peer.SortByID(sortedPeerIDs) // 对Peer ID进行排序，以便输出一致

		foundPeers := false
		for _, id := range sortedPeerIDs {
			if id != h.ID() { // 不列出中继节点自己
				sb.WriteString(fmt.Sprintf("    %s\n", id))
				foundPeers = true
			}
		}
		if !foundPeers {
			sb.WriteString("    目前没有其他对等节点连接到本中继。\n")
		}
		// 在列表内容结束后，再加一个额外的换行符，以便接收端可以更确定地读取到末尾
		sb.WriteString("\n")

		_, err := s.Write([]byte(sb.String()))
		if err != nil {
			fmt.Printf("发送活跃节点列表失败: %v\n", err)
		}
		// 写入完成后关闭流
		s.Close()
	})

	fmt.Printf("中继节点已启动:\nID: %s\n地址:\n", h.ID())
	for _, addr := range h.Addrs() {
		fmt.Printf("    %s/p2p/%s\n", addr, h.ID())
	}

	select {} // 阻塞主goroutine，使节点持续运行
}

// --- 普通 Peer 节点功能 ---

// createPeerNode 创建并启动一个普通P2P节点
func createPeerNode(ctx context.Context, opts []libp2p.Option) {
	h, err := libp2p.New(opts...)
	if err != nil {
		log.Fatal(err)
	}

	setupDiscovery(h)                 // 设置mDNS发现
	connectToRelay(h)                 // 连接到指定中继
	setupChatProtocol(h)              // 设置聊天协议处理
	setupConnectionRequestProtocol(h) // 设置连接请求协议处理

	fmt.Printf("普通节点已启动:\nID: %s\n监听地址:\n", h.ID())
	for _, addr := range h.Addrs() {
		fmt.Println(" ", addr)
	}

	go monitorConnections(h) // 启动连接状态监控
	go readConsoleInput(h)   // 启动控制台输入处理
	select {}                // 阻塞主goroutine，使节点持续运行
}

// setupDiscovery 设置mDNS节点发现服务
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

	// 将中继地址信息添加到Peerstore，以便libp2p知道如何连接
	h.Peerstore().AddAddrs(relayInfo.ID, relayInfo.Addrs, peerstore.PermanentAddrTTL)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
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
		remotePeer := s.Conn().RemotePeer()
		fmt.Printf("\n新聊天连接来自: %s\n", remotePeer)
		streams.Store(remotePeer, s) // 存储新建立的聊天流
		go readStream(s)             // 为每个流启动一个读取goroutine
		fmt.Print("> ")              // 重新打印提示符
	})
	fmt.Println("聊天协议处理程序已设置。")
}

// setupConnectionRequestProtocol 设置连接请求协议的处理逻辑
func setupConnectionRequestProtocol(h host.Host) {
	h.SetStreamHandler(connReqProtocol, func(s network.Stream) {
		remotePeer := s.Conn().RemotePeer()
		fmt.Printf("\n收到来自 %s 的连接请求。是否接受？(yes/no)\n", remotePeer)

		// 确保在读取用户输入时，只有当前处理程序能访问 stdinReader
		inputMtx.Lock()
		fmt.Print("> ") // 打印提示，让用户知道需要输入
		response, err := stdinReader.ReadString('\n')
		inputMtx.Unlock() // 解锁
		if err != nil {
			fmt.Printf("读取连接请求响应失败: %v\n", err)
			s.Close()
			return
		}
		response = strings.TrimSpace(strings.ToLower(response))

		if response == "yes" {
			_, err := s.Write([]byte("accepted\n"))
			if err != nil {
				fmt.Printf("发送接受响应失败: %v\n", err)
				s.Close()
				return
			}
			fmt.Printf("已接受 %s 的连接请求。等待对方建立聊天流。\n", remotePeer)
		} else {
			_, err := s.Write([]byte("rejected\n"))
			if err != nil {
				fmt.Printf("发送拒绝响应失败: %v\n", err)
			}
			s.Close()
			fmt.Printf("已拒绝 %s 的连接请求。\n", remotePeer)
		}
		fmt.Print("> ") // 重新打印提示符
	})
	fmt.Println("连接请求协议处理程序已设置。")
}

// readStream 从流中读取数据
func readStream(s network.Stream) {
	r := bufio.NewReader(s)
	for {
		msg, err := r.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				fmt.Printf("\n连接 %s 已优雅关闭\n", s.Conn().RemotePeer())
			} else {
				fmt.Printf("\n连接 %s 读取错误: %v\n", s.Conn().RemotePeer(), err)
			}
			s.Close()                             // 确保流被关闭
			streams.Delete(s.Conn().RemotePeer()) // 从活跃流中移除

			// 如果当前聊天目标流关闭，重置 currentChatTarget
			currentChatTargetMtx.Lock()
			if currentChatTarget == s.Conn().RemotePeer() {
				currentChatTarget = ""
				fmt.Println("已退出一对一聊天模式。")
			}
			currentChatTargetMtx.Unlock()
			fmt.Print("> ") // 重新打印提示符
			return
		}
		conn := s.Conn()
		isRelayed := false
		remoteAddr := conn.RemoteMultiaddr()
		if remoteAddr != nil && strings.Contains(remoteAddr.String(), "/p2p-circuit") {
			isRelayed = true
		}

		if isRelayed {
			fmt.Printf("\n[通过中继 | 来自 %s] %s", s.Conn().RemotePeer(), msg) // msg 已经包含换行符
		} else {
			fmt.Printf("\n[直接连接 | 来自 %s] %s", s.Conn().RemotePeer(), msg) // msg 已经包含换行符
		}
		fmt.Print("> ") // 重新打印提示符
	}
}

// readConsoleInput 读取控制台输入并执行命令
func readConsoleInput(h host.Host) {
	for {
		// 在读取控制台输入前加锁
		inputMtx.Lock()
		fmt.Print("> ")
		input, err := stdinReader.ReadString('\n')
		inputMtx.Unlock() // 解锁

		if err != nil {
			log.Printf("读取控制台输入错误: %v", err)
			time.Sleep(1 * time.Second) // 避免错误循环过快
			continue
		}
		input = strings.TrimSpace(input)

		if len(input) == 0 {
			continue
		}

		// 检查是否是命令
		if strings.HasPrefix(input, "/") {
			parts := strings.Fields(input)
			command := parts[0]
			switch command {
			case "/list_active":
				currentChatTargetMtx.Lock()
				if currentChatTarget != "" {
					fmt.Println("请先退出一对一聊天模式 (/exit_chat) 再使用此命令。")
					currentChatTargetMtx.Unlock()
					continue
				}
				currentChatTargetMtx.Unlock()
				requestActivePeersFromRelay(h)
			case "/send":
				currentChatTargetMtx.Lock()
				if currentChatTarget != "" {
					fmt.Println("您已处于一对一聊天模式，请直接输入消息发送。")
					currentChatTargetMtx.Unlock()
					continue
				}
				currentChatTargetMtx.Unlock()

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
			case "/connect":
				currentChatTargetMtx.Lock()
				if currentChatTarget != "" {
					fmt.Println("您已处于一对一聊天模式，请先退出 (/exit_chat)。")
					currentChatTargetMtx.Unlock()
					continue
				}
				currentChatTargetMtx.Unlock()

				if len(parts) < 2 {
					fmt.Println("用法: /connect <目标PeerID>")
					continue
				}
				targetPeerIDStr := parts[1]
				targetPeerID, err := peer.Decode(targetPeerIDStr)
				if err != nil {
					fmt.Printf("无效的Peer ID: %v\n", err)
					continue
				}
				connectToPeerByID(h, targetPeerID)
			case "/disconnect":
				if len(parts) < 2 {
					fmt.Println("用法: /disconnect <目标PeerID>")
					continue
				}
				targetPeerIDStr := parts[1]
				targetPeerID, err := peer.Decode(targetPeerIDStr)
				if err != nil {
					fmt.Printf("无效的Peer ID: %v\n", err)
					continue
				}
				disconnectFromPeer(targetPeerID)
			case "/peers":
				currentChatTargetMtx.Lock()
				if currentChatTarget != "" {
					fmt.Println("请先退出一对一聊天模式 (/exit_chat) 再使用此命令。")
					currentChatTargetMtx.Unlock()
					continue
				}
				currentChatTargetMtx.Unlock()

				// 明确说明这里是本地已建立聊天连接的节点
				fmt.Println("本地已建立聊天连接的活跃节点:")
				foundChatPeers := false
				streams.Range(func(key, value interface{}) bool {
					pID, ok := key.(peer.ID)
					if !ok {
						return true
					}
					_, ok = value.(network.Stream)
					if !ok {
						return true
					}

					// 不列出自己或中继节点（聊天连接列表）
					if pID != h.ID() && pID != globalRelayPeerID {
						conns := h.Network().ConnsToPeer(pID)
						isRelayed := false
						if len(conns) > 0 {
							// 检查是否有任何连接是通过中继建立的
							for _, conn := range conns {
								if conn.RemoteMultiaddr() != nil && strings.Contains(conn.RemoteMultiaddr().String(), "/p2p-circuit") {
									isRelayed = true
									break
								}
							}
						}

						if isRelayed {
							fmt.Printf("    %s (聊天流活跃, 通过中继)\n", pID)
						} else {
							fmt.Printf("    %s (聊天流活跃, 直接连接)\n", pID)
						}
						foundChatPeers = true
					}
					return true
				})
				if !foundChatPeers {
					fmt.Println("    目前没有活跃的聊天连接。")
				}
			case "/exit_chat": // 退出一对一聊天模式
				currentChatTargetMtx.Lock()
				if currentChatTarget != "" {
					fmt.Printf("已退出与 %s 的一对一聊天模式。\n", currentChatTarget)
					currentChatTarget = ""
				} else {
					fmt.Println("当前不在一对一聊天模式中。")
				}
				currentChatTargetMtx.Unlock()
			case "/exit":
				fmt.Println("正在退出...")
				h.Close()
				os.Exit(0)
			default:
				fmt.Println("未知命令。可用命令: /list_active, /send, /connect, /disconnect, /peers, /exit_chat, /exit")
			}
		} else { // 不是命令，作为消息处理
			currentChatTargetMtx.Lock()
			target := currentChatTarget
			currentChatTargetMtx.Unlock()

			if target != "" {
				// 如果处于一对一聊天模式，将消息发送给目标
				sendMessage(h, target, input)
			} else {
				// 否则，广播给所有活跃的聊天流
				broadcastMessage(h, input)
			}
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
	// 不在这里 defer s.Close()，让它在读取完成或出错时再关闭

	// 关键修改：使用 io.ReadAll 确保读取所有内容
	responseBytes, err := io.ReadAll(s)
	s.Close() // 读取完成后关闭流

	if err != nil {
		fmt.Printf("从中继节点读取响应失败: %v\n", err)
		return
	}
	fmt.Print(string(responseBytes)) // 打印中继返回的列表
}

// sendMessage 向指定目标节点发送消息
func sendMessage(h host.Host, targetPeerID peer.ID, msg string) {
	if targetPeerID == h.ID() {
		fmt.Println("不能给自己发送消息。")
		return
	}
	if targetPeerID == globalRelayPeerID {
		fmt.Println("不能直接向中继节点发送聊天消息。中继节点仅用于转发。")
		return
	}

	sVal, ok := streams.Load(targetPeerID)
	var s network.Stream
	var err error

	if ok {
		s, ok = sVal.(network.Stream)
		if !ok {
			// 如果存储的不是Stream类型，移除并尝试重新创建
			streams.Delete(targetPeerID)
			ok = false
		} else if s.Stat().Direction == network.DirUnknown || s.Stat().Direction == network.DirInbound {
			// 对于入站流，可能无法直接写，但理论上 libp2p 应该处理好。
			// 这里的检查是为了确保流可用，如果流的状态异常，也尝试重新创建。
			// 但通常在聊天场景，双向流是预期的。
			// 简单的判断 s.Stat().Direction == network.DirUnknown 可能不足以判断流是否真正活跃。
			// 更可靠的方式是尝试写，如果失败就重建。
		}
	}

	if !ok || s == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		// 尝试建立一个新的聊天流
		s, err = h.NewStream(ctx, targetPeerID, chatProtocol)
		cancel()

		if err != nil {
			fmt.Printf("无法创建流到 %s: %v\n", targetPeerID, err)
			return
		}
		streams.Store(targetPeerID, s) // 存储新创建的流
		go readStream(s)               // 为新流启动读取goroutine
		fmt.Printf("已建立新流到 %s\n", targetPeerID)
	}

	if msg != "" {
		_, err = s.Write([]byte(msg + "\n"))
		if err != nil {
			fmt.Printf("发送失败到 %s: %v\n", targetPeerID, err)
			s.Close()                    // 发送失败通常意味着流已损坏，关闭它
			streams.Delete(targetPeerID) // 从活跃流中移除
			// 如果当前聊天目标流失败，重置 currentChatTarget
			currentChatTargetMtx.Lock()
			if currentChatTarget == targetPeerID {
				currentChatTarget = ""
				fmt.Println("已退出一对一聊天模式。")
			}
			currentChatTargetMtx.Unlock()
			return
		}

		conn := s.Conn()
		isRelayed := false
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
}

// broadcastMessage 向所有活跃的聊天流发送消息 (除中继节点外)
func broadcastMessage(h host.Host, msg string) {
	if msg == "" {
		return
	}
	fmt.Println("正在向所有活跃聊天连接广播消息...")
	sentCount := 0
	streams.Range(func(key, value interface{}) bool {
		targetPeerID, ok := key.(peer.ID)
		if !ok {
			return true
		}
		s, ok := value.(network.Stream)
		if !ok {
			return true
		}

		// 不向自己或中继节点广播
		if targetPeerID == h.ID() || targetPeerID == globalRelayPeerID {
			return true
		}

		_, err := s.Write([]byte(msg + "\n"))
		if err != nil {
			fmt.Printf("广播消息失败到 %s: %v\n", targetPeerID, err)
			s.Close()                    // 关闭失败的流
			streams.Delete(targetPeerID) // 从活跃流中移除
		} else {
			sentCount++
			conn := s.Conn()
			isRelayed := false
			remoteAddr := conn.RemoteMultiaddr()
			if remoteAddr != nil && strings.Contains(remoteAddr.String(), "/p2p-circuit") {
				isRelayed = true
			}
			if isRelayed {
				fmt.Printf("  广播消息已通过中继发送到 %s\n", targetPeerID)
			} else {
				fmt.Printf("  广播消息已直接发送到 %s\n", targetPeerID)
			}
		}
		return true
	})
	if sentCount == 0 {
		fmt.Println("没有活跃的聊天连接可以广播消息。")
	}
}

// connectToPeerByID 是新的函数，用于通过 Peer ID 连接到其他节点，并发送连接请求
func connectToPeerByID(h host.Host, targetPeerID peer.ID) {
	peerInfo := h.Peerstore().PeerInfo(targetPeerID)
	if len(peerInfo.Addrs) == 0 {
		// 如果Peerstore中没有已知地址，尝试通过中继连接。
		// libp2p的EnableRelay和NATPortMap会帮助找到路径。
		fmt.Printf("Peerstore 中没有找到 %s 的已知地址。尝试通过中继建立连接...\n", targetPeerID)
		// 不直接返回，而是让 h.Connect 尝试查找路径
	}

	if targetPeerID == h.ID() {
		fmt.Println("不能连接到自己。")
		return
	}
	if targetPeerID == globalRelayPeerID {
		fmt.Println("不能直接连接到中继节点以进行聊天。中继节点仅用于转发。")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 首先，尝试建立或确认与目标节点的底层连接
	if h.Network().Connectedness(targetPeerID) != network.Connected {
		fmt.Printf("尝试建立底层连接到 Peer ID: %s...\n", targetPeerID)
		// 注意：这里的 connect 可能会尝试通过中继
		if err := h.Connect(ctx, peerInfo); err != nil {
			fmt.Printf("无法建立底层连接到 %s: %v\n", targetPeerID, err)
			return
		}
		fmt.Printf("已建立底层连接到 %s\n", targetPeerID)
	}

	fmt.Printf("尝试发送连接请求到 Peer ID: %s\n", targetPeerID)

	reqStream, err := h.NewStream(ctx, targetPeerID, connReqProtocol)
	if err != nil {
		fmt.Printf("无法创建连接请求流到 %s: %v\n", targetPeerID, err)
		return
	}
	defer reqStream.Close() // 确保请求流关闭

	_, err = reqStream.Write([]byte("request_connection\n"))
	if err != nil {
		fmt.Printf("发送连接请求失败到 %s: %v\n", targetPeerID, err)
		return
	}

	reader := bufio.NewReader(reqStream)
	response, err := reader.ReadString('\n')
	if err != nil {
		fmt.Printf("读取连接请求响应失败: %v\n", err)
		return
	}
	response = strings.TrimSpace(response)

	if response == "accepted" {
		fmt.Printf("连接请求已被 %s 接受。尝试建立聊天流...\n", targetPeerID)
		// 建立聊天流（调用 sendMessage，传入空消息以仅建立流）
		sendMessage(h, targetPeerID, "")
		fmt.Printf("已与 %s 建立聊天连接。\n", targetPeerID)
		currentChatTargetMtx.Lock()
		currentChatTarget = targetPeerID // 设置当前聊天目标
		currentChatTargetMtx.Unlock()
		fmt.Printf("您现在处于与 %s 的一对一聊天模式，直接输入消息即可发送。\n", targetPeerID)
		fmt.Println("输入 /exit_chat 退出此模式。")
	} else {
		fmt.Printf("连接请求被 %s 拒绝。\n", targetPeerID)
	}
}

// disconnectFromPeer 断开与指定Peer ID的聊天流
func disconnectFromPeer(targetPeerID peer.ID) {
	if sVal, ok := streams.Load(targetPeerID); ok {
		if s, sOk := sVal.(network.Stream); sOk {
			s.Close()                    // 关闭流
			streams.Delete(targetPeerID) // 从map中移除
			fmt.Printf("已断开与 %s 的聊天连接。\n", targetPeerID)
			currentChatTargetMtx.Lock()
			if currentChatTarget == targetPeerID {
				currentChatTarget = "" // 如果断开的是当前聊天目标，则重置
				fmt.Println("已退出一对一聊天模式。")
			}
			currentChatTargetMtx.Unlock()
			return
		}
	}
	fmt.Printf("与 %s 没有活跃的聊天连接。\n", targetPeerID)
}

// genDebugKey 生成用于调试的固定私钥
func genDebugKey(port int) crypto.PrivKey {
	priv, _, err := crypto.GenerateKeyPairWithReader(
		crypto.RSA,
		2048,
		mrand.New(mrand.NewSource(int64(port))), // 使用端口作为随机种子，确保固定ID
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
		// 在打印连接状态前加锁，避免与用户输入提示冲突
		inputMtx.Lock()
		fmt.Println("\n--- 当前连接状态 ---")
		if globalRelayPeerID != "" {
			fmt.Printf("已连接中继节点: %s\n", globalRelayPeerID)
		} else {
			fmt.Println("未连接到中继节点。")
		}

		fmt.Println("本地已建立聊天连接的活跃节点:")
		foundChatPeers := false
		streams.Range(func(key, value interface{}) bool {
			pID, ok := key.(peer.ID)
			if !ok {
				return true
			}
			_, ok = value.(network.Stream)
			if !ok {
				return true
			}

			// 不列出自己或中继节点
			if pID != h.ID() && pID != globalRelayPeerID {
				conns := h.Network().ConnsToPeer(pID)
				isRelayed := false
				if len(conns) > 0 {
					// 检查是否有任何连接是通过中继建立的
					for _, conn := range conns {
						if conn.RemoteMultiaddr() != nil && strings.Contains(conn.RemoteMultiaddr().String(), "/p2p-circuit") {
							isRelayed = true
							break
						}
					}
				}
				if isRelayed {
					fmt.Printf("    %s (通过中继)\n", pID)
				} else {
					fmt.Printf("    %s (直接连接)\n", pID)
				}
				foundChatPeers = true
			}
			return true
		})
		if !foundChatPeers {
			fmt.Println("    目前没有连接到其他对等节点。")
		}
		// 显示当前一对一聊天目标（如果有）
		currentChatTargetMtx.Lock()
		if currentChatTarget != "" {
			fmt.Printf("当前一对一聊天目标: %s\n", currentChatTarget)
		}
		currentChatTargetMtx.Unlock()

		fmt.Println("--------------------")
		fmt.Print("> ")
		inputMtx.Unlock() // 解锁
	}
}
