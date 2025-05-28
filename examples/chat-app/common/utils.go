package common

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
)

// CreateHost 使用随机身份创建一个 libp2p 主机。
// port 参数指定监听端口，如果为0则随机选择。
func CreateHost(port int) (host.Host, error) {
	// 为了简化，如果传入0，我们将使用随机端口。
	// 在实际应用中，您可能希望指定一个固定端口或使用密钥对。
	// 在此示例中，我们不使用NAT遍历或UPnP以保持简单。
	h, err := libp2p.New(
		libp2p.ListenAddrStrings(fmt.Sprintf("/ip4/0.0.0.0/tcp/%d", port)),
		libp2p.DisableRelay(), // 普通节点默认禁用中继，中继节点才启用
	)
	if err != nil {
		log.Printf("[ERROR] 创建libp2p主机失败，端口 %d: %v", port, err)
		return nil, fmt.Errorf("创建libp2p主机失败: %w", err)
	}
	log.Printf("[INFO] libp2p主机创建成功，监听地址: %s", h.Addrs())
	return h, nil
}

// GetPeerIDFromInput 提示用户输入一个Peer ID。
func GetPeerIDFromInput(prompt string) (peer.ID, error) {
	fmt.Print(prompt)
	reader := bufio.NewReader(os.Stdin)
	input, err := reader.ReadString('\n')
	if err != nil {
		log.Printf("[ERROR] 读取用户输入失败: %v", err)
		return "", fmt.Errorf("读取用户输入失败: %w", err)
	}
	input = strings.TrimSpace(input)
	pid, err := peer.Decode(input)
	if err != nil {
		log.Printf("[ERROR] 解码Peer ID '%s' 失败: %v", input, err)
		return "", fmt.Errorf("无效的Peer ID: %w", err)
	}
	log.Printf("[INFO] 成功解析Peer ID: %s", pid.String()) // 修改点：pid.Pretty() -> pid.String()
	return pid, nil
}

// ReadInput 从标准输入读取一行。
func ReadInput(prompt string) string {
	fmt.Print(prompt)
	reader := bufio.NewReader(os.Stdin)
	input, err := reader.ReadString('\n')
	if err != nil {
		log.Printf("[WARN] 读取标准输入失败: %v", err)
		return ""
	}
	return strings.TrimSpace(input)
}
