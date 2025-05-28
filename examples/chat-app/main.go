package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	// 导入time包以供可能的将来使用，但目前在此文件中未直接使用
	"chat-app/relay"

	"chat-app/peer"
)

func main() {
	// 配置日志输出，例如，将时间戳和文件名/行号添加到日志中
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)

	isRelay := flag.Bool("relay", false, "启动为中继节点")
	port := flag.Int("port", 0, "节点监听端口 (0表示随机)")
	relayAddr := flag.String("relay-addr", "", "中继节点的多地址 (例如: /ip4/127.0.0.1/tcp/4001/p2p/<中继节点的Peer ID>)")
	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if *isRelay {
		log.Println("[INFO] 正在启动中继节点...")
		r, err := relay.NewRelayNode(*port)
		if err != nil {
			log.Fatalf("[FATAL] 启动中继节点失败: %v", err)
		}
		defer func() {
			if err := r.Host.Close(); err != nil {
				log.Printf("[ERROR] 关闭中继主机失败: %v", err)
			} else {
				log.Println("[INFO] 中继主机已关闭。")
			}
		}()

		// 保持中继节点运行
		select {
		case <-ctx.Done():
			log.Println("[INFO] 中继节点正在关闭。")
		case sig := <-setupShutdownSignalHandler():
			log.Printf("[INFO] 捕获到信号 %s。正在关闭中继节点...", sig)
			cancel()
		}
	} else {
		log.Println("[INFO] 正在启动对等体节点...")
		pn, err := peer.NewPeerNode(*port, *relayAddr)
		if err != nil {
			log.Fatalf("[FATAL] 启动对等体节点失败: %v", err)
		}
		defer func() {
			if err := pn.Host.Close(); err != nil {
				log.Printf("[ERROR] 关闭对等体主机失败: %v", err)
			} else {
				log.Println("[INFO] 对等体主机已关闭。")
			}
			if err := pn.DHT.Close(); err != nil {
				log.Printf("[ERROR] 关闭DHT失败: %v", err)
			} else {
				log.Println("[INFO] DHT已关闭。")
			}
		}()

		// 如果指定了中继节点，则连接到中继节点
		if *relayAddr != "" {
			if err := pn.ConnectToRelay(ctx); err != nil {
				// 连接中继可能失败，但节点仍可以尝试直接发现和连接其他节点，所以这里是WARN而不是FATAL
				log.Printf("[WARN] 连接中继节点失败: %v", err)
			}
		}

		// 在后台启动对等体发现
		go pn.DiscoverPeers(ctx)

		// 启动控制台输入处理器
		go pn.StartConsoleInput(ctx)

		select {
		case <-ctx.Done():
			log.Println("[INFO] 对等体节点正在关闭。")
		case sig := <-setupShutdownSignalHandler():
			log.Printf("[INFO] 捕获到信号 %s。正在关闭对等体节点...", sig)
			cancel()
		}
	}
	log.Println("[INFO] 应用已退出。")
}

// setupShutdownSignalHandler 设置一个通道来接收操作系统信号以进行优雅关闭。
func setupShutdownSignalHandler() <-chan os.Signal {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	return sigs
}
