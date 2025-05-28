package main

import (
	"bufio"           // 用于带缓冲的I/O操作
	"context"         // 用于处理上下文，控制取消、超时等
	"flag"            // 用于解析命令行参数
	"fmt"             // 用于格式化I/O
	"io"              // 提供基本的I/O接口
	"log"             // 用于日志记录
	mrand "math/rand" // 用于生成伪随机数 (math/rand)
	"os"              // 提供操作系统功能接口
	"strings"         // 用于字符串操作
	"sync"            // 提供同步原语，如互斥锁
	"time"            // 用于时间操作

	"github.com/libp2p/go-libp2p"                                           // libp2p核心库
	"github.com/libp2p/go-libp2p/core/crypto"                               // libp2p加密相关
	"github.com/libp2p/go-libp2p/core/host"                                 // libp2p主机接口
	"github.com/libp2p/go-libp2p/core/network"                              // libp2p网络核心接口
	"github.com/libp2p/go-libp2p/core/peer"                                 // libp2p节点表示
	"github.com/libp2p/go-libp2p/core/peerstore"                            // libp2p节点存储
	discovery "github.com/libp2p/go-libp2p/p2p/discovery/mdns"              // libp2p mDNS发现服务
	relayclient "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/client" // libp2p Circuit Relay v2 客户端实现
	relayservice "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay" // libp2p Circuit Relay v2 中继服务实现
	ma "github.com/multiformats/go-multiaddr"                               // 多重地址格式处理
)

const (
	chatProtocol      = "/p2p-chat/1.0.0"       // 聊天协议ID
	relayInfoProtocol = "/p2p-relay-info/1.0.0" // 中继提供活跃节点列表的协议ID
	connReqProtocol   = "/p2p-conn-req/1.0.0"   // 连接请求协议ID
	relayMode         = "relay"                 // 节点模式：中继
	peerMode          = "peer"                  // 节点模式：普通对等节点
)

var (
	mode       = flag.String("mode", peerMode, "节点模式：relay (中继) / peer (对等节点)")                // 命令行参数：节点模式
	port       = flag.Int("port", 6666, "监听端口")                                                // 命令行参数：监听端口
	relayAddr  = flag.String("relay", "", "中继节点地址 (例如 /ip4/127.0.0.1/tcp/6666/p2p/QmRelayID)") // 命令行参数：中继节点的多重地址
	rendezvous = flag.String("rendezvous", "chat-with-relay", "节点发现标识串，用于mDNS服务")              // 命令行参数：mDNS发现服务使用的集合点字符串
	debug      = flag.Bool("debug", false, "调试模式，生成固定的节点ID (基于端口)")                            // 命令行参数：是否启用调试模式以生成固定ID
	// forceRelay 用于强制所有peer-to-peer通信通过中继，会禁用mDNS并声明节点仅通过中继可达
	forceRelay = flag.Bool("forceRelay", false, "强制所有通信通过中继 (将禁用mDNS并声明节点仅通过中继可达)")

	// streams 用于维护与其他节点建立的持久化网络流，用于发送消息
	streams sync.Map // key: peer.ID, value: network.Stream
	// globalRelayPeerID 保存当前连接的中继节点的PeerID
	globalRelayPeerID peer.ID

	// activePeers 存储连接到本中继节点的活跃Peer ID及其地址信息，仅中继节点使用
	activePeers     = make(map[peer.ID]peer.AddrInfo) // key: peer.ID, value: peer.AddrInfo
	activePeersLock sync.Mutex                        // 用于保护activePeers的并发访问

	// stdinReader 是一个共享的bufio.Reader，用于读取标准输入
	stdinReader *bufio.Reader
	// inputMtx 用于同步对stdinReader的访问，避免多个goroutine同时读取标准输入导致冲突
	inputMtx sync.Mutex

	// currentChatTarget 存储当前正在进行一对一聊天的目标Peer ID
	currentChatTarget    peer.ID    // 如果为空字符串，则表示未处于一对一聊天模式
	currentChatTargetMtx sync.Mutex // 用于保护currentChatTarget的并发访问
)

// discoveryNotifee 用于处理通过mDNS发现的对等节点的回调
type discoveryNotifee struct {
	host host.Host // 当前节点的主机实例
}

// HandlePeerFound 是当mDNS发现新节点时被调用的方法
func (n *discoveryNotifee) HandlePeerFound(pi peer.AddrInfo) {
	// 只有当 forceRelay 为 false 时才处理mDNS发现的节点并尝试连接
	// 注意：setupDiscovery 函数本身也受 !*forceRelay 条件的控制
	if !*forceRelay {
		fmt.Printf("mDNS 发现新节点: %s\n", pi.ID)

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second) // 设置连接超时
		defer cancel()

		// 尝试连接通过mDNS发现的节点
		if err := n.host.Connect(ctx, pi); err != nil {
			fmt.Printf("连接 mDNS 发现的节点 %s 失败: %v\n", pi.ID, err)
		} else {
			fmt.Printf("成功连接到 mDNS 发现的节点: %s\n", pi.ID)
		}
	}
}

