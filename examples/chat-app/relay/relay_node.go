package relay

import (
	"context"
	"fmt"
	"log"
	"sync"

	"chat-app/common"

	"encoding/json" // 添加 json 导入

	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/host/autorelay" // 导入 autorelay 包
	// 导入 relay 包，用于 EnableRelay
	// 添加 io 导入
	// 添加 strings 导入
)

// RelayNode 表示一个中继节点
type RelayNode struct {
	Host           host.Host
	DHT            *dht.IpfsDHT
	ConnectedPeers map[peer.ID]struct{} // 连接到中继的对等体
	PeersMux       sync.Mutex           // 保护 ConnectedPeers map 的互斥锁
}

// NewRelayNode 创建一个新的中继节点。
func NewRelayNode(port int) (*RelayNode, error) {
	// 创建libp2p主机，并启用中继功能
	// 关键修改在这里：为 EnableRelay 提供 AutoRelay 选项
	h, err := libp2p.New(
		libp2p.ListenAddrStrings(fmt.Sprintf("/ip4/0.0.0.0/tcp/%d", port)),
		libp2p.EnableRelay(), // 启用中继功能
		// 当启用中继时，AutoRelay 也会被默认启用，需要给它一个发现源
		// 对于中继节点，通常它不需要去发现其他中继，所以可以提供一个空的静态列表
		libp2p.EnableAutoRelay(autorelay.WithStaticRelays(nil)), // <--- 修复点
		// 或者您可以选择不启用 AutoRelay，只启用 Relay 功能
		// libp2p.DisableAutoRelay(), // 如果你只想让这个节点是中继，而不自动连接其他中继
	)
	if err != nil {
		log.Printf("[ERROR] 创建中继主机失败，端口 %d: %v", port, err)
		return nil, fmt.Errorf("创建中继主机失败: %w", err)
	}

	log.Printf("[INFO] 中继节点已启动，ID: %s", h.ID().String())
	for _, addr := range h.Addrs() {
		log.Printf("[INFO] 正在监听: %s/p2p/%s", addr, h.ID().String())
	}

	// 初始化 DHT
	kademliaDHT, err := dht.New(context.Background(), h, dht.Mode(dht.ModeServer)) // 中继通常是服务器模式
	if err != nil {
		log.Printf("[ERROR] 创建DHT失败: %v", err)
		h.Close()
		return nil, fmt.Errorf("创建DHT失败: %w", err)
	}

	// 引导DHT
	if err = kademliaDHT.Bootstrap(context.Background()); err != nil {
		log.Printf("[ERROR] DHT引导失败: %v", err)
		kademliaDHT.Close()
		h.Close()
		return nil, fmt.Errorf("DHT引导失败: %w", err)
	}
	log.Println("[INFO] DHT引导完成。")

	rn := &RelayNode{
		Host:           h,
		DHT:            kademliaDHT,
		ConnectedPeers: make(map[peer.ID]struct{}),
	}

	// 设置处理中继请求的流处理器 (通常 libp2p 内部已经处理了 /p2p/circuit/v2 协议)
	// 这里您可以添加一个自定义协议处理器，例如用于列出连接的对等体
	h.SetStreamHandler(common.RelayProtocolID, rn.handleRelayRequest)

	// 监听连接事件，维护连接的对等体列表
	h.Network().Notify(&network.NotifyBundle{
		ConnectedF: func(_ network.Network, conn network.Conn) {
			rn.PeersMux.Lock()
			rn.ConnectedPeers[conn.RemotePeer()] = struct{}{}
			rn.PeersMux.Unlock()
			log.Printf("[INFO] 中继：新的连接来自: %s", conn.RemotePeer().String())
		},
		DisconnectedF: func(_ network.Network, conn network.Conn) {
			rn.PeersMux.Lock()
			delete(rn.ConnectedPeers, conn.RemotePeer())
			rn.PeersMux.Unlock()
			log.Printf("[INFO] 中继：已断开与 %s 的连接", conn.RemotePeer().String())
		},
	})

	return rn, nil
}

