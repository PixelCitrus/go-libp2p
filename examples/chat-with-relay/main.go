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
)

type discoveryNotifee struct {
	host host.Host
}

func (n *discoveryNotifee) HandlePeerFound(pi peer.AddrInfo) {
	fmt.Printf("发现新节点: %s\n", pi.ID)
	if err := n.host.Connect(context.Background(), pi); err != nil {
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
		libp2p.EnableRelay(),
		libp2p.NATPortMap(),
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
	opts = append(opts, libp2p.DisableRelay())
	h, err := libp2p.New(opts...)
	if err != nil {
		log.Fatal(err)
	}

	_, err = relayv2.New(h)
	if err != nil {
		log.Fatal("无法启动中继服务:", err)
	}

	fmt.Printf("中继节点已启动:\nID: %s\n地址:\n", h.ID())
	for _, addr := range h.Addrs() {
		fmt.Printf("  %s/p2p/%s\n", addr, h.ID())
	}

	select {}
}

func createPeerNode(ctx context.Context, opts []libp2p.Option) {
	h, err := libp2p.New(opts...)
	if err != nil {
		log.Fatal(err)
	}

	setupDiscovery(h)
	connectToRelay(h)
	setupChatProtocol(h)

	fmt.Printf("普通节点已启动:\nID: %s\n监听地址:\n", h.ID())
	for _, addr := range h.Addrs() {
		fmt.Println(" ", addr)
	}

	go readConsoleInput(h)
	select {}
}

func setupDiscovery(h host.Host) {
	discoveryService := discovery.NewMdnsService(h, *rendezvous, &discoveryNotifee{host: h})
	if err := discoveryService.Start(); err != nil {
		log.Fatal("启动节点发现失败:", err)
	}
}

func connectToRelay(h host.Host) {
	if *relayAddr == "" {
		return
	}

	maddr, err := ma.NewMultiaddr(*relayAddr)
	if err != nil {
		log.Fatal("解析中继地址失败:", err)
	}

	relayInfo, err := peer.AddrInfoFromP2pAddr(maddr)
	if err != nil {
		log.Fatal("获取中继信息失败:", err)
	}

	h.Peerstore().AddAddrs(relayInfo.ID, relayInfo.Addrs, peerstore.PermanentAddrTTL)
	if err := h.Connect(context.Background(), *relayInfo); err != nil {
		log.Fatal("连接中继失败:", err)
	}
}

func setupChatProtocol(h host.Host) {
	h.SetStreamHandler(chatProtocol, func(s network.Stream) {
		defer s.Close()
		fmt.Printf("\n新连接来自: %s\n> ", s.Conn().RemotePeer())
		go readStream(s)
	})
}

func readStream(s network.Stream) {
	r := bufio.NewReader(s)
	for {
		msg, err := r.ReadString('\n')
		if err != nil {
			fmt.Printf("\n连接 %s 已关闭\n> ", s.Conn().RemotePeer())
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
			connectToPeer(h, strings.TrimSpace(msg[8:]))
			continue
		}

		conns := h.Network().Conns()
		for _, c := range conns {
			if c.RemotePeer() == h.ID() {
				continue
			}
			s, err := h.NewStream(context.Background(), c.RemotePeer(), chatProtocol)
			if err != nil {
				continue
			}
			defer s.Close()
			s.Write([]byte(msg + "\n"))
		}
	}
}

func connectToPeer(h host.Host, addr string) {
	maddr, err := ma.NewMultiaddr(addr)
	if err != nil {
		fmt.Println("地址解析错误:", err)
		return
	}

	peerInfo, err := peer.AddrInfoFromP2pAddr(maddr)
	if err != nil {
		fmt.Println("节点信息错误:", err)
		return
	}

	h.Peerstore().AddAddrs(peerInfo.ID, peerInfo.Addrs, peerstore.PermanentAddrTTL)
	if err := h.Connect(context.Background(), *peerInfo); err != nil {
		fmt.Println("连接失败:", err)
	}
}

func genDebugKey(port int) crypto.PrivKey {
	priv, _, err := crypto.GenerateKeyPairWithReader(
		crypto.RSA,
		2048,
		mrand.New(mrand.NewSource(int64(port))),
	)
	if err != nil {
		panic(err)
	}
	return priv
}
