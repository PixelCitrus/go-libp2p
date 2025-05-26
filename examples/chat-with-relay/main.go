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
	// relayPeerID holds the PeerID of the relay node, if connected.
	// This is set once successfully connected to the relay.
	globalRelayPeerID peer.ID
)

type discoveryNotifee struct {
	host host.Host
}

func (n *discoveryNotifee) HandlePeerFound(pi peer.AddrInfo) {
	// Add relay address to discovered peers if specified
	if *relayAddr != "" {
		_, err := ma.NewMultiaddr(*relayAddr)
		if err == nil {
			// Extract just the address part, not the /p2p/peerid part
			// The AddrInfoFromP2pAddr already adds the PeerID to the PeerInfo
			// So, we just need the multiaddress to add to the peer's known addresses.
			// However, in this context, pi.Addrs are the addresses the discovered peer
			// advertises. If we want to connect to a peer *via* a relay, we add the relay
			// address to the *relay info* when connecting to it, not to other discovered peers.
			// This part is generally incorrect for adding a relay to other peers' AddrInfo,
			// as the relay connection is usually established directly with the relay.
			// The original intention here might have been to add the relay as an alternate
			// route, but libp2p's relay mechanism handles that automatically if the relay
			// is in the peerstore and reachable.
		}
	}

	fmt.Printf("发现新节点: %s\n", pi.ID)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := n.host.Connect(ctx, pi); err != nil {
		fmt.Printf("连接失败: %v\n", err)
	}
}

func main() {
	flag.Parse()
	ctx := context.Background()

	opts := []libp2p.Option{
		libp2p.ListenAddrStrings(
			fmt.Sprintf("/ip4/0.0.0.0/tcp/%d", *port),
			fmt.Sprintf("/ip6/::/tcp/%d", *port)),
		libp2p.EnableRelay(),      // Enables relay dialing and listening
		libp2p.NATPortMap(),       // Enables NAT port mapping
		libp2p.EnableNATService(), // Enables NAT service for others to discover
		libp2p.Ping(true),         // Enables ping service
	}

	if *debug {
		opts = append(opts, libp2p.Identity(genDebugKey(*port)))
	}

	if *mode == relayMode {
		createRelayNode(ctx, opts)
	} else {
		createPeerNode(ctx, opts)
	}
}

func createRelayNode(ctx context.Context, opts []libp2p.Option) {
	// For a relay node, we disable the client-side relay capability
	// as it is providing the service.
	opts = append(opts, libp2p.DisableRelay())
	h, err := libp2p.New(opts...)
	if err != nil {
		log.Fatal(err)
	}

	// Start the relay service
	_, err = relayv2.New(h)
	if err != nil {
		log.Fatal("无法启动中继服务:", err)
	}

	fmt.Printf("中继节点已启动:\nID: %s\n地址:\n", h.ID())
	for _, addr := range h.Addrs() {
		fmt.Printf("  %s/p2p/%s\n", addr, h.ID())
	}

	// Keep the relay node running indefinitely
	select {}
}

func createPeerNode(ctx context.Context, opts []libp2p.Option) {
	h, err := libp2p.New(opts...)
	if err != nil {
		log.Fatal(err)
	}

	setupDiscovery(h)
	connectToRelay(h) // This will also set globalRelayPeerID if successful
	setupChatProtocol(h)

	fmt.Printf("普通节点已启动:\nID: %s\n监听地址:\n", h.ID())
	for _, addr := range h.Addrs() {
		fmt.Println(" ", addr)
	}

	go monitorConnections(h)
	go readConsoleInput(h)
	select {}
}

