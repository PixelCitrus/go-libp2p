package peer

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"chat-app/common"

	drouting "github.com/libp2p/go-libp2p/p2p/discovery/routing"
	dutil "github.com/libp2p/go-libp2p/p2p/discovery/util"

	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	_ "github.com/libp2p/go-libp2p/core/routing"
	ma "github.com/multiformats/go-multiaddr"
)

// PeerNode 表示一个普通的聊天节点。
type PeerNode struct {
	Host           host.Host
	RelayAddr      ma.Multiaddr
	RelayPeerID    peer.ID
	DHT            *dht.IpfsDHT
	Discovery      *drouting.RoutingDiscovery
	ConnectedPeers map[peer.ID]struct{}    // 直接连接的对等体
	PeersMux       sync.Mutex              // 保护ConnectedPeers map的互斥锁
	MsgQueue       chan common.ChatMessage // 用于接收传入消息（当前未完全使用，消息直接由handler打印）
}

// NewPeerNode 创建一个新的聊天节点。
func NewPeerNode(port int, relayAddrStr string) (*PeerNode, error) {
	h, err := common.CreateHost(port)
	if err != nil {
		log.Printf("[ERROR] 创建对等体主机失败: %v", err)
		return nil, fmt.Errorf("创建对等体主机失败: %w", err)
	}

	log.Printf("[INFO] 对等体节点已启动，ID: %s", h.ID().String())
	for _, addr := range h.Addrs() {
		log.Printf("[INFO] 正在监听: %s/p2p/%s", addr, h.ID().String())
	}

	// 设置DHT用于发现
	kademliaDHT, err := dht.New(context.Background(), h, dht.Mode(dht.ModeClient))
	if err != nil {
		log.Printf("[ERROR] 创建DHT失败: %v", err)
		h.Close()
		return nil, fmt.Errorf("创建DHT失败: %w", err)
	}

	if err = kademliaDHT.Bootstrap(context.Background()); err != nil {
		log.Printf("[ERROR] DHT引导失败: %v", err)
		kademliaDHT.Close()
		h.Close()
		return nil, fmt.Errorf("DHT引导失败: %w", err)
	}
	log.Println("[INFO] DHT引导完成。")

	routingDiscovery := drouting.NewRoutingDiscovery(kademliaDHT)

	// 解析中继地址
	var relayAddr ma.Multiaddr
	var relayPeerID peer.ID
	if relayAddrStr != "" {
		relayAddr, err = ma.NewMultiaddr(relayAddrStr)
		if err != nil {
			log.Printf("[ERROR] 无效的中继地址 '%s': %v", relayAddrStr, err)
			kademliaDHT.Close()
			h.Close()
			return nil, fmt.Errorf("无效的中继地址: %w", err)
		}
		relayInfo, err := peer.AddrInfoFromP2pAddr(relayAddr)
		if err != nil {
			log.Printf("[ERROR] 无法从中继地址 '%s' 解析PeerInfo: %v", relayAddrStr, err)
			kademliaDHT.Close()
			h.Close()
			return nil, fmt.Errorf("无效的中继地址信息: %w", err)
		}
		relayPeerID = relayInfo.ID
		log.Printf("[INFO] 配置中继节点: %s", relayPeerID.String()) // 修改点：relayPeerID.Pretty() -> relayPeerID.String()
	}

	pn := &PeerNode{
		Host:           h,
		RelayAddr:      relayAddr,
		RelayPeerID:    relayPeerID,
		DHT:            kademliaDHT,
		Discovery:      routingDiscovery,
		ConnectedPeers: make(map[peer.ID]struct{}),
		MsgQueue:       make(chan common.ChatMessage, common.MessageBufferSize),
	}

	// 为传入的聊天消息设置流处理器
	h.SetStreamHandler(common.ChatProtocolID, pn.handleChatMessage)

	// 监控对等体节点的连接
	h.Network().Notify(&network.NotifyBundle{
		ConnectedF: func(_ network.Network, conn network.Conn) {
			pn.PeersMux.Lock()
			pn.ConnectedPeers[conn.RemotePeer()] = struct{}{}
			pn.PeersMux.Unlock()
			log.Printf("[INFO] 对等体: 新的直接连接来自: %s", conn.RemotePeer().String()) // 修改点：conn.RemotePeer().Pretty() -> conn.RemotePeer().String()
		},
		DisconnectedF: func(_ network.Network, conn network.Conn) {
			pn.PeersMux.Lock()
			delete(pn.ConnectedPeers, conn.RemotePeer())
			pn.PeersMux.Unlock()
			log.Printf("[INFO] 对等体: 已断开与 %s 的连接", conn.RemotePeer().String()) // 修改点：conn.RemotePeer().Pretty() -> conn.RemotePeer().String()
		},
	})

	return pn, nil
}

