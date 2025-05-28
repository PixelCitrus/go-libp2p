package common

// ChatMessage 表示一个聊天消息。
type ChatMessage struct {
	From    string `json:"from"`    // 发送方的PeerID
	To      string `json:"to"`      // 接收方的PeerID（如果为空则为广播或用于中继请求）
	Content string `json:"content"` // 消息内容
	Relayed bool   `json:"relayed"` // 如果消息通过中继转发，则为true
}