func setupDiscovery(h host.Host) {
	// mDNS discovery for local peer discovery
	discoveryService := discovery.NewMdnsService(h, *rendezvous, &discoveryNotifee{host: h})
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

	// Correctly parse the Peer ID from the multiaddr, if it contains one.
	// AddrInfoFromP2pAddr is designed for multiaddrs like /ip4/127.0.0.1/tcp/4001/p2p/Qm...
	relayInfo, err := peer.AddrInfoFromP2pAddr(maddr)
	if err != nil {
		log.Fatalf("获取中继信息失败: %v", err)
	}

	log.Printf("解析出的中继 Peer ID: %s, 地址: %v", relayInfo.ID, relayInfo.Addrs)

	// Add the relay's addresses to our peerstore, so libp2p knows how to reach it.
	h.Peerstore().AddAddrs(relayInfo.ID, relayInfo.Addrs, peerstore.PermanentAddrTTL)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	fmt.Printf("尝试连接中继节点: %s\n", relayInfo.ID)
	if err := h.Connect(ctx, *relayInfo); err != nil {
		log.Fatalf("连接中继失败: %v", err)
	}

	// Store the relay's Peer ID globally once connected successfully
	globalRelayPeerID = relayInfo.ID
	fmt.Printf("成功连接到中继节点: %s\n", globalRelayPeerID)
}

func setupChatProtocol(h host.Host) {
	// Set up a stream handler for the chat protocol
	h.SetStreamHandler(chatProtocol, func(s network.Stream) {
		fmt.Printf("\n新连接来自: %s\n> ", s.Conn().RemotePeer())
		// Store the new stream for potential future use (though not strictly necessary
		// for reading, as readStream handles the current stream lifecycle).
		// This ensures we can reply on this stream if needed.
		streams.Store(s.Conn().RemotePeer(), s)
		go readStream(s)
	})
	fmt.Println("聊天协议处理程序已设置。")
}

func readStream(s network.Stream) {
	// A stream should be closed by the party that initiates the closing.
	// If the remote peer closes their side of the stream, ReadString will return an EOF error.
	// We only close our side when we are done sending, or if an unrecoverable error occurs.
	// defer s.Close() // Removed to keep the stream open for sending messages

	r := bufio.NewReader(s)
	for {
		msg, err := r.ReadString('\n')
		if err != nil {
			fmt.Printf("\n连接 %s 已关闭 (%v)\n> ", s.Conn().RemotePeer(), err)
			streams.Delete(s.Conn().RemotePeer()) // Remove stream from map on disconnect
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

func broadcastMessage(h host.Host, msg string) {
	peers := h.Network().Peers()

	// Filter out the relay node and self from the peers list for chat
	var chatPeers []peer.ID
	for _, p := range peers {
		// Exclude self and the designated relay peer
		if p != h.ID() && (globalRelayPeerID == "" || p != globalRelayPeerID) {
			chatPeers = append(chatPeers, p)
		}
	}

	if len(chatPeers) == 0 {
		fmt.Println("没有其他可聊天的节点。")
		return
	}

	for _, p := range chatPeers {
		// Attempt to load an existing stream
		sVal, ok := streams.Load(p)
		var s network.Stream
		var err error

		if ok {
			s, ok = sVal.(network.Stream)
			if !ok {
				// Type assertion failed, something is wrong with the stored value.
				// Delete and recreate.
				streams.Delete(p)
				ok = false // Force stream recreation
			}
		}

		if !ok || s == nil { // If no existing stream or type assertion failed
			// Create a new stream to the peer
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			s, err = h.NewStream(ctx, p, chatProtocol)
			cancel() // Release resources associated with the context

			if err != nil {
				fmt.Printf("无法创建流到 %s: %v\n", p, err)
				// Do not store invalid stream, and continue to next peer
				continue
			}
			streams.Store(p, s) // Store the new stream
			go readStream(s)    // Start reading from this new stream
			fmt.Printf("已建立新流到 %s\n", p)
		}

		// Send the message
		_, err = s.Write([]byte(msg + "\n"))
		if err != nil {
			fmt.Printf("发送失败到 %s: %v\n", p, err)
			streams.Delete(p) // Remove the stream if writing fails
			// Consider re-establishing stream on next broadcast if desirable,
			// or have readStream handle this. For now, it's removed.
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
