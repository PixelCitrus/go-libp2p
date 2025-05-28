package peer

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"

	"chat-app/common"

	"github.com/libp2p/go-libp2p/core/network"
)

// ChatHandler 处理传入的聊天消息。
func (pn *PeerNode) handleChatMessage(s network.Stream) {
	defer func() {
		if err := s.Close(); err != nil {
			log.Printf("[ERROR] 关闭聊天流失败: %v", err)
		}
	}()

	decoder := json.NewDecoder(s)
	var msg common.ChatMessage
	if err := decoder.Decode(&msg); err != nil {
		if err == io.EOF {
			log.Printf("[INFO] 聊天流从 %s EOF", s.Conn().RemotePeer().String()) // 修改点：s.RemotePeer().Pretty() -> s.RemotePeer().String()
		} else {
			log.Printf("[ERROR] 从 %s 解码聊天消息失败: %v", s.Conn().RemotePeer().String(), err) // 修改点：s.RemotePeer().Pretty() -> s.RemotePeer().String()
		}
		return
	}

	log.Printf("[INFO] 收到来自 %s 的聊天消息 (通过中继: %t): %s", msg.From, msg.Relayed, msg.Content)

	// 打印消息并重新显示用户输入提示符
	fmt.Printf("\n[消息来自 %s]: %s\n> ", msg.From, msg.Content)
	// 清除当前行并将光标移动到行首
	fmt.Print("\033[1A\033[K")
	fmt.Printf("> ")
	os.Stdout.Sync()
}
