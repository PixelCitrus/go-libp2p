package main

import (
	"flag"
	"fmt"
	"strings"
)

type Config struct {
	// 节点模式：peer 或 relay
	Mode string
	// 监听地址
	ListenAddrs []string
	// 监听端口
	Port int
	// 中继节点地址
	RelayAddrs []string
	// 用于节点发现的标识字符串
	RendezvousString string
}

func ParseFlags() *Config {
	config := &Config{}

	// 解析命令行参数
	mode := flag.String("mode", "peer", "节点模式: peer 或 relay")
	port := flag.Int("port", 0, "监听端口（必需）")
	relayAddrs := flag.String("relay", "", "中继节点地址，多个地址用逗号分隔")
	rendezvous := flag.String("rendezvous", "chat-with-relay", "用于节点发现的标识字符串")

	flag.Parse()

	// 验证必需参数
	if *port == 0 {
		fmt.Println("Error: 必须指定监听端口")
		flag.Usage()
		return nil
	}

	// 设置配置
	config.Mode = *mode
	config.Port = *port
	config.RendezvousString = *rendezvous

	// 设置监听地址（同时支持 IPv4 和 IPv6）
	config.ListenAddrs = []string{
		fmt.Sprintf("/ip4/0.0.0.0/tcp/%d", *port),
		fmt.Sprintf("/ip6/::/tcp/%d", *port),
	}

	// 解析中继节点地址
	if *relayAddrs != "" {
		config.RelayAddrs = strings.Split(*relayAddrs, ",")
		// 移除空白字符
		for i := range config.RelayAddrs {
			config.RelayAddrs[i] = strings.TrimSpace(config.RelayAddrs[i])
		}
	}

	// 验证模式
	if config.Mode != "peer" && config.Mode != "relay" {
		fmt.Printf("Error: 无效的模式 '%s'，必须是 'peer' 或 'relay'\n", config.Mode)
		flag.Usage()
		return nil
	}

	// 如果是普通节点模式，验证是否提供了中继节点地址
	if config.Mode == "peer" && len(config.RelayAddrs) == 0 {
		fmt.Println("Warning: 未指定中继节点地址，将只能在本地网络中发现节点")
	}

	return config
}