// ConnectToRelay 尝试连接到中继节点。
func (pn *PeerNode) ConnectToRelay(ctx context.Context) error {
	if pn.RelayAddr == nil {
		log.Println("[INFO] 未配置中继地址。")
		return nil
	}

	log.Printf("[INFO] 尝试连接到中继节点: %s", pn.RelayPeerID.String()) // 修改点：pn.RelayPeerID.Pretty() -> pn.RelayPeerID.String()
	// 使用`WithAddrs`明确告诉libp2p中继的多地址。
	relayInfo := peer.AddrInfo{
		ID:    pn.RelayPeerID,
		Addrs: []ma.Multiaddr{pn.RelayAddr},
	}

	connectCtx, cancel := context.WithTimeout(ctx, common.RelayConnectTimeout)
	defer cancel()

	err := pn.Host.Connect(connectCtx, relayInfo)
	if err != nil {
		log.Printf("[ERROR] 连接中继 %s 失败: %v", pn.RelayPeerID.String(), err) // 修改点：pn.RelayPeerID.Pretty() -> pn.RelayPeerID.String()
		return fmt.Errorf("连接中继 %s 失败: %w", pn.RelayPeerID.String(), err)  // 修改点：pn.RelayPeerID.Pretty() -> pn.RelayPeerID.String()
	}
	log.Printf("[INFO] 成功连接到中继: %s", pn.RelayPeerID.String()) // 修改点：pn.RelayPeerID.Pretty() -> pn.RelayPeerID.String()
	return nil
}

// DiscoverPeers 使用DHT发现其他节点。
func (pn *PeerNode) DiscoverPeers(ctx context.Context) {
	log.Println("[INFO] 启动对等体发现...")
	ticker := time.NewTicker(common.DiscoveryInterval)
	defer ticker.Stop()

	// 使用ChatProtocolID宣布自己，以发现其他聊天对等体
	dutil.Advertise(ctx, pn.Discovery, string(common.ChatProtocolID))
	log.Printf("[INFO] 已在DHT中宣布自己为协议 %s 的提供者。", common.ChatProtocolID)

	for {
		select {
		case <-ctx.Done():
			log.Println("[INFO] 对等体发现已停止。")
			return
		case <-ticker.C:
			log.Println("[INFO] 正在搜索新对等体...")
			peerChan, err := pn.Discovery.FindPeers(ctx, string(common.ChatProtocolID))
			if err != nil {
				log.Printf("[ERROR] 查找对等体失败: %v", err)
				continue
			}

			foundNew := false
			for p := range peerChan {
				if p.ID == pn.Host.ID() || p.ID == pn.RelayPeerID {
					continue // 不要尝试连接自身或中继节点作为普通对等体
				}

				// 仅在尚未直接连接时才尝试连接
				pn.PeersMux.Lock()
				_, connected := pn.ConnectedPeers[p.ID]
				pn.PeersMux.Unlock()

				if !connected {
					log.Printf("[INFO] 发现对等体: %s。尝试连接...", p.ID.String()) // 修改点：p.ID.Pretty() -> p.ID.String()
					foundNew = true
					go func(p peer.AddrInfo) {
						// 如果连接到中继，则将中继地址添加到对等体的地址信息中
						// 这将告诉libp2p尝试通过中继拨号
						if pn.RelayPeerID != "" {
							relayCircuitAddr, err := ma.NewMultiaddr(fmt.Sprintf("/p2p/%s/p2p-circuit/p2p/%s", pn.RelayPeerID.String(), p.ID.String())) // 修改点：pn.RelayPeerID.Pretty() -> pn.RelayPeerID.String(), p.ID.Pretty() -> p.ID.String()
							if err != nil {
								log.Printf("[WARN] 为对等体 %s 创建中继电路地址失败: %v", p.ID.String(), err) // 修改点：p.ID.Pretty() -> p.ID.String()
							} else {
								p.Addrs = append(p.Addrs, relayCircuitAddr)
								log.Printf("[DEBUG] 为对等体 %s 添加中继电路地址: %s", p.ID.String(), relayCircuitAddr.String()) // 修改点：p.ID.Pretty() -> p.ID.String()
							}
						}
						connectCtx, connectCancel := context.WithTimeout(context.Background(), 15*time.Second) // 增加连接超时
						defer connectCancel()

						err := pn.Host.Connect(connectCtx, p)
						if err != nil {
							log.Printf("[WARN] 连接到对等体 %s 失败: %v", p.ID.String(), err) // 修改点：p.ID.Pretty() -> p.ID.String()
							return
						}
						log.Printf("[INFO] 成功连接到对等体: %s", p.ID.String()) // 修改点：p.ID().Pretty() -> p.ID().String()
					}(p)
				}
			}
			if !foundNew {
				log.Println("[INFO] 未发现新对等体。")
			}
		}
	}
}

