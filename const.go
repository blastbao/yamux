package yamux

import (
	"encoding/binary"
	"fmt"
)

var (
	// ErrInvalidVersion means we received a frame with an invalid version
	ErrInvalidVersion = fmt.Errorf("invalid protocol version")

	// ErrInvalidMsgType means we received a frame with an invalid message type
	ErrInvalidMsgType = fmt.Errorf("invalid msg type")

	// ErrSessionShutdown is used if there is a shutdown during an operation
	ErrSessionShutdown = fmt.Errorf("session shutdown")

	// ErrStreamsExhausted is returned if we have no more stream ids to issue
	ErrStreamsExhausted = fmt.Errorf("streams exhausted")

	// ErrDuplicateStream is used if a duplicate stream is opened inbound
	ErrDuplicateStream = fmt.Errorf("duplicate stream initiated")

	// ErrReceiveWindowExceeded indicates the window was exceeded
	ErrRecvWindowExceeded = fmt.Errorf("recv window exceeded")

	// ErrTimeout is used when we reach an IO deadline
	ErrTimeout = fmt.Errorf("i/o deadline reached")

	// ErrStreamClosed is returned when using a closed stream
	ErrStreamClosed = fmt.Errorf("stream closed")

	// ErrUnexpectedFlag is set when we get an unexpected flag
	ErrUnexpectedFlag = fmt.Errorf("unexpected flag")

	// ErrRemoteGoAway is used when we get a go away from the other side
	ErrRemoteGoAway = fmt.Errorf("remote end is not accepting connections")

	// ErrConnectionReset is sent if a stream is reset.
	// This can happen if the backlog is exceeded, or if there was a remote GoAway.
	ErrConnectionReset = fmt.Errorf("connection reset")

	// ErrConnectionWriteTimeout indicates that we hit the "safety valve"
	// timeout writing to the underlying stream connection.
	ErrConnectionWriteTimeout = fmt.Errorf("connection write timeout")

	// ErrKeepAliveTimeout is sent if a missed keepalive caused the stream close
	ErrKeepAliveTimeout = fmt.Errorf("keepalive timeout")
)

const (
	// protoVersion is the only version we support
	protoVersion uint8 = 0
)

const (
	// Data is used for data frames.
	// They are followed by length bytes worth of payload.
	typeData uint8 = iota

	// WindowUpdate is used to change the window of a given stream.
	// The length indicates the delta update to the window.
	typeWindowUpdate

	// Ping is sent as a keep-alive or to measure the RTT.
	// The StreamID and Length value are echoed back in the response.
	typePing

	// GoAway is sent to terminate a session.
	// The StreamID should be 0 and the length is an error code.
	typeGoAway
)

const (


	// SYN is sent to signal a new stream.
	// May be sent with a data payload
	flagSYN uint16 = 1 << iota

	// ACK is sent to acknowledge a new stream.
	// May be sent with a data payload
	flagACK

	// FIN is sent to half-close the given stream.
	// May be sent with a data payload.
	flagFIN

	// RST is used to hard close a given stream.
	flagRST
)

const (
	// initialStreamWindow is the initial stream window size
	initialStreamWindow uint32 = 256 * 1024
)

const (

	// goAwayNormal is sent on a normal termination
	goAwayNormal uint32 = iota

	// goAwayProtoErr sent on a protocol error
	goAwayProtoErr

	// goAwayInternalErr sent on an internal error
	goAwayInternalErr
)



const (
	sizeOfVersion  = 1
	sizeOfType     = 1
	sizeOfFlags    = 2
	sizeOfStreamID = 4
	sizeOfLength   = 4

	// 16B
	headerSize     = sizeOfVersion + sizeOfType + sizeOfFlags + sizeOfStreamID + sizeOfLength
)



type header []byte

func (h header) Version() uint8 {
	return h[0]
}

func (h header) MsgType() uint8 {
	return h[1]
}

func (h header) Flags() uint16 {
	return binary.BigEndian.Uint16(h[2:4])
}

func (h header) StreamID() uint32 {
	return binary.BigEndian.Uint32(h[4:8])
}

func (h header) Length() uint32 {
	return binary.BigEndian.Uint32(h[8:12])
}

func (h header) String() string {
	return fmt.Sprintf("Vsn:%d Type:%d Flags:%d StreamID:%d Length:%d",
		h.Version(), h.MsgType(), h.Flags(), h.StreamID(), h.Length())
}

// 每一帧数据都需要包含如下的头部 header :
//
//  Version(1B)
//  Type(1B)
//  Flags(2B)
//  StreamID(4B)
//  Length(4B)
//
// 一共 12 个字节，所有的字段都采用大端（big endian）传输。
//
// 接下来我们解释每个字段的意思:
//
// 1. Version 字段 --- 版本号
// 目的是为了向后兼容，当前版本中 vsn 始终等于 0 。
//
// 2. Type 字段 --- 消息类型
//
//	0x0 Data - 用于数据传输，其中 length 代表 payload 的长度。
//	0x1 Window Update - 更新 Stream 发送者的接收窗口 recvWindow 大小，length 标示窗口的增量更新值。
//	0x2 Ping - 用作 keep-alives 心跳保持长连接，或者测试 RTT 值，在响应中要回复 StreamID 和 Length 属性。
//	0x3 Go Away -  用来终止会话，此时 StreamID = 0，而 Length 标示ErrorCode。
//
//
// 3. Flag 字段 --- 配合 type 字段提供额外的信息
//
//	0x1 SYN - 创建一个新的 Stream。可以在 data 或者 window update 消息中发送，也可以在 ping 消息中发送以表明是带外数据。
//	0x2 Ack - 用于响应 SYN 消息。可以在 data 或者 window update 消息中发送，也可以在 ping 消息中发送以表明是带外数据。
//	0x4 FIN - 执行 Stream 的半关闭。可以在 data 或者 window update 消息中发送。
//	0x8 RST - 立即 reset 一个 Stream 。可以在 data 或者 window update 消息中发送。
//
// 4. StreamID 字段
//
// 用来区分同一个 session 下的不同的 stream，这个 StreamID 就是不同数据流的标识。
// 客户端使用奇数的 ID，服务端使用偶数 ID，0 代表了这个 session 本身。
// ping 和 go away 消息的 StreamID 为0。
//
//
// 5. Length 字段 --- 根据不同的消息类型，length 的意义也不一样
//
//	Data - 表示紧跟头部的数据长度（单位：bytes）
//	Window update - 更新窗口大小的增量
//	Ping - 不透明的值，回显
//	Go Away - 包含错误码
func (h header) encode(msgType uint8, flags uint16, streamID uint32, length uint32) {
	h[0] = protoVersion
	h[1] = msgType
	binary.BigEndian.PutUint16(h[2:4], flags)
	binary.BigEndian.PutUint32(h[4:8], streamID)
	binary.BigEndian.PutUint32(h[8:12], length)
}