// main 函数是程序的入口点
func main() {
	flag.Parse()                // 解析命令行参数
	ctx := context.Background() // 创建一个空的根上下文

	// 初始化共享的 stdinReader
	stdinReader = bufio.NewReader(os.Stdin)

	// libp2p节点的基础配置选项
	opts := []libp2p.Option{
		libp2p.ListenAddrStrings( // 设置监听地址
			fmt.Sprintf("/ip4/0.0.0.0/tcp/%d", *port), // IPv4监听
			fmt.Sprintf("/ip6/::/tcp/%d", *port),      // IPv6监听
		),
		libp2p.NATPortMap(),       // 尝试使用UPnP或NAT-PMP进行端口映射
		libp2p.EnableNATService(), // 启用NAT服务，允许其他节点通过NAT发现本节点
		libp2p.Ping(true),         // 启用ping协议，用于检测节点活性
	}

	if *debug { // 如果启用了调试模式
		// 使用基于端口生成的固定私钥，从而获得固定的PeerID，方便调试
		opts = append(opts, libp2p.Identity(genDebugKey(*port)))
	}

	if *mode == relayMode { // 如果当前节点被配置为中继模式
		createRelayNode(ctx, opts) // 创建并运行中继节点
	} else { // 否则，当前节点为普通对等节点模式
		// 普通对等节点默认启用中继客户端功能，以便能够通过中继进行连接
		opts = append(opts, libp2p.EnableRelay()) // 允许此节点使用中继进行出站连接

		// MODIFICATION: 如果forceRelay为true，添加ForceReachabilityRelayed选项
		if *forceRelay {
			opts = append(opts, libp2p.ForceReachabilityPrivate()) // 声明此节点主要通过中继可达
			fmt.Println("forceRelay 模式已激活：节点将声明为仅通过中继可达。mDNS 本地发现也已禁用。")
		}
		createPeerNode(ctx, opts) // 创建并运行普通对等节点
	}
}

// --- 中继 (Relay) 节点功能 ---

// createRelayNode 创建并启动一个中继节点
func createRelayNode(ctx context.Context, opts []libp2p.Option) {
	h, err := libp2p.New(opts...) // 创建libp2p主机实例
	if err != nil {
		log.Fatalf("创建中继节点主机失败: %v", err)
	}

	// 启动Circuit Relay v2中继服务
	// relayv2.New 在给定的主机上实例化并启动中继服务。
	// 注意：libp2p.EnableRelay(circuitv2.OptHop) 是另一种在创建主机时启用中继服务的方式，
	// 但这里我们选择在主机创建后显式初始化中继服务。
	_, err = relayservice.New(h)
	if err != nil {
		log.Fatalf("无法启动中继服务: %v", err)
	}

	// 监听网络连接事件，用于更新连接到此中继的活跃节点列表
	h.Network().Notify(&network.NotifyBundle{
		ConnectedF: func(_ network.Network, conn network.Conn) { // 当有新连接建立时
			peerID := conn.RemotePeer()
			if peerID == h.ID() { // 忽略连接到自己的情况
				return
			}
			log.Printf("中继节点：新连接来自 %s", peerID)
			activePeersLock.Lock() // 加锁保护activePeers
			// 只要有节点连接到中继，就将其信息（ID和已知地址）添加到活跃列表
			activePeers[peerID] = h.Peerstore().PeerInfo(peerID)
			activePeersLock.Unlock() // 解锁
		},
		DisconnectedF: func(_ network.Network, conn network.Conn) { // 当有连接断开时
			peerID := conn.RemotePeer()
			log.Printf("中继节点：连接断开 %s", peerID)
			activePeersLock.Lock()      // 加锁保护activePeers
			delete(activePeers, peerID) // 从活跃列表中移除
			activePeersLock.Unlock()    // 解锁
		},
	})

	// 设置中继信息协议 (/p2p-relay-info/1.0.0) 的流处理程序
	// 当其他节点向此中继请求活跃节点列表时，此处理程序会被调用
	h.SetStreamHandler(relayInfoProtocol, func(s network.Stream) {
		fmt.Printf("\n收到来自 %s 的活跃节点列表请求\n> ", s.Conn().RemotePeer())

		activePeersLock.Lock() // 加锁，读取activePeers
		var sb strings.Builder
		sb.WriteString("活跃节点列表 (已连接到本中继的其他对等节点):\n") // 响应头信息

		var sortedPeerIDs []peer.ID // 用于排序（可选）
		for id := range activePeers {
			sortedPeerIDs = append(sortedPeerIDs, id)
		}
		// 可以取消注释以进行排序: import "sort"; sort.Slice(sortedPeerIDs, func(i, j int) bool { return sortedPeerIDs[i].String() < sortedPeerIDs[j].String() })

		foundPeers := false
		for _, id := range sortedPeerIDs {
			if id != h.ID() { // 不列出中继节点自己
				// peerInfo := activePeers[id] // 可以获取更详细的地址信息，但这里只列出ID
				sb.WriteString(fmt.Sprintf("    %s\n", id))
				foundPeers = true
			}
		}
		if !foundPeers {
			sb.WriteString("    目前没有其他对等节点连接到本中继。\n")
		}
		activePeersLock.Unlock() // 解锁
		sb.WriteString("\n")     // 添加末尾换行符，帮助客户端完整读取

		_, err := s.Write([]byte(sb.String())) // 将列表发送给请求方
		if err != nil {
			fmt.Printf("发送活跃节点列表给 %s 失败: %v\n", s.Conn().RemotePeer(), err)
		}
		s.Close() // 数据发送完毕，关闭流
	})

	fmt.Printf("中继节点已启动:\nID: %s\n地址 (其他节点可以使用这些地址连接到本中继):\n", h.ID())
	for _, addr := range h.Addrs() {
		// 对于中继节点，关键是其 /p2p/<peerID> 地址。/p2p-circuit 不是它自己监听的地址。
		fmt.Printf("    %s/p2p/%s\n", addr, h.ID())
	}

	select {} // 阻塞主goroutine，使中继节点持续运行，直到程序被外部终止
}

// --- 普通对等 (Peer) 节点功能 ---

// createPeerNode 创建并启动一个普通P2P节点
func createPeerNode(ctx context.Context, opts []libp2p.Option) {
	h, err := libp2p.New(opts...) // 使用配置选项创建libp2p主机实例
	if err != nil {
		log.Fatalf("创建对等节点主机失败: %v", err)
	}

	// 根据 forceRelay 开关决定是否设置 mDNS 本地发现服务
	if !*forceRelay { // 如果不强制所有流量走中继
		setupDiscovery(h) // 则启动mDNS服务进行本地节点发现
	} else {
		// 如果forceRelay为true，在main函数中已经打印了相关提示信息
	}

	connectToRelay(h)                 // 连接到命令行指定的中继节点 (如果指定了)
	setupChatProtocol(h)              // 设置聊天协议 (/p2p-chat/1.0.0) 的流处理程序
	setupConnectionRequestProtocol(h) // 设置连接请求协议 (/p2p-conn-req/1.0.0) 的流处理程序

	fmt.Printf("普通对等节点已启动:\nID: %s\n监听地址:\n", h.ID())
	for _, addr := range h.Addrs() { // 打印本节点正在监听的地址
		fmt.Println(" ", addr)
	}

	go monitorConnections(h) // 启动一个goroutine定期监控并打印连接状态
	go readConsoleInput(h)   // 启动一个goroutine处理用户在控制台的输入
	select {}                // 阻塞主goroutine，使节点持续运行
}