// ListConnectedPeers 列出所有直接连接到此节点的对等体。
func (pn *PeerNode) ListConnectedPeers() {
	pn.PeersMux.Lock()
	defer pn.PeersMux.Unlock()

	if len(pn.ConnectedPeers) == 0 {
		fmt.Println("没有直接连接的对等体。")
		return
	}

	fmt.Println("直接连接的对等体:")
	for p := range pn.ConnectedPeers {
		fmt.Printf("- %s\n", p.String()) // 修改点：p.Pretty() -> p.String()
	}
}

// GetPeersFromRelay 请求连接到中继的对等体列表。
func (pn *PeerNode) GetPeersFromRelay(ctx context.Context) ([]peer.ID, error) {
	if pn.RelayPeerID == "" {
		return nil, fmt.Errorf("未配置中继")
	}

	// 检查是否直接连接到中继
	pn.PeersMux.Lock()
	_, connectedToRelay := pn.ConnectedPeers[pn.RelayPeerID]
	pn.PeersMux.Unlock()

	if !connectedToRelay {
		log.Printf("[WARN] 未直接连接到中继节点 %s，无法请求对等体列表。", pn.RelayPeerID.String()) // 修改点：pn.RelayPeerID.Pretty() -> pn.RelayPeerID.String()
		return nil, fmt.Errorf("未直接连接到中继节点 %s", pn.RelayPeerID.String())       // 修改点：pn.RelayPeerID.Pretty() -> pn.RelayPeerID.String()
	}

	// 打开一个流到中继，使用特定协议来列出对等体
	streamCtx, streamCancel := context.WithTimeout(ctx, 5*time.Second) // 设置流创建超时
	defer streamCancel()
	stream, err := pn.Host.NewStream(streamCtx, pn.RelayPeerID, common.RelayProtocolID)
	if err != nil {
		log.Printf("[ERROR] 无法打开到中继 %s 的流以获取对等体列表: %v", pn.RelayPeerID.String(), err) // 修改点：pn.RelayPeerID.Pretty() -> pn.RelayPeerID.String()
		return nil, fmt.Errorf("无法打开到中继的流以获取对等体列表: %w", err)
	}
	defer func() {
		if err := stream.Close(); err != nil {
			log.Printf("[WARN] 关闭获取中继对等体列表流失败: %v", err)
		}
	}()

	// 发送一个特殊消息来请求对等体列表
	req := common.ChatMessage{
		From:    pn.Host.ID().String(), // 修改点：pn.Host.ID().Pretty() -> pn.Host.ID().String()
		To:      "",                    // 没有特定接收方，是对中继本身的请求
		Content: "/listpeers",
	}
	if err := json.NewEncoder(stream).Encode(req); err != nil {
		log.Printf("[ERROR] 发送对等体列表请求到中继失败: %v", err)
		return nil, fmt.Errorf("发送对等体列表请求到中继失败: %w", err)
	}
	log.Printf("[INFO] 已向中继 %s 发送 /listpeers 请求。", pn.RelayPeerID.String()) // 修改点：pn.RelayPeerID.Pretty() -> pn.RelayPeerID.String()

	// 设置读取超时以避免阻塞
	if err := stream.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		log.Printf("[ERROR] 设置读取中继对等体列表超时失败: %v", err)
		return nil, fmt.Errorf("设置读取中继对等体列表超时失败: %w", err)
	}

	var peerList []string
	if err := json.NewDecoder(stream).Decode(&peerList); err != nil {
		if err == io.EOF {
			log.Printf("[WARN] 从中继读取对等体列表时遇到EOF，可能中继未响应。")
		} else {
			log.Printf("[ERROR] 从中继 %s 解码对等体列表失败: %v", pn.RelayPeerID.String(), err) // 修改点：pn.RelayPeerID.Pretty() -> pn.RelayPeerID.String()
		}
		return nil, fmt.Errorf("从中继解码对等体列表失败: %w", err)
	}
	// 重置读取超时
	if err := stream.SetReadDeadline(time.Time{}); err != nil {
		log.Printf("[WARN] 重置读取中继对等体列表超时失败: %v", err)
	}

	var pids []peer.ID
	for _, ps := range peerList {
		pid, err := peer.Decode(ps)
		if err != nil {
			log.Printf("[WARN] 从中继获取的无效Peer ID: %s, 错误: %v", ps, err)
			continue
		}
		pids = append(pids, pid)
	}
	log.Printf("[INFO] 成功从中继 %s 获取到 %d 个对等体。", pn.RelayPeerID.String(), len(pids)) // 修改点：pn.RelayPeerID.Pretty() -> pn.RelayPeerID.String()
	return pids, nil
}