// handleRelayRequest 处理来自其他节点的请求，例如请求连接到中继的对等体列表
func (rn *RelayNode) handleRelayRequest(s network.Stream) {
	log.Printf("[INFO] 收到来自 %s 的中继请求", s.ID())
	defer func() {
		if err := s.Close(); err != nil {
			log.Printf("[WARN] 关闭中继请求流失败: %v", err)
		}
	}()

	var msg common.ChatMessage
	if err := json.NewDecoder(s).Decode(&msg); err != nil {
		log.Printf("[ERROR] 解码中继请求消息失败: %v", err)
		return
	}

	if msg.Content == "/listpeers" {
		rn.PeersMux.Lock()
		defer rn.PeersMux.Unlock()

		var peerIDs []string
		for pID := range rn.ConnectedPeers {
			peerIDs = append(peerIDs, pID.String())
		}
		log.Printf("[INFO] 返回 %d 个连接的对等体到 %s", len(peerIDs), msg.From)

		if err := json.NewEncoder(s).Encode(peerIDs); err != nil {
			log.Printf("[ERROR] 发送对等体列表给 %s 失败: %v", msg.From, err)
		}
	} else if msg.To != "" && msg.Content != "" {
		// 这是一个转发消息的请求
		targetPeerID, err := peer.Decode(msg.To)
		if err != nil {
			log.Printf("[ERROR] 无法解析目标Peer ID '%s': %v", msg.To, err)
			// 发送错误响应
			errMsg := common.ChatMessage{
				From:    rn.Host.ID().String(),
				To:      msg.From,
				Content: fmt.Sprintf("Error: Invalid target Peer ID '%s'", msg.To),
			}
			if err := json.NewEncoder(s).Encode(errMsg); err != nil {
				log.Printf("[ERROR] 发送错误响应失败: %v", err)
			}
			return
		}

		// 尝试直接连接到目标，然后转发消息
		// 注意：这里的转发逻辑是简化的。libp2p 的 circuit v2 协议会自动处理流量转发，
		// 除非您需要实现更复杂的应用层转发逻辑。
		// 对于一个纯粹的中继，通常只需要 EnableRelay()，它会自动处理转发。
		// 这里是针对 PeerNode 的 SendMessage 中 "回退到中继转发" 的处理。
		log.Printf("[INFO] 中继 %s 正在尝试转发消息从 %s 到 %s: %s", rn.Host.ID().String(), msg.From, msg.To, msg.Content)

		// 检查目标是否已连接到中继
		rn.PeersMux.Lock()
		_, isTargetConnected := rn.ConnectedPeers[targetPeerID]
		rn.PeersMux.Unlock()

		if !isTargetConnected {
			log.Printf("[WARN] 目标对等体 %s 未连接到中继，无法转发消息。", targetPeerID.String())
			errMsg := common.ChatMessage{
				From:    rn.Host.ID().String(),
				To:      msg.From,
				Content: fmt.Sprintf("Error: Target peer %s not connected to relay.", targetPeerID.String()),
			}
			if err := json.NewEncoder(s).Encode(errMsg); err != nil {
				log.Printf("[ERROR] 发送目标未连接错误响应失败: %v", err)
			}
			return
		}

		// 打开一个流到目标对等体
		forwardStream, err := rn.Host.NewStream(context.Background(), targetPeerID, common.ChatProtocolID)
		if err != nil {
			log.Printf("[ERROR] 中继打开流到目标 %s 失败: %v", targetPeerID.String(), err)
			errMsg := common.ChatMessage{
				From:    rn.Host.ID().String(),
				To:      msg.From,
				Content: fmt.Sprintf("Error: Failed to open stream to target %s: %v", targetPeerID.String(), err),
			}
			if err := json.NewEncoder(s).Encode(errMsg); err != nil {
				log.Printf("[ERROR] 发送流打开失败错误响应失败: %v", err)
			}
			return
		}
		defer func() {
			if err := forwardStream.Close(); err != nil {
				log.Printf("[WARN] 关闭转发流失败: %v", err)
			}
		}()

		// 修改消息的 Relayed 字段
		msg.Relayed = true
		// 发送消息到目标对等体
		if err := json.NewEncoder(forwardStream).Encode(msg); err != nil {
			log.Printf("[ERROR] 中继转发消息到目标 %s 失败: %v", targetPeerID.String(), err)
			errMsg := common.ChatMessage{
				From:    rn.Host.ID().String(),
				To:      msg.From,
				Content: fmt.Sprintf("Error: Failed to forward message to target %s: %v", targetPeerID.String(), err),
			}
			if err := json.NewEncoder(s).Encode(errMsg); err != nil {
				log.Printf("[ERROR] 发送转发失败错误响应失败: %v", err)
			}
			return
		}
		log.Printf("[INFO] 成功转发消息从 %s 到 %s", msg.From, msg.To)

		// 发送确认给请求者
		ackMsg := common.ChatMessage{
			From:    rn.Host.ID().String(),
			To:      msg.From,
			Content: fmt.Sprintf("Message relayed successfully to %s", targetPeerID.String()),
		}
		if err := json.NewEncoder(s).Encode(ackMsg); err != nil {
			log.Printf("[ERROR] 发送转发确认给 %s 失败: %v", msg.From, err)
		}
	} else {
		log.Printf("[WARN] 收到未知或格式错误的中继请求: %+v", msg)
		errMsg := common.ChatMessage{
			From:    rn.Host.ID().String(),
			To:      msg.From,
			Content: "Error: Unknown or malformed relay request.",
		}
		if err := json.NewEncoder(s).Encode(errMsg); err != nil {
			log.Printf("[ERROR] 发送未知请求错误响应失败: %v", err)
		}
	}
}