// setupDiscovery 设置并启动mDNS本地节点发现服务
func setupDiscovery(h host.Host) {
	// NewMdnsService 创建一个新的mDNS服务实例
	// h: 当前节点的主机
	// *rendezvous: 集合点字符串，只有使用相同字符串的节点才能互相发现
	// &discoveryNotifee{host: h}: 处理发现事件的回调通知器
	discoveryService := discovery.NewMdnsService(h, *rendezvous, &discoveryNotifee{host: h})
	if err := discoveryService.Start(); err != nil { // 启动mDNS服务
		log.Fatalf("启动mDNS节点发现服务失败: %v", err)
	}
	fmt.Println("mDNS 本地发现服务已启动。")
}

// connectToRelay 连接到命令行参数中指定的中继节点
func connectToRelay(h host.Host) {
	if *relayAddr == "" { // 如果没有在命令行中通过 -relay 参数指定中继地址
		// 对于一个依赖中继进行通信的P2P应用，这通常是需要的。
		// 但我们允许节点在没有配置中继的情况下启动，它可能只进行本地mDNS通信或等待其他方式的连接。
		log.Println("提示：未指定中继节点地址 (-relay 参数)。可能无法连接到需要通过中继发现或访问的节点。")
		return // 允许继续运行
	}

	maddr, err := ma.NewMultiaddr(*relayAddr) // 解析中继节点的多重地址字符串
	if err != nil {
		log.Fatalf("解析中继地址 '%s' 失败: %v", *relayAddr, err)
	}

	relayInfo, err := peer.AddrInfoFromP2pAddr(maddr) // 从多重地址中提取PeerID和地址信息
	if err != nil {
		log.Fatalf("从多重地址 '%s' 获取中继节点信息失败: %v", maddr, err)
	}

	log.Printf("解析出的中继节点 Peer ID: %s, 地址: %v", relayInfo.ID, relayInfo.Addrs)

	// 将中继节点的地址信息添加到本地Peerstore中，并设置永久有效（直到程序退出）
	// 这样libp2p在后续需要连接此PeerID时就知道它的地址
	h.Peerstore().AddAddrs(relayInfo.ID, relayInfo.Addrs, peerstore.PermanentAddrTTL)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second) // 设置连接中继的超时
	defer cancel()

	fmt.Printf("尝试连接中继节点: %s\n", relayInfo.ID)
	// 调用 h.Connect 尝试与中继节点建立底层网络连接
	if err := h.Connect(ctx, *relayInfo); err != nil {
		log.Fatalf("连接中继节点 %s 失败: %v", relayInfo.ID, err)
	}

	globalRelayPeerID = relayInfo.ID // 保存成功连接的中继节点的PeerID
	fmt.Printf("成功连接到中继节点: %s\n", globalRelayPeerID)

	// 在成功连接到中继后，尝试向中继节点预留一个槽位
	fmt.Printf("尝试在中继节点 %s 上预留槽位...\n", globalRelayPeerID)
	reserveCtx, reserveCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer reserveCancel()

	// 调用relayv2.Reserve向中继节点发送预留请求
	_, err = relayclient.Reserve(reserveCtx, h, *relayInfo)
	if err != nil {
		log.Printf("在中继节点 %s 上预留槽位失败: %v\n", globalRelayPeerID, err)
		// 预留失败不应阻止程序继续运行，但会影响其他节点通过此中继连接到本节点
	} else {
		fmt.Printf("成功在中继节点 %s 上预留槽位。\n", globalRelayPeerID)
	}
}

// setupChatProtocol 设置聊天协议 (/p2p-chat/1.0.0) 的流处理程序
// 当其他节点使用此协议向本节点发起新流时，此函数会被调用
func setupChatProtocol(h host.Host) {
	h.SetStreamHandler(chatProtocol, func(s network.Stream) { // s 是新建立的流
		remotePeer := s.Conn().RemotePeer() // 获取流对端节点的PeerID
		fmt.Printf("\n收到来自 %s 的新聊天连接\n", remotePeer)
		streams.Store(remotePeer, s) // 将新建立的流存储到全局map中，以便后续发送消息
		go readStream(s)             // 为每个新流启动一个goroutine专门负责读取该流上的数据
		fmt.Print("> ")              // 重新打印命令提示符
	})
	fmt.Println("聊天协议处理程序已设置。")
}

