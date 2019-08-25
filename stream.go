package yamux

import (
	"bytes"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

type streamState int

const (
	streamInit streamState = iota
	streamSYNSent
	streamSYNReceived
	streamEstablished
	streamLocalClose
	streamRemoteClose
	streamClosed
	streamReset
)

// Stream is used to represent a logical stream within a session.
// Stream 被用于表示会话 session 中的逻辑流。
type Stream struct {

	recvWindow uint32 		//接收窗口
	sendWindow uint32 		//发送窗口

	id      uint32			//流ID
	session *Session		//归属的会话

	state     streamState	//流状态
	stateLock sync.Mutex	//互斥锁

	recvBuf  *bytes.Buffer 	//接收缓存
	recvLock sync.Mutex 	//缓存锁

	controlHdr     header		//
	controlErr     chan error	//
	controlHdrLock sync.Mutex	//

	sendHdr  header				//
	sendErr  chan error			//
	sendLock sync.Mutex			//

	recvNotifyCh chan struct{}
	sendNotifyCh chan struct{}

	readDeadline  atomic.Value // time.Time
	writeDeadline atomic.Value // time.Time
}

// newStream is used to construct a new stream within a given session for an ID
func newStream(session *Session, id uint32, state streamState) *Stream {

	s := &Stream{
		id:           id,
		session:      session,
		state:        state,
		controlHdr:   header(make([]byte, headerSize)),	// 消息头大小 headerSize = 12byte
		controlErr:   make(chan error, 1),
		sendHdr:      header(make([]byte, headerSize)), // 消息头大小 headerSize = 12byte
		sendErr:      make(chan error, 1),
		recvWindow:   initialStreamWindow, 				// 初始的流窗口尺寸 initialStreamWindow = 256k
		sendWindow:   initialStreamWindow,				// 初始的流窗口尺寸 initialStreamWindow = 256k
		recvNotifyCh: make(chan struct{}, 1),
		sendNotifyCh: make(chan struct{}, 1),
	}

	s.readDeadline.Store(time.Time{})
	s.writeDeadline.Store(time.Time{})
	return s
}

// Session returns the associated stream session
func (s *Stream) Session() *Session {
	return s.session
}

// StreamID returns the ID of this stream
func (s *Stream) StreamID() uint32 {
	return s.id
}





// Read is used to read from the stream
func (s *Stream) Read(b []byte) (n int, err error) {




	defer asyncNotify(s.recvNotifyCh)



START:
	s.stateLock.Lock()








	switch s.state {
	case streamLocalClose:
		fallthrough
	case streamRemoteClose:
		fallthrough
	case streamClosed:
		s.recvLock.Lock()
		if s.recvBuf == nil || s.recvBuf.Len() == 0 {
			s.recvLock.Unlock()
			s.stateLock.Unlock()
			return 0, io.EOF
		}
		s.recvLock.Unlock()
	case streamReset:
		s.stateLock.Unlock()
		return 0, ErrConnectionReset
	}


	s.stateLock.Unlock()

	// If there is no data available, block
	s.recvLock.Lock()
	if s.recvBuf == nil || s.recvBuf.Len() == 0 {
		s.recvLock.Unlock()
		goto WAIT
	}

	// Read any bytes
	n, _ = s.recvBuf.Read(b)
	s.recvLock.Unlock()

	// Send a window update potentially
	err = s.sendWindowUpdate()
	return n, err

WAIT:
	var timeout <-chan time.Time
	var timer *time.Timer
	readDeadline := s.readDeadline.Load().(time.Time)
	if !readDeadline.IsZero() {
		delay := readDeadline.Sub(time.Now())
		timer = time.NewTimer(delay)
		timeout = timer.C
	}
	select {
	case <-s.recvNotifyCh:
		if timer != nil {
			timer.Stop()
		}
		goto START
	case <-timeout:
		return 0, ErrTimeout
	}
}

// Write is used to write to the stream
func (s *Stream) Write(b []byte) (n int, err error) {
	s.sendLock.Lock()
	defer s.sendLock.Unlock()
	total := 0
	for total < len(b) {
		n, err := s.write(b[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// write is used to write to the stream, may return on a short write.
func (s *Stream) write(b []byte) (n int, err error) {
	var flags uint16
	var max uint32
	var body io.Reader
START:
	s.stateLock.Lock()
	switch s.state {
	case streamLocalClose:
		fallthrough
	case streamClosed:
		s.stateLock.Unlock()
		return 0, ErrStreamClosed
	case streamReset:
		s.stateLock.Unlock()
		return 0, ErrConnectionReset
	}
	s.stateLock.Unlock()

	// If there is no data available, block
	window := atomic.LoadUint32(&s.sendWindow)
	if window == 0 {
		goto WAIT
	}

	// Determine the flags if any
	flags = s.sendFlags()

	// Send up to our send window
	max = min(window, uint32(len(b)))
	body = bytes.NewReader(b[:max])

	// Send the header
	//
	s.sendHdr.encode(typeData, flags, s.id, max)


	//
	if err = s.session.waitForSendErr(s.sendHdr, body, s.sendErr); err != nil {
		return 0, err
	}

	// Reduce our send window
	atomic.AddUint32(&s.sendWindow, ^uint32(max-1))

	// Unlock
	return int(max), err


WAIT:

	var timeout <-chan time.Time
	writeDeadline := s.writeDeadline.Load().(time.Time)
	if !writeDeadline.IsZero() {
		delay := writeDeadline.Sub(time.Now())
		timeout = time.After(delay)
	}

	select {
	case <-s.sendNotifyCh:
		goto START
	case <-timeout:
		return 0, ErrTimeout
	}


	return 0, nil
}



// sendFlags determines any flags that are appropriate based on the current stream state
//
// sendFlags() 根据当前流 Stream 的状态 state 确定适当的标志 flags 。
// 如果当前 Stream 处于 streamInit 初始化状态，则返回 flag |= flagSYN 且将 Stream 流转至 streamSYNSent 状态。
// 如果当前 Stream 处于 streamSYNReceived 状态，则返回 flag |= flagACK 且 Stream 流转至 streamEstablished 状态。
// 如果当前 Stream 未处于 streamInit/streamSYNReceived 状态，则 flags 为 0 。
func (s *Stream) sendFlags() uint16 {
	s.stateLock.Lock()
	defer s.stateLock.Unlock()

	// 连接过程中的 Stream 状态流转，返回流转过程中的 flag 标识。

	var flags uint16
	switch s.state {
	// 如果当前 Stream 处于 streamInit 初始化状态，则 flag |= flagSYN 且 Stream 流转至 streamSYNSent 状态。
	case streamInit:
		flags |= flagSYN
		s.state = streamSYNSent

	// 如果当前 Stream 处于 streamSYNReceived 状态，则 flag |= flagACK 且 Stream 流转至 streamEstablished 状态。
	case streamSYNReceived:
		flags |= flagACK
		s.state = streamEstablished
	}
	// 如果当前 Stream 未处于 streamInit/streamSYNReceived 状态，则 flags 为 0 。

	return flags
}




// sendWindowUpdate potentially sends a window update enabling further writes to take place.
// Must be invoked with the lock.

// sendWindowUpdate() 可能会发送一个窗口更新消息，使进一步的写入能够发生。
func (s *Stream) sendWindowUpdate() error {


	s.controlHdrLock.Lock()
	defer s.controlHdrLock.Unlock()

	// Determine the delta update


	// 默认最大流窗口大小 MaxStreamWindowSize == 256k
	max := s.session.config.MaxStreamWindowSize

	// bufLen 为当前 s.recvBuf 中存储的数据字节数，若 s.recvBuf 为 nil 则为 0 。
	var bufLen uint32
	s.recvLock.Lock()

	// s.recvBuf 是在 readData 时通过 bytes.NewBuffer() 函数分配的。
	if s.recvBuf != nil {
		bufLen = uint32(s.recvBuf.Len())
	}


	// 最大容量 max 减去已使用容量 bufLen ，差值为剩余可用的 buf 容量，
	// 由此可见，在每次 readData 后都会减小窗口，这就是熟悉的滑动窗口协议。
	//
	// 由于要计算 window delta， 所以还要减去当前窗口大小 s.recvWindow。
	delta := (max - bufLen) - s.recvWindow


	// Determine the flags if any
	//
	// sendFlags() 根据当前流 Stream 的状态 state 确定适当的标志 flags 。
	// 如果当前 Stream 处于 streamInit 初始化状态，则返回 flag |= flagSYN 且将 Stream 流转至 streamSYNSent 状态。
	// 如果当前 Stream 处于 streamSYNReceived 状态，则返回 flag |= flagACK 且 Stream 流转至 streamEstablished 状态。
	// 如果当前 Stream 未处于 streamInit/streamSYNReceived 状态，则 flags 为 0 。
	flags := s.sendFlags()


	// Check if we can omit the update
	//
	// 如果窗口的增量不大，且当前 Stream 未处于 streamInit/streamSYNReceived 状态，则无需更新窗口，也无需发送响应消息给对端。
	if delta < (max/2) && flags == 0 {
		s.recvLock.Unlock()
		return nil
	}


	// Update our window
	// 增量更新 recvWindow 窗口大小，
	s.recvWindow += delta
	s.recvLock.Unlock()



	// Send the header
	//
	// 向对端发送一个 header=[]byte{0, windowUpdate, flags, streamID, delta} 消息。
	s.controlHdr.encode(typeWindowUpdate, flags, s.id, delta)
	if err := s.session.waitForSendErr(s.controlHdr, nil, s.controlErr); err != nil {
		return err
	}
	return nil
}

// sendClose is used to send a FIN
//
// 发送 FIN 控制信令给对端
func (s *Stream) sendClose() error {

	s.controlHdrLock.Lock()
	defer s.controlHdrLock.Unlock()

	// sendFlags() 根据当前流 Stream 的状态 state 确定适当的标志 flags 。
	// 如果当前 Stream 处于 streamInit 初始化状态，则返回 flag |= flagSYN 且将 Stream 流转至 streamSYNSent 状态。
	// 如果当前 Stream 处于 streamSYNReceived 状态，则返回 flag |= flagACK 且 Stream 流转至 streamEstablished 状态。
	// 如果当前 Stream 未处于 streamInit/streamSYNReceived 状态，则 flags 为 0 。
	flags := s.sendFlags()

	// 为 flags 附加 FIN 标记位
	flags |= flagFIN

	// 用 FIN flags 构造 WindowUpdate 控制消息
	s.controlHdr.encode(typeWindowUpdate, flags, s.id, 0)

	// 发送 FIN 控制消息给对端，发送过程中出现的错误会写入到 s.controlErr 中，整个函数的执行结果会以 err 返回。
	if err := s.session.waitForSendErr(s.controlHdr, nil, s.controlErr); err != nil {
		return err
	}

	return nil
}

// Close is used to close the stream
func (s *Stream) Close() error {
	closeStream := false
	s.stateLock.Lock()
	switch s.state {
	// Opened means we need to signal a close
	case streamSYNSent:
		fallthrough
	case streamSYNReceived:
		fallthrough
	case streamEstablished:
		s.state = streamLocalClose
		goto SEND_CLOSE

	case streamLocalClose:
	case streamRemoteClose:
		s.state = streamClosed
		closeStream = true
		goto SEND_CLOSE

	case streamClosed:
	case streamReset:
	default:
		panic("unhandled state")
	}
	s.stateLock.Unlock()
	return nil
SEND_CLOSE:
	s.stateLock.Unlock()
	s.sendClose()
	s.notifyWaiting()
	if closeStream {
		s.session.closeStream(s.id)
	}
	return nil
}

// forceClose is used for when the session is exiting
func (s *Stream) forceClose() {
	s.stateLock.Lock()
	s.state = streamClosed
	s.stateLock.Unlock()
	s.notifyWaiting()
}

// processFlags is used to update the state of the stream based on set flags, if any.
// Lock must be held.
//
// 1. 根据 flags 标识判断连接状态，从而更新 Stream 的 state。
// 2. 如果 flags 状态意味着连接的关闭，则在函数退出前会执行 closeStream() 关闭 Stream。
// 3. 在特定时刻调用 s.notifyWaiting() ...... ????
func (s *Stream) processFlags(flags uint16) error {


	// Close the stream without holding the state lock
	closeStream := false
	defer func() {
		// 如果下面的操作将 closeStream 置为 true，则在函数退出时执行流的关闭
		if closeStream {
			s.session.closeStream(s.id)
		}
	}()

	s.stateLock.Lock()
	defer s.stateLock.Unlock()

	// 1. 收到 SYN ACK 包，对端已经建立连接。
	if flags&flagACK == flagACK {

		// 若当前 state = SYNSent，则 state 会流转至 Established；若当前处于其它状态，则不做处理。
		if s.state == streamSYNSent {
			s.state = streamEstablished
		}

		// 因为已经收到 SYN ACK 包，意味着当前 stream 已经完成连接建立，更新一下。
		s.session.establishStream(s.id)
	}

	// 2. 收到 FIN 包，意味着对端关闭连接。
	// (1) 若当前 state = SYNSent/SYNReceived/Established, 则 state 会流转至 RemoteClose, 意味着对端已经关闭连接，但本端尚未关闭，也即处于半连状态。
	// (2) 若当前 state = LocalClose, 意味着双端都已经关闭连接，则 state 会流转至 Closed，同时设置流关闭标识 closeStream 为 true。
	// (3) 若当前 state 处于其他状态, 则报错。
	if flags&flagFIN == flagFIN {

		switch s.state {
		case streamSYNSent:
			fallthrough
		case streamSYNReceived:
			fallthrough
		case streamEstablished:
			s.state = streamRemoteClose
			s.notifyWaiting()
		case streamLocalClose:
			s.state = streamClosed
			closeStream = true
			s.notifyWaiting()
		default:
			s.session.logger.Printf("[ERR] yamux: unexpected FIN flag in state %d", s.state)
			return ErrUnexpectedFlag
		}
	}

	// 3. 收到 RST 包，需要重置连接。
	// 则把 state 流转至 Reset，同时设置流关闭标识 closeStream 为 true。
	if flags&flagRST == flagRST {
		s.state = streamReset
		closeStream = true
		s.notifyWaiting()
	}


	return nil
}

// notifyWaiting notifies all the waiting channels
func (s *Stream) notifyWaiting() {
	asyncNotify(s.recvNotifyCh)
	asyncNotify(s.sendNotifyCh)
}



// incrSendWindow updates the size of our send window
func (s *Stream) incrSendWindow(hdr header, flags uint16) error {

	// 如果收到 WindowUpdate 类型消息，意味着收到了控制信息，需要检查其所含的 flags，并更新当前窗口大小。

	// 1. 这里先调用 s.processFlags() 检查一下消息附带的 flags，根据 flags 是 ACK、FIN、RST 来更新 stream 的 state 状态。
	if err := s.processFlags(flags); err != nil {
		return err
	}

	// Increase window, unblock a sender
	// 2. 更新 stream 窗口大小
	atomic.AddUint32(&s.sendWindow, hdr.Length())

	// 3.
	asyncNotify(s.sendNotifyCh)
	return nil
}

// readData is used to handle a data frame
func (s *Stream) readData(hdr header, flags uint16, conn io.Reader) error {


	//
	if err := s.processFlags(flags); err != nil {
		return err
	}

	// Check that our recv window is not exceeded
	length := hdr.Length()
	if length == 0 {
		return nil
	}

	// Wrap in a limited reader
	conn = &io.LimitedReader{R: conn, N: int64(length)}

	// Copy into buffer
	s.recvLock.Lock()

	if length > s.recvWindow {
		s.session.logger.Printf("[ERR] yamux: receive window exceeded (stream: %d, remain: %d, recv: %d)", s.id, s.recvWindow, length)
		return ErrRecvWindowExceeded
	}

	if s.recvBuf == nil {
		// Allocate the receive buffer just-in-time to fit the full data frame.
		// This way we can read in the whole packet without further allocations.
		s.recvBuf = bytes.NewBuffer(make([]byte, 0, length))
	}
	if _, err := io.Copy(s.recvBuf, conn); err != nil {
		s.session.logger.Printf("[ERR] yamux: Failed to read stream data: %v", err)
		s.recvLock.Unlock()
		return err
	}

	// Decrement the receive window
	s.recvWindow -= length
	s.recvLock.Unlock()

	// Unblock any readers
	asyncNotify(s.recvNotifyCh)
	return nil
}

// SetDeadline sets the read and write deadlines
func (s *Stream) SetDeadline(t time.Time) error {
	if err := s.SetReadDeadline(t); err != nil {
		return err
	}
	if err := s.SetWriteDeadline(t); err != nil {
		return err
	}
	return nil
}

// SetReadDeadline sets the deadline for future Read calls.
func (s *Stream) SetReadDeadline(t time.Time) error {
	s.readDeadline.Store(t)
	return nil
}

// SetWriteDeadline sets the deadline for future Write calls
func (s *Stream) SetWriteDeadline(t time.Time) error {
	s.writeDeadline.Store(t)
	return nil
}

// Shrink is used to compact the amount of buffers utilized
// This is useful when using Yamux in a connection pool to reduce the idle memory utilization.
func (s *Stream) Shrink() {
	s.recvLock.Lock()
	if s.recvBuf != nil && s.recvBuf.Len() == 0 {
		s.recvBuf = nil
	}
	s.recvLock.Unlock()
}
