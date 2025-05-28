package common

import (
	"time"

	"github.com/libp2p/go-libp2p/core/protocol"
)

const (
	// RelayProtocolID 是中继节点用于处理消息转发的协议ID
	RelayProtocolID protocol.ID = "/chat/relay/1.0.0"
	// ChatProtocolID 是普通节点之间用于直接聊天消息的协议ID
	ChatProtocolID protocol.ID = "/chat/message/1.0.0"
	// DiscoveryInterval 是节点发现的间隔时间
	DiscoveryInterval = 30 * time.Second
	// RelayConnectTimeout 是连接中继节点的超时时间
	RelayConnectTimeout = 10 * time.Second
	// MessageBufferSize 是消息队列的缓冲区大小
	MessageBufferSize = 1024
)