// setupConnectionRequestProtocol 设置连接请求协议 (/p2p-conn-req/1.0.0) 的流处理程序
// 当其他节点想要与本节点建立正式的聊天会话前，会先通过此协议发送一个连接请求
func setupConnectionRequestProtocol(h host.Host) {
	h.SetStreamHandler(connReqProtocol, func(s network.Stream) {
		remotePeer := s.Conn().RemotePeer()
		fmt.Printf("\n收到来自 %s 的连接请求。是否接受？(yes/no)\n", remotePeer)

		// 确保在读取用户输入时，只有当前处理程序能访问 stdinReader
		inputMtx.Lock()                               // 获取标准输入锁
		fmt.Print("> ")                               // 打印提示符，等待用户输入
		response, err := stdinReader.ReadString('\n') // 读取用户输入 (yes/no)
		inputMtx.Unlock()                             // 释放标准输入锁

		if err != nil {
			fmt.Printf("读取对 %s 连接请求的响应失败: %v\n", remotePeer, err)
			s.Reset() // 重置流以示出错
			return
		}
		response = strings.TrimSpace(strings.ToLower(response)) // 清理并转换为小写

		if response == "yes" { // 如果用户同意连接
			_, err := s.Write([]byte("accepted\n")) // 向请求方发送 "accepted"
			if err != nil {
				fmt.Printf("发送接受响应给 %s 失败: %v\n", remotePeer, err)
				s.Reset()
				return
			}
			fmt.Printf("已接受来自 %s 的连接请求。等待对方建立聊天流...\n", remotePeer)
			// 注意：这里只是接受了连接请求，对方接下来应该会用chatProtocol建立聊天流
			// 接受方不需要在此处主动建立chatProtocol流，而是等待请求方建立。
			// 流 s 在这里只是用于传输接受/拒绝的响应，之后可以关闭。
			s.Close() // 关闭连接请求流
		} else { // 如果用户拒绝连接
			_, err := s.Write([]byte("rejected\n")) // 向请求方发送 "rejected"
			if err != nil {
				fmt.Printf("发送拒绝响应给 %s 失败: %v\n", remotePeer, err)
			}
			s.Close() // 关闭连接请求流 (无论成功或失败都关闭)
			fmt.Printf("已拒绝来自 %s 的连接请求。\n", remotePeer)
		}
		fmt.Print("> ") // 重新打印命令提示符
	})
	fmt.Println("连接请求协议处理程序已设置。")
}

// readStream 从指定的网络流中循环读取数据，并打印到控制台
func readStream(s network.Stream) {
	r := bufio.NewReader(s)             // 为流创建一个带缓冲的读取器
	remotePeer := s.Conn().RemotePeer() // 获取对端节点的PeerID，用于日志

	defer func() { // 确保在函数退出时执行清理操作
		s.Close()                  // 关闭流
		streams.Delete(remotePeer) // 从全局流map中移除此流
		currentChatTargetMtx.Lock()
		if currentChatTarget == remotePeer { // 如果断开的是当前一对一聊天对象
			currentChatTarget = "" // 重置一对一聊天对象
			fmt.Printf("\n与 %s 的连接已断开，已退出一对一聊天模式。\n", remotePeer)
		} else {
			fmt.Printf("\n与 %s 的连接已断开。\n", remotePeer)
		}
		currentChatTargetMtx.Unlock()
		fmt.Print("> ") // 重新打印命令提示符
	}()

	for { // 无限循环，持续读取
		msg, err := r.ReadString('\n') // 从流中读取一行数据 (以换行符为界)
		if err != nil {                // 如果读取出错
			if err == io.EOF { // 如果是正常的流结束信号
				// log.Printf("连接 %s 已由对方关闭。\n", remotePeer) // 可以用更安静的方式处理
			} else {
				log.Printf("从 %s 读取流错误: %v\n", remotePeer, err)
			}
			return // 退出读取循环和goroutine
		}

		conn := s.Conn()
		isRelayed := false                   // 标记此连接是否通过中继
		remoteAddr := conn.RemoteMultiaddr() // 获取连接的远程多重地址
		// 如果远程地址中包含 "/p2p-circuit"，则说明这是一个通过中继的连接
		if remoteAddr != nil && strings.Contains(remoteAddr.String(), "/p2p-circuit") {
			isRelayed = true
		}

		// MODIFICATION: 移除了 !*forceRelay 条件，直接根据 isRelayed 判断打印内容
		// 打印接收到的消息，并标明来源和连接类型（直接或中继）
		if isRelayed {
			// 如果是通过中继收到的消息
			fmt.Printf("\n[通过中继 | 来自 %s]: %s", remotePeer, msg) // msg本身已包含换行符
		} else {
			// 如果是直接连接收到的消息
			fmt.Printf("\n[直接连接 | 来自 %s]: %s", remotePeer, msg) // msg本身已包含换行符
		}
		fmt.Print("> ") // 重新打印命令提示符
	}
}