// SendMessage 向目标对等体发送消息。
func (pn *PeerNode) SendMessage(ctx context.Context, targetID peer.ID, message string) error {
	msg := common.ChatMessage{
		From:    pn.Host.ID().String(), // 修改点：pn.Host.ID().Pretty() -> pn.Host.ID().String()
		To:      targetID.String(),     // 修改点：targetID.Pretty() -> targetID.String()
		Content: message,
		Relayed: false,
	}

	// 1. 首先尝试直接连接发送
	pn.PeersMux.Lock()
	_, directlyConnected := pn.ConnectedPeers[targetID]
	pn.PeersMux.Unlock()

	if directlyConnected {
		log.Printf("[INFO] 尝试直接发送消息到 %s...", targetID.String())            // 修改点：targetID.Pretty() -> targetID.String()
		streamCtx, streamCancel := context.WithTimeout(ctx, 5*time.Second) // 设置流创建超时
		defer streamCancel()
		stream, err := pn.Host.NewStream(streamCtx, targetID, common.ChatProtocolID)
		if err == nil {
			defer func() {
				if err := stream.Close(); err != nil {
					log.Printf("[WARN] 关闭直接发送流失败: %v", err)
				}
			}()
			if err := json.NewEncoder(stream).Encode(msg); err != nil {
				log.Printf("[ERROR] 直接发送消息到 %s 失败: %v", targetID.String(), err) // 修改点：targetID.Pretty() -> targetID.String()
				return fmt.Errorf("直接发送消息到 %s 失败: %w", targetID.String(), err)  // 修改点：targetID.Pretty() -> targetID.String()
			}
			log.Printf("[INFO] 直接消息已发送到 %s。", targetID.String()) // 修改点：targetID.Pretty() -> targetID.String()
			return nil
		}
		log.Printf("[WARN] 直接连接到 %s 失败，尝试通过中继发送: %v", targetID.String(), err) // 修改点：targetID.Pretty() -> targetID.String()
	} else {
		log.Printf("[INFO] 未直接连接到 %s，尝试通过中继发送。", targetID.String()) // 修改点：targetID.Pretty() -> targetID.String()
	}

	// 2. 如果直接发送失败或未连接，则尝试通过中继
	if pn.RelayPeerID == "" {
		log.Printf("[ERROR] 无法发送消息，没有直接连接且未配置中继。")
		return fmt.Errorf("无法发送消息，没有直接连接且未配置中继")
	}

	// 检查是否连接到中继
	pn.PeersMux.Lock()
	_, connectedToRelay := pn.ConnectedPeers[pn.RelayPeerID]
	pn.PeersMux.Unlock()

	if !connectedToRelay {
		log.Printf("[ERROR] 无法发送消息，未连接到中继节点 %s。", pn.RelayPeerID.String()) // 修改点：pn.RelayPeerID.Pretty() -> pn.RelayPeerID.String()
		return fmt.Errorf("无法发送消息，未连接到中继节点")
	}

	// 尝试通过libp2p的电路中继功能发送
	// 这将告诉libp2p尝试通过中继连接到目标
	_, err := ma.NewMultiaddr(fmt.Sprintf("/p2p/%s/p2p-circuit/p2p/%s", pn.RelayPeerID.String(), targetID.String())) // 修改点：pn.RelayPeerID.Pretty() -> pn.RelayPeerID.String(), targetID.Pretty() -> targetID.String()
	if err != nil {
		log.Printf("[ERROR] 创建中继多地址失败: %v", err)
		return fmt.Errorf("创建中继多地址失败: %w", err)
	}

	// relayTargetInfo := peer.AddrInfo{
	// 	ID:    targetID,
	// 	Addrs: []ma.Multiaddr{relayTargetAddr},
	// }

	log.Printf("[INFO] 尝试通过中继电路向 %s 发送消息...", targetID.String())        // 修改点：targetID.Pretty() -> targetID.String()
	streamCtx, streamCancel := context.WithTimeout(ctx, 10*time.Second) // 增加中继流创建超时
	defer streamCancel()
	stream, err := pn.Host.NewStream(streamCtx, targetID, common.ChatProtocolID)
	if err != nil {
		// 如果通过电路无法打开直接流，这意味着中继连接未能促进电路。
		// 回退到明确发送到中继进行转发。
		log.Printf("[WARN] 无法通过中继电路打开到 %s 的直接流: %v。回退到中继转发。", targetID.String(), err) // 修改点：targetID.Pretty() -> targetID.String()

		// 打开一个流到中继本身
		relayStreamCtx, relayStreamCancel := context.WithTimeout(ctx, 5*time.Second)
		defer relayStreamCancel()
		relayStream, err := pn.Host.NewStream(relayStreamCtx, pn.RelayPeerID, common.RelayProtocolID)
		if err != nil {
			log.Printf("[ERROR] 无法打开中继流到中继节点 %s 进行转发: %v", pn.RelayPeerID.String(), err) // 修改点：pn.RelayPeerID.Pretty() -> pn.RelayPeerID.String()
			return fmt.Errorf("无法打开中继流到中继节点进行转发: %w", err)
		}
		defer func() {
			if err := relayStream.Close(); err != nil {
				log.Printf("[WARN] 关闭显式中继流失败: %v", err)
			}
		}()

		// 将消息发送到中继进行转发
		if err := json.NewEncoder(relayStream).Encode(msg); err != nil {
			log.Printf("[ERROR] 发送消息到中继进行转发失败: %v", err)
			return fmt.Errorf("发送消息到中继进行转发失败: %w", err)
		}

		// 设置读取超时以等待中继的确认
		if err := relayStream.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			log.Printf("[ERROR] 设置读取中继确认超时失败: %v", err)
			return fmt.Errorf("设置读取中继确认超时失败: %w", err)
		}

		// 等待中继的确认
		var ack common.ChatMessage
		if err := json.NewDecoder(relayStream).Decode(&ack); err != nil {
			if err == io.EOF {
				log.Printf("[WARN] 从中继读取确认时遇到EOF，可能中继未响应。")
			} else {
				log.Printf("[ERROR] 从中继获取确认失败: %v", err)
			}
			return fmt.Errorf("从中继获取确认失败: %w", err)
		}
		// 重置读取超时
		if err := relayStream.SetReadDeadline(time.Time{}); err != nil {
			log.Printf("[WARN] 重置读取中继确认超时失败: %v", err)
		}

		if strings.HasPrefix(ack.Content, "Error:") {
			log.Printf("[ERROR] 中继报告错误: %s", ack.Content)
			return fmt.Errorf("中继报告错误: %s", ack.Content)
		}
		log.Printf("[INFO] 消息已通过显式转发成功中继到 %s: %s", targetID.String(), ack.Content) // 修改点：targetID.Pretty() -> targetID.String()
		return nil

	}
	defer func() {
		if err := stream.Close(); err != nil {
			log.Printf("[WARN] 关闭通过中继电路发送的流失败: %v", err)
		}
	}()

	// 如果通过电路成功打开了流，直接发送消息
	if err := json.NewEncoder(stream).Encode(msg); err != nil {
		log.Printf("[ERROR] 通过中继电路发送消息失败: %v", err)
		return fmt.Errorf("通过中继电路发送消息失败: %w", err)
	}
	log.Printf("[INFO] 消息已通过中继电路发送到 %s。", targetID.String()) // 修改点：targetID.Pretty() -> targetID.String()
	return nil
}