// readConsoleInput 读取用户在控制台的输入，并根据输入内容执行相应操作 (发送消息或执行命令)
func readConsoleInput(h host.Host) {
	for {
		inputMtx.Lock()                            // 获取标准输入锁
		fmt.Print("> ")                            // 打印命令提示符
		input, err := stdinReader.ReadString('\n') // 读取用户输入的一行
		inputMtx.Unlock()                          // 释放标准输入锁

		if err != nil {
			log.Printf("读取控制台输入错误: %v。请重试。\n", err)
			// 短暂休眠以避免在持续错误时CPU占用过高
			time.Sleep(100 * time.Millisecond)
			continue
		}
		input = strings.TrimSpace(input) // 去除输入内容两端的空白字符

		if len(input) == 0 { // 如果输入为空，则忽略
			continue
		}

		// 检查输入是否为命令 (以 "/" 开头)
		if strings.HasPrefix(input, "/") {
			parts := strings.Fields(input) // 按空格分割命令和参数
			command := parts[0]            // 第一个部分为命令名
			switch command {
			case "/list_active": // 列出通过中继发现的活跃节点
				currentChatTargetMtx.Lock()
				if currentChatTarget != "" { // 如果当前在一对一聊天模式
					fmt.Println("请先使用 /exit_chat 退出当前的一对一聊天模式，然后再使用此命令。")
					currentChatTargetMtx.Unlock()
					continue
				}
				currentChatTargetMtx.Unlock()
				requestActivePeersFromRelay(h) // 向中继请求活跃节点列表
			case "/send": // 发送消息给指定PeerID的节点 (旧版，通常在非一对一模式下使用)
				currentChatTargetMtx.Lock()
				if currentChatTarget != "" { // 如果当前在一对一聊天模式
					fmt.Println("您已处于与 " + currentChatTarget.String() + " 的一对一聊天模式，请直接输入消息发送，或使用 /exit_chat 退出。")
					currentChatTargetMtx.Unlock()
					continue
				}
				currentChatTargetMtx.Unlock()

				if len(parts) < 3 { // 参数不足
					fmt.Println("用法: /send <目标PeerID> <消息内容>")
					continue
				}
				targetPeerIDStr := parts[1]             // 第二个部分为目标PeerID字符串
				message := strings.Join(parts[2:], " ") // 其余部分为消息内容

				targetPeerID, err := peer.Decode(targetPeerIDStr) // 解码PeerID字符串
				if err != nil {
					fmt.Printf("无效的目标Peer ID '%s': %v\n", targetPeerIDStr, err)
					continue
				}
				sendMessage(h, targetPeerID, message) // 发送消息
			case "/connect": // 请求与指定PeerID的节点建立连接并进入一对一聊天
				currentChatTargetMtx.Lock()
				if currentChatTarget != "" { // 如果已在一对一聊天
					fmt.Println("您已处于与 " + currentChatTarget.String() + " 的一对一聊天模式。如需连接其他节点，请先使用 /exit_chat 退出。")
					currentChatTargetMtx.Unlock()
					continue
				}
				currentChatTargetMtx.Unlock()

				if len(parts) < 2 { // 参数不足
					fmt.Println("用法: /connect <目标PeerID>")
					continue
				}
				targetPeerIDStr := parts[1]
				targetPeerID, err := peer.Decode(targetPeerIDStr)
				if err != nil {
					fmt.Printf("无效的目标Peer ID '%s': %v\n", targetPeerIDStr, err)
					continue
				}
				connectToPeerByID(h, targetPeerID) // 尝试连接并建立聊天
			case "/disconnect": // 断开与指定PeerID的聊天连接
				if len(parts) < 2 { // 参数不足
					fmt.Println("用法: /disconnect <目标PeerID>")
					continue
				}
				targetPeerIDStr := parts[1]
				targetPeerID, err := peer.Decode(targetPeerIDStr)
				if err != nil {
					fmt.Printf("无效的目标Peer ID '%s': %v\n", targetPeerIDStr, err)
					continue
				}
				disconnectFromPeer(targetPeerID) // 执行断开操作
			case "/peers": // 列出本地已建立聊天连接的节点
				currentChatTargetMtx.Lock()
				if currentChatTarget != "" { // 如果在一对一聊天
					fmt.Println("请先使用 /exit_chat 退出当前的一对一聊天模式，然后再查看所有节点。")
					currentChatTargetMtx.Unlock()
					continue
				}
				currentChatTargetMtx.Unlock()

				fmt.Println("本地当前已建立聊天连接的节点:")
				foundChatPeers := false
				streams.Range(func(key, value interface{}) bool { //遍历streams map
					pID, ok := key.(peer.ID)
					stream, okStream := value.(network.Stream)
					if !ok || !okStream || stream.Conn() == nil { // 检查类型和有效性
						return true // 继续遍历
					}

					// 不列出自己或中继节点 (如果已连接中继)
					if pID != h.ID() && (globalRelayPeerID == "" || pID != globalRelayPeerID) {
						isRelayed := false
						if stream.Conn().RemoteMultiaddr() != nil && strings.Contains(stream.Conn().RemoteMultiaddr().String(), "/p2p-circuit") {
							isRelayed = true
						}
						// MODIFICATION: 移除了 !*forceRelay 条件
						if isRelayed {
							fmt.Printf("    %s (聊天流活跃, 通过中继)\n", pID)
						} else {
							fmt.Printf("    %s (聊天流活跃, 直接连接)\n", pID)
						}
						foundChatPeers = true
					}
					return true // 继续遍历
				})
				if !foundChatPeers {
					fmt.Println("    目前没有与其他对等节点建立聊天连接。")
				}
			case "/exit_chat": // 退出当前的一对一聊天模式
				currentChatTargetMtx.Lock()
				if currentChatTarget != "" {
					fmt.Printf("已退出与 %s 的一对一聊天模式。\n", currentChatTarget)
					currentChatTarget = "" // 清空当前聊天对象
				} else {
					fmt.Println("当前不在任何一对一聊天模式中。")
				}
				currentChatTargetMtx.Unlock()
			case "/exit": // 退出程序
				fmt.Println("正在退出程序...")
				if err := h.Close(); err != nil { // 关闭libp2p主机
					log.Printf("关闭主机时发生错误: %v", err)
				}
				os.Exit(0) // 正常退出
			default: // 未知命令
				fmt.Println("未知命令。可用命令: /list_active, /send, /connect, /disconnect, /peers, /exit_chat, /exit")
			}
		} else { // 如果输入不是 "/" 开头的命令，则视为聊天消息
			currentChatTargetMtx.Lock()
			target := currentChatTarget // 获取当前的一对一聊天对象
			currentChatTargetMtx.Unlock()

			if target != "" { // 如果处于一对一聊天模式
				sendMessage(h, target, input) // 将消息发送给当前聊天对象
			} else { // 否则，视为广播消息 (或提示用户选择对象)
				// 当前实现是广播给所有已连接的非中继节点
				broadcastMessage(h, input)
				// 也可以修改为提示用户使用 /send <peerID> <message> 或 /connect <peerID>
				// fmt.Println("提示: 当前不处于一对一聊天模式。如需发送给特定用户，请使用 /send <PeerID> <消息> 或使用 /connect <PeerID> 进入一对一聊天。")
			}
		}
	}
}

// requestActivePeersFromRelay 向已连接的中继节点请求其维护的活跃节点列表
func requestActivePeersFromRelay(h host.Host) {
	if globalRelayPeerID == "" { // 如果未连接到任何中继节点
		fmt.Println("错误：未连接到中继节点，无法获取活跃节点列表。请先使用 -relay 参数连接到一个中继。")
		return
	}

	fmt.Printf("正在向中继节点 %s 请求活跃节点列表...\n", globalRelayPeerID)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second) // 设置请求超时
	defer cancel()

	// 向中继节点发起一个新的流，使用 relayInfoProtocol 协议
	s, err := h.NewStream(ctx, globalRelayPeerID, relayInfoProtocol)
	if err != nil {
		fmt.Printf("无法创建到中继节点 %s 的流 (协议 %s) 以获取列表: %v\n", globalRelayPeerID, relayInfoProtocol, err)
		return
	}

	// 使用 io.ReadAll 读取中继返回的所有数据，确保完整性
	responseBytes, err := io.ReadAll(s)
	s.Close() // 读取完毕后，关闭流

	if err != nil {
		fmt.Printf("从中继节点 %s 读取响应失败: %v\n", globalRelayPeerID, err)
		return
	}
	fmt.Print("从中继获取的活跃节点列表:\n" + string(responseBytes)) // 打印中继返回的列表
}

// sendMessage 向指定的目标节点发送消息
func sendMessage(h host.Host, targetPeerID peer.ID, msg string) {
	if targetPeerID == h.ID() { // 不能给自己发送消息
		fmt.Println("错误：不能给自己发送消息。")
		return
	}
	if globalRelayPeerID != "" && targetPeerID == globalRelayPeerID { // 不能向中继节点本身发送聊天消息
		fmt.Println("错误：不能向中继节点发送聊天消息。中继节点仅用于转发和元数据服务。")
		return
	}

	sVal, streamExists := streams.Load(targetPeerID) // 尝试从map中获取已存在的流
	var s network.Stream
	var err error

	if streamExists { // 如果流已存在
		var okCast bool
		s, okCast = sVal.(network.Stream)
		if !okCast { // 类型断言失败，说明map中存储的不是有效的流
			streams.Delete(targetPeerID) // 移除无效条目
			streamExists = false         // 标记为流不存在，以便后续重新创建
			log.Printf("警告: streams map 中存储了 %s 的无效流类型, 将尝试重建。", targetPeerID)
		} else {
			// 可选：检查流的状态 s.Stat()，但通常直接尝试写入，失败再处理更简单
		}
	}

	// 如果流不存在，或者之前存在的流无效，则尝试创建新流
	if !streamExists || s == nil {
		// 增加连接超时时间，因为可能需要通过中继建立连接，这可能比直接连接慢
		ctxConnect, cancelConnect := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancelConnect()

		fmt.Printf("与 %s 的聊天流不存在或已失效，尝试建立新流...\n", targetPeerID)
		// 使用 chatProtocol 协议向目标节点发起新流
		// 如果设置了 forceRelay 并且主机配置了 ForceReachabilityRelayed，
		// libp2p 的连接逻辑应优先尝试中继路径（如果直接路径不可行或对方也声明为仅中继可达）。
		s, err = h.NewStream(ctxConnect, targetPeerID, chatProtocol)
		if err != nil {
			fmt.Printf("无法创建到 %s 的聊天流: %v\n", targetPeerID, err)
			// 提供一些可能的错误原因提示
			if strings.Contains(err.Error(), "no addresses") && globalRelayPeerID != "" {
				fmt.Println("提示: 确保目标节点已连接到同一个中继，并且中继服务可用。")
			} else if strings.Contains(err.Error(), "context deadline exceeded") {
				fmt.Println("提示: 连接超时。目标节点可能不在线，或网络连接存在问题。")
			}
			return
		}
		streams.Store(targetPeerID, s) // 存储新创建的流
		go readStream(s)               // 为新流启动读取goroutine
		fmt.Printf("已成功建立到 %s 的新聊天流。\n", targetPeerID)
	}

	// 如果消息内容不为空，则发送消息
	if msg != "" {
		_, err = s.Write([]byte(msg + "\n")) // 将消息写入流 (追加换行符)
		if err != nil {                      // 如果写入失败
			fmt.Printf("发送消息到 %s 失败: %v\n", targetPeerID, err)
			s.Close()                    // 关闭损坏的流
			streams.Delete(targetPeerID) // 从map中移除
			currentChatTargetMtx.Lock()
			if currentChatTarget == targetPeerID { // 如果是当前聊天对象
				currentChatTarget = "" // 退出一对一聊天模式
				fmt.Println("因发送失败，已自动退出与该用户的一对一聊天模式。")
			}
			currentChatTargetMtx.Unlock()
			return
		}

		// 判断消息是通过直接连接还是中继发送的
		conn := s.Conn()
		isRelayed := false
		remoteAddr := conn.RemoteMultiaddr()
		if remoteAddr != nil && strings.Contains(remoteAddr.String(), "/p2p-circuit") {
			isRelayed = true
		}

		// MODIFICATION: 移除了 !*forceRelay 条件
		if isRelayed {
			fmt.Printf("消息已通过中继成功发送到 %s\n", targetPeerID)
		} else {
			fmt.Printf("消息已通过直接连接成功发送到 %s\n", targetPeerID)
		}
	}
}

// broadcastMessage 向所有当前已建立聊天连接的活跃节点广播消息 (不包括中继节点和自己)
func broadcastMessage(h host.Host, msg string) {
	if msg == "" { // 不广播空消息
		return
	}
	fmt.Println("准备向所有活跃聊天连接广播消息...")
	sentCount := 0
	streams.Range(func(key, value interface{}) bool { // 遍历 streams map
		targetPeerID, okKey := key.(peer.ID)
		s, okVal := value.(network.Stream)
		if !okKey || !okVal || s == nil { // 确保类型正确且流有效
			return true // 继续遍历下一个
		}

		// 不向自己或中继节点广播消息
		if targetPeerID == h.ID() || (globalRelayPeerID != "" && targetPeerID == globalRelayPeerID) {
			return true // 跳过自己和中继
		}

		_, err := s.Write([]byte(msg + "\n")) // 发送消息
		if err != nil {
			fmt.Printf("广播消息到 %s 失败: %v\n", targetPeerID, err)
			s.Close()                    // 关闭失败的流
			streams.Delete(targetPeerID) // 从map中移除
		} else {
			sentCount++
			conn := s.Conn()
			isRelayed := false
			remoteAddr := conn.RemoteMultiaddr()
			if remoteAddr != nil && strings.Contains(remoteAddr.String(), "/p2p-circuit") {
				isRelayed = true
			}
			// MODIFICATION: 移除了 !*forceRelay 条件
			if isRelayed {
				fmt.Printf("    广播消息已通过中继发送到 %s\n", targetPeerID)
			} else {
				fmt.Printf("    广播消息已通过直接连接发送到 %s\n", targetPeerID)
			}
		}
		return true // 继续遍历
	})

	if sentCount == 0 {
		fmt.Println("没有活跃的聊天连接可以广播消息。")
	} else {
		fmt.Printf("广播完成，共发送给 %d 个节点。\n", sentCount)
	}
}