// StartConsoleInput 开始监听控制台命令。
func (pn *PeerNode) StartConsoleInput(ctx context.Context) {
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("> ")
		input, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				log.Println("[INFO] 控制台输入 EOF，正在退出。")
				return
			}
			log.Printf("[ERROR] 读取控制台输入失败: %v", err)
			continue
		}
		input = strings.TrimSpace(input)

		parts := strings.Fields(input)
		if len(parts) == 0 {
			continue
		}

		cmd := parts[0]
		switch cmd {
		case "/send":
			if len(parts) < 3 {
				fmt.Println("用法: /send <peerID> <消息>")
				continue
			}
			targetIDStr := parts[1]
			messageContent := strings.Join(parts[2:], " ")

			targetID, err := peer.Decode(targetIDStr)
			if err != nil {
				fmt.Printf("无效的Peer ID: %v\n", err)
				log.Printf("[WARN] 用户输入无效的Peer ID: %s, 错误: %v", targetIDStr, err)
				continue
			}

			if err := pn.SendMessage(ctx, targetID, messageContent); err != nil {
				fmt.Printf("发送消息失败: %v\n", err)
				log.Printf("[ERROR] 发送消息失败: %v", err)
			}
		case "/list_direct":
			pn.ListConnectedPeers()
		case "/list_relay_peers":
			peers, err := pn.GetPeersFromRelay(ctx)
			if err != nil {
				fmt.Printf("从中继获取对等体列表失败: %v\n", err)
				log.Printf("[ERROR] 从中继获取对等体列表失败: %v", err)
				continue
			}
			if len(peers) == 0 {
				fmt.Println("没有对等体连接到中继。")
				continue
			}
			fmt.Println("连接到中继的对等体:")
			for _, p := range peers {
				fmt.Printf("- %s\n", p.String()) // 修改点：p.Pretty() -> p.String()
			}
		case "/exit":
			fmt.Println("正在退出聊天应用。")
			log.Println("[INFO] 收到 /exit 命令，正在退出。")
			return
		default:
			fmt.Println("未知命令。可用命令: /send <peerID> <消息>, /list_direct, /list_relay_peers, /exit")
			log.Printf("[WARN] 收到未知命令: %s", cmd)
		}
	}
}