// connectToPeerByID 用于通过指定的Peer ID与其他节点建立连接，并发送连接请求，成功后进入一对一聊天
func connectToPeerByID(h host.Host, targetPeerID peer.ID) {
	// peerInfo := h.Peerstore().PeerInfo(targetPeerID) // 获取已知的对方节点信息 (地址等)
	// 地址可能为空，特别是如果对方仅通过中继可达且尚未直接发现

	if targetPeerID == h.ID() { // 不能连接自己
		fmt.Println("错误：不能连接到自己。")
		return
	}
	if globalRelayPeerID != "" && targetPeerID == globalRelayPeerID { // 不能连接中继进行聊天
		fmt.Println("错误：不能直接连接到中继节点进行聊天。中继节点仅用于转发。")
		return
	}

	// 如果已连接到中继节点，且目标节点地址未知，则手动构建中继地址
	if globalRelayPeerID != "" {
		// 检查是否已有目标节点的地址
		knownAddrs := h.Peerstore().Addrs(targetPeerID)
		if len(knownAddrs) == 0 {
			fmt.Printf("目标节点 %s 的地址未知，尝试通过中继连接...\n", targetPeerID)

			// 获取中继节点的地址
			relayAddrs := h.Peerstore().Addrs(globalRelayPeerID)
			if len(relayAddrs) > 0 {
				// 为每个中继地址构建一个通过中继到目标节点的地址
				for _, relayAddr := range relayAddrs {
					// 构建中继地址格式：/中继地址/p2p/中继ID/p2p-circuit/p2p/目标ID
					relayedAddr := relayAddr.Encapsulate(ma.StringCast("/p2p/" + globalRelayPeerID.String()))
					relayedAddr = relayedAddr.Encapsulate(ma.StringCast("/p2p-circuit/p2p/" + targetPeerID.String()))

					// 将构建的中继地址添加到目标节点的Peerstore中
					h.Peerstore().AddAddr(targetPeerID, relayedAddr, peerstore.TempAddrTTL)
					fmt.Printf("已添加中继地址: %s\n", relayedAddr)
				}
			} else {
				fmt.Println("错误：无法获取中继节点地址")
			}
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second) // 增加连接和请求的总体超时
	defer cancel()

	// libp2p的h.NewStream会自动尝试建立底层连接（如果尚不存在）。
	// 如果主机配置了ForceReachabilityRelayed，或者目标节点已知只能通过中继访问，
	// libp2p会自动尝试使用中继。

	fmt.Printf("尝试向节点 %s 发送连接请求 (libp2p将自动选择最佳路径)...\n", targetPeerID)

	// 使用 connReqProtocol 协议向目标节点发起新流，用于发送连接请求
	reqStream, err := h.NewStream(ctx, targetPeerID, connReqProtocol)
	if err != nil {

		fmt.Printf("无法创建到 %s 的连接请求流: %v\n", targetPeerID, err)
		if strings.Contains(err.Error(), "no addresses") && globalRelayPeerID != "" && *forceRelay {
			fmt.Println("提示: 目标节点可能未连接到同一中继，或者中继服务不可用。")
		} else if strings.Contains(err.Error(), "context deadline exceeded") {
			fmt.Println("提示: 连接超时。目标节点可能不在线或无法访问。")
		} else if strings.Contains(err.Error(), "dial backoff") {
			fmt.Println("提示: 连接尝试可能因重复失败 (dial backoff) 而暂时受限。请稍后再试或检查目标节点状态。")
		}
		return
	}
	defer reqStream.Close() // 确保连接请求流在函数结束时关闭

	// 在连接请求流上发送一个简单的请求消息
	_, err = reqStream.Write([]byte("request_connection\n"))
	if err != nil {
		fmt.Printf("发送连接请求到 %s 失败: %v\n", targetPeerID, err)
		return
	}

	// 等待对方通过连接请求流返回响应 (accepted/rejected)
	reader := bufio.NewReader(reqStream)
	response, err := reader.ReadString('\n')
	if err != nil {
		fmt.Printf("读取来自 %s 的连接请求响应失败: %v\n", targetPeerID, err)
		return
	}
	response = strings.TrimSpace(response)

	if response == "accepted" { // 如果对方接受了连接请求
		fmt.Printf("来自 %s 的连接请求已被接受。正在尝试建立聊天流...\n", targetPeerID)
		// 连接请求被接受后，调用sendMessage函数 (传入空消息) 来建立实际的聊天流 (使用chatProtocol)
		sendMessage(h, targetPeerID, "") // 空消息仅用于建立流

		// 检查聊天流是否真的通过sendMessage成功建立并存储
		if _, ok := streams.Load(targetPeerID); ok {
			fmt.Printf("已与 %s 成功建立聊天连接！\n", targetPeerID)
			currentChatTargetMtx.Lock()
			currentChatTarget = targetPeerID // 设置为当前一对一聊天对象
			currentChatTargetMtx.Unlock()
			fmt.Printf("您现在已进入与 %s 的一对一聊天模式。直接输入消息即可发送。\n", targetPeerID)
			fmt.Println("输入 /exit_chat 可退出此模式。")
		} else {
			// 这种情况理论上不应发生，如果连接请求接受了但聊天流没建立起来，说明sendMessage内部逻辑可能有问题
			fmt.Printf("错误：与 %s 的连接请求被接受，但建立聊天流失败。请检查日志。\n", targetPeerID)
		}
	} else { // 如果对方拒绝了连接请求
		fmt.Printf("来自 %s 的连接请求被拒绝。\n", targetPeerID)
	}
}

// disconnectFromPeer 断开与指定Peer ID的聊天流，并更新状态
func disconnectFromPeer(targetPeerID peer.ID) {
	if sVal, ok := streams.Load(targetPeerID); ok { // 检查是否存在与该Peer的流
		if s, sOk := sVal.(network.Stream); sOk { // 类型断言确保是network.Stream
			s.Close()                    // 关闭流
			streams.Delete(targetPeerID) // 从全局map中移除该流的记录
			fmt.Printf("已断开与 %s 的聊天连接。\n", targetPeerID)
			currentChatTargetMtx.Lock()
			if currentChatTarget == targetPeerID { // 如果断开的是当前一对一聊天对象
				currentChatTarget = "" // 重置一对一聊天状态
				fmt.Println("已退出当前的一对一聊天模式。")
			}
			currentChatTargetMtx.Unlock()
			return
		}
	}
	// 如果streams map中没有找到对应的流，说明本来就没有连接或已被断开
	fmt.Printf("与 %s 并没有活跃的聊天连接，无需断开。\n", targetPeerID)
}

// genDebugKey 根据给定的端口号生成一个固定的RSA私钥，用于调试时获得固定的PeerID
func genDebugKey(port int) crypto.PrivKey {
	// 使用固定的种子来初始化随机数生成器，确保每次生成的密钥对是相同的。
	// 这对于调试非常有用，因为PeerID将保持不变。
	// 注意：在生产环境中，应使用不带自定义Reader的 crypto.GenerateKeyPair(crypto.RSA, 2048) 来生成随机密钥。
	r := mrand.New(mrand.NewSource(int64(port)))                          // 使用端口号作为种子，确保不同端口有不同但固定的ID
	priv, _, err := crypto.GenerateKeyPairWithReader(crypto.RSA, 2048, r) // 生成2048位的RSA密钥对
	if err != nil {
		panic(fmt.Sprintf("生成调试密钥失败: %v", err)) // 如果失败则panic
	}
	return priv
}

// monitorConnections 定期监控并打印当前节点的连接状态信息
func monitorConnections(h host.Host) {
	ticker := time.NewTicker(30 * time.Second) // 创建一个每30秒触发一次的定时器
	defer ticker.Stop()                        // 确保在函数退出时停止定时器，释放资源

	for range ticker.C { // 每当定时器触发时执行循环体
		inputMtx.Lock() // 获取标准输入锁，避免与用户输入提示符冲突
		fmt.Println("\n--- 当前连接状态 ---")

		if globalRelayPeerID != "" { // 如果配置并尝试连接了中继
			// 检查与中继节点的实际连接状态
			if h.Network().Connectedness(globalRelayPeerID) == network.Connected {
				fmt.Printf("已连接到中继节点: %s\n", globalRelayPeerID)
			} else {
				fmt.Printf("警告：与配置的中继节点 %s 的连接已断开或未成功建立。\n", globalRelayPeerID)
				// 可选：尝试重新连接中继或将globalRelayPeerID清空
			}
		} else if *mode == peerMode && *relayAddr != "" { // 配置了中继地址但globalRelayPeerID为空 (可能连接失败)
			fmt.Println("已配置中继地址，但当前未连接到中继节点 (可能初始连接失败)。")
		} else { // 没有配置中继地址
			fmt.Println("未配置中继节点或未尝试连接中继。")
		}

		fmt.Println("本地活跃聊天连接 (已建立聊天流的对等节点):")
		foundChatPeers := false
		streams.Range(func(key, value interface{}) bool { // 遍历streams map
			pID, okKey := key.(peer.ID)
			stream, okVal := value.(network.Stream) // 检查流是否有效
			if !okKey || !okVal {
				return true // 继续遍历
			}

			// 进一步检查流的底层连接是否仍然活跃
			conn := stream.Conn()
			if conn == nil || h.Network().Connectedness(pID) != network.Connected {
				// 流可能已经失效或者其底层连接已断开
				// 可以考虑在这里从streams map中移除这种失效的流
				// streams.Delete(pID)
				// log.Printf("调试: 节点 %s 的流似乎已失效或连接断开。\n", pID)
				return true // 继续遍历
			}

			// 不列出自己或中继节点
			if pID != h.ID() && (globalRelayPeerID == "" || pID != globalRelayPeerID) {
				isRelayed := false
				if conn.RemoteMultiaddr() != nil && strings.Contains(conn.RemoteMultiaddr().String(), "/p2p-circuit") {
					isRelayed = true
				}

				// MODIFICATION: 移除了 !*forceRelay 条件
				if isRelayed {
					fmt.Printf("    %s (通过中继)\n", pID)
				} else {
					fmt.Printf("    %s (直接连接)\n", pID)
				}
				foundChatPeers = true
			}
			return true // 继续遍历
		})
		if !foundChatPeers {
			fmt.Println("    目前没有与其他对等节点建立聊天连接。")
		}

		// 显示当前一对一聊天目标（如果有）
		currentChatTargetMtx.Lock()
		if currentChatTarget != "" {
			// 验证当前聊天对象是否仍然在活跃流中
			if _, stillConnected := streams.Load(currentChatTarget); stillConnected {
				fmt.Printf("当前一对一聊天目标: %s\n", currentChatTarget)
			} else {
				// 如果当前聊天对象已不在streams中，说明连接可能已断开
				fmt.Printf("先前的一对一聊天目标 %s 的连接已断开。\n", currentChatTarget)
				currentChatTarget = "" // 清空，退出一对一模式
			}
		}
		currentChatTargetMtx.Unlock()

		fmt.Println("--------------------")
		fmt.Print("> ")   // 重新打印命令提示符
		inputMtx.Unlock() // 释放标准输入锁
	}
}
