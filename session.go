package yamux

import (
	"bufio"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)









// Session is used to wrap a reliable ordered connection and to
// multiplex it into multiple streams.
type Session struct {



	// remoteGoAway indicates the remote side does not want futher connections.
	// Must be first for alignment.
	remoteGoAway int32



	// localGoAway indicates that we should stop accepting futher connections.
	// Must be first for alignment.
	localGoAway int32



	// nextStreamID is the next stream we should send.
	// This depends if we are a client/server.
	nextStreamID uint32

	// config holds our configuration
	config *Config

	// logger is used for our logs
	logger *log.Logger



	// conn is the underlying connection
	//【重要】底层网络连接
	conn io.ReadWriteCloser

	// bufRead is a buffered reader
	//【重要】bufRead 是底层网络连接 conn 的带缓冲读封装
	bufRead *bufio.Reader


	// pings is used to track inflight pings
	pings    map[uint32]chan struct{}
	pingID   uint32
	pingLock sync.Mutex


	// streams maps a stream id to a stream, and inflight has an entry
	// for any outgoing stream that has not yet been established.
	// Both are protected by streamLock.
	streams    map[uint32]*Stream
	inflight   map[uint32]struct{} // inflight[] 中保存了对尚未完成连接建立的 streams 。
	streamLock sync.Mutex

	// synCh acts like a semaphore. It is sized to the AcceptBacklog which
	// is assumed to be symmetric between the client and server.
	// This allows the client to avoid exceeding the backlog and instead blocks the open.
	synCh chan struct{}

	// acceptCh is used to pass ready streams to the client
	acceptCh chan *Stream


	// sendCh is used to mark a stream as ready to send, or to send a header out directly.
	sendCh chan sendReady

	// recvDoneCh is closed when recv() exits to avoid a race
	// between stream registration and stream shutdown
	recvDoneCh chan struct{}

	// shutdown is used to safely close a session
	shutdown     bool
	shutdownErr  error
	shutdownCh   chan struct{}
	shutdownLock sync.Mutex
}


// sendReady is used to either mark a stream as ready or to directly send a header
type sendReady struct {
	Hdr  []byte       	// 消息头
	Body io.Reader		// 消息体
	Err  chan error 	// 消息发送过程中出现的错误会写入到此管道中
}

// newSession is used to construct a new session
//
// 1. 初始化 session。
// 2. 计算起始的 streamID。
// 3. 开启 recv loop，从网络读取对端发来的消息，并将回复消息写到 s.sendCh 中。
// 4. 开启 send loop，从 s.sendCh 中读取消息并发送给对端。
// 5. 开启 keepalive loop，定时和对端发送 Ping 和接收 Pong。
func newSession(config *Config, conn io.ReadWriteCloser, client bool) *Session {
	logger := config.Logger
	if logger == nil {
		logger = log.New(config.LogOutput, "", log.LstdFlags)
	}
	// 1. 初始化 session
	s := &Session{
		config:     config,
		logger:     logger,
		conn:       conn,
		bufRead:    bufio.NewReader(conn),
		pings:      make(map[uint32]chan struct{}),
		streams:    make(map[uint32]*Stream),


		// inflight[] 中保存了对尚未完成连接建立的 streams ，当 stream 被关闭或者完成建立之后，会从 inflight[] 中删除它。
		inflight:   make(map[uint32]struct{}),

		//【重要】
		// synCh 和 acceptCh 大小相同且同时增减，synCh 充当了 acceptCh 的条件变量，
		// 控制同时等待被 accept 的 stream 连接请求的数目。
		//
		// 当在调用 OpenStream() 创建一个新的初始化状态的 stream 时，会往 acceptCh 写入一个 struct{}{} 对象占用一个资源；
		// 当一个 stream 尚未完成连接建立就被关闭，或者已经完成连接建立，均需要释放 acceptCh 中的一个 struct{}{} 对象，
		// 以允许创建新的 stream 连接。
		synCh:      make(chan struct{}, config.AcceptBacklog),
		acceptCh:   make(chan *Stream, config.AcceptBacklog),

		sendCh:     make(chan sendReady, 64),
		recvDoneCh: make(chan struct{}),
		shutdownCh: make(chan struct{}),
	}

	// 2. 计算 streamID 的基准值
	if client {
		// 客户端使用奇数 streamID
		s.nextStreamID = 1
	} else {
		// 服务端使用偶数 streamID
		s.nextStreamID = 2
	}

	// 3. 开启 recv loop，不断从底层网络连接 conn 中读取消息头 header，做参数检查，然后选择匹配的 handler 进行处理，
	// 在 handler 处理完后会将响应消息写入到 s.sendCh 中，等待被 `go send()` 协程发往对端。
	go s.recv()

	// 4. 开启 send loop，从 s.sendCh 中不断读取消息并发往对端
	go s.send()

	// 5. 开启 KeepAlive loop，定时和对端发送 Ping 和接收 Pong。
	if config.EnableKeepAlive {
		go s.keepalive()
	}
	return s
}

// IsClosed does a safe check to see if we have shutdown
func (s *Session) IsClosed() bool {
	select {
	case <-s.shutdownCh:
		return true
	default:
		return false
	}
}

// CloseChan returns a read-only channel which is closed as
// soon as the session is closed.
func (s *Session) CloseChan() <-chan struct{} {
	return s.shutdownCh
}

// NumStreams returns the number of currently open streams
func (s *Session) NumStreams() int {
	s.streamLock.Lock()
	num := len(s.streams)
	s.streamLock.Unlock()
	return num
}

// Open is used to create a new stream as a net.Conn
func (s *Session) Open() (net.Conn, error) {
	conn, err := s.OpenStream()
	if err != nil {
		return nil, err
	}
	return conn, nil
}



// OpenStream is used to create a new stream
//
//【重要】
// OpenStream 生成 streamID 并且创建了 yamux.Stream 对象，还发送了 windowUpdate 协议；
func (s *Session) OpenStream() (*Stream, error) {

	// 若 Session 已关闭，则不许重复打开，直接报错。
	if s.IsClosed() {
		return nil, ErrSessionShutdown
	}

	// 如果对端拒绝接收所有新的 stream 连接，则报错。
	if atomic.LoadInt32(&s.remoteGoAway) == 1 {
		return nil, ErrRemoteGoAway
	}

	// Block if we have too many inflight SYNs
	select {
	// 通过条件变量 synCh 检查 acceptCh 是否足以容纳新 stream，synCh 的缓冲大小决定了同时等待 accept 的 session 最大数目。
	case s.synCh <- struct{}{}:
	// stream 关闭检查
	case <-s.shutdownCh:
		return nil, ErrSessionShutdown
	}

// cas 获取 streamID 的重试标记
GET_ID:

	// Get an ID, and check for stream exhaustion

	// 获取当前 streamID
	id := atomic.LoadUint32(&s.nextStreamID)

	// id 耗尽？
	if id >= math.MaxUint32-1 {
		return nil, ErrStreamsExhausted
	}

	// 服务端生成的 streamID 为偶数，cas 获取
	if !atomic.CompareAndSwapUint32(&s.nextStreamID, id, id+2) {
		goto GET_ID
	}

	// Register the stream

	//【重要】根据 streamID 创建 stream 对象，其状态为 streamInit
	stream := newStream(s, id, streamInit)

	// 注册新创建的 stream 对象到 s.streams[] 中，同时保存到 s.inflight[id] 中
	s.streamLock.Lock()
	s.streams[id] = stream
	s.inflight[id] = struct{}{} // inflight[] 中保存了对尚未完成连接建立的 streams 。
	s.streamLock.Unlock()

	// Send the window update to create

	//【重要】
	// 此时 stream 对象的状态为 streamInit，在 stream.sendWindowUpdate() 会
	// 根据 streamInit 状态而返回 flag |= flagSYN 且将 Stream 流转至 streamSYNSent 状态。
	// 然后会向被连接端（ To 端）发送一个 header=[]byte{0, windowUpdate, SYN, id, delta} 消息。
	if err := stream.sendWindowUpdate(); err != nil {
		// 如果 WindowUpdate 消息发送出错，则连接无法建立，需要释放刚刚锁定的条件变量资源。
		select {
		case <-s.synCh:
		default:
			// 异常处理。
			s.logger.Printf("[ERR] yamux: aborted stream open without inflight syn semaphore")
		}
		return nil, err
	}


	return stream, nil
}



// Accept is used to block until the next available stream is ready to be accepted.
func (s *Session) Accept() (net.Conn, error) {

	// s.AcceptStream() 返回的 Stream 对象实现了 net.Conn 接口。
	conn, err := s.AcceptStream()
	if err != nil {
		return nil, err
	}

	return conn, err

}

// AcceptStream is used to block until the next available stream is ready to be accepted.
//
// 当 server 接收到 "SYN" 消息时，会调用 incomingStream() 函数为新到达的连接请求进行 Stream 对象
// 的创建、初始化、注册，然后会将 Session 对象放到 s.acceptCh 管道，这里 s.AcceptStream() 会监听
// s.acceptCh 管道，从其中获取新创建的 Init 状态 Stream，在完成一些列的参数计算和设置之后，向对端
// 发送一个 header=[]byte{0, windowUpdate, flags, streamID, delta} 消息，发送成功便返回 Session 对象。

func (s *Session) AcceptStream() (*Stream, error) {
	select {
	// 1. 从 s.acceptCh 管道中获取新建的 Init 状态 Stream 对象
	case stream := <-s.acceptCh:
		if err := stream.sendWindowUpdate(); err != nil {
			return nil, err
		}
		return stream, nil
	// 2. 监听退出信号
	case <-s.shutdownCh:
		return nil, s.shutdownErr
	}
}

// Close is used to close the session and all streams.
// Attempts to send a GoAway before closing the connection.
func (s *Session) Close() error {
	s.shutdownLock.Lock()
	defer s.shutdownLock.Unlock()

	// 如果 Session 已经关闭，就直接返回
	if s.shutdown {
		return nil
	}

	// 设置 Session 的 关闭标识 和 关闭信息
	s.shutdown = true
	if s.shutdownErr == nil {
		s.shutdownErr = ErrSessionShutdown
	}

	// 广播关闭信息，在所有数据收发的协程中，会监听此信号，收到则会退出。
	close(s.shutdownCh)

	// 关闭底层网络连接，该连接会触发网络读写报错，也能间接触发 recvLoop() 和 acceptLoop() 协程的退出。
	s.conn.Close()

	// 因为 s.shutdownCh 广播信号是异步的，需要等待其他处理 goroutine 真正退出之后，才能进行后续处理。
	// 这里 s.recvDoneCh 就是 recvLoop() 协程在退出时广播的退出信号，这里阻塞式等待该信号，收到该信号
	// 便意味着 session 上的数据接收协程已经退出，便可以进行后续的关闭处理。
	<-s.recvDoneCh

	// 关闭当前 session 下的所有 streams。
	s.streamLock.Lock()
	defer s.streamLock.Unlock()
	for _, stream := range s.streams {
		stream.forceClose()
	}
	return nil
}

// exitErr is used to handle an error that is causing the
// session to terminate.
func (s *Session) exitErr(err error) {
	s.shutdownLock.Lock()
	if s.shutdownErr == nil {
		s.shutdownErr = err
	}
	s.shutdownLock.Unlock()
	s.Close()
}

// GoAway can be used to prevent accepting further connections.
// It does not close the underlying conn.
func (s *Session) GoAway() error {
	return s.waitForSend(s.goAway(goAwayNormal), nil)
}

// goAway is used to send a goAway message
//
// 1. 设置 s.localGoAway 标识，表示本 server 拒绝接受任何新的 stream 连接。
// 2. 构造一个 GoAway 类型的 goAwayNormal 消息，知会 client 不要再创建新的 Stream。
func (s *Session) goAway(reason uint32) header {

	// 设置 s.localGoAway 标识，表示本 server 拒绝接受任何新的 stream 连接。
	atomic.SwapInt32(&s.localGoAway, 1)

	// 构造一个 GoAway 类型的 goAwayNormal 消息并返回，知会 client 不要再创建新的 Stream。
	hdr := header(make([]byte, headerSize))
	hdr.encode(typeGoAway, 0, 0, reason)
	return hdr
}

// Ping is used to measure the RTT response time
func (s *Session) Ping() (time.Duration, error) {

	// Get a channel for the ping
	ch := make(chan struct{})

	// Get a new ping id, mark as pending
	// 1.
	// (1) 递增 pingID；
	// (2) 创建用于接收 Ping ACK 信号的 ch 管道并保存到 s.pings[id] 中。
	// 备注: 当收到 Ping ACK 时 handlerPing() 函数会触发 ch 并从 s.pings[] 中删除它。
	s.pingLock.Lock()
	id := s.pingID
	s.pingID++
	s.pings[id] = ch
	s.pingLock.Unlock()


	// Send the ping request
	// 2. 构造一个 Ping SYN 消息并发送。
	hdr := header(make([]byte, headerSize))
	hdr.encode(typePing, flagSYN, 0, id)
	if err := s.waitForSend(hdr, nil); err != nil {
		return 0, err
	}

	// Wait for a response
	// 3. 阻塞式的等待 Ping ACK 回复消息或者 `超时`、`关闭` 事件。

	start := time.Now()
	select {

	// 3.1 如果收到 Ping ACK 响应，在 handlerPing() 中会 close(ch) 触发这里的监听，然后会退出 select 并走到最后的 RTT 计算的 return 语句中。
	case <-ch:
		// do nothing but break out select

	// 3.2 监听 Ping 超时事件，超时则报错，并主动从 s.pings[] 中删除 id 对应的 ch
	case <-time.After(s.config.ConnectionWriteTimeout):
		s.pingLock.Lock()
		delete(s.pings, id) // Ignore it if a response comes later.
		s.pingLock.Unlock()
		return 0, ErrTimeout

	// 3.3 监听 session 关闭
	case <-s.shutdownCh:
		return 0, ErrSessionShutdown
	}

	// Compute the RTT
	// 4. 收到 Ping ACK 响应，计算从 Ping SYN 发送到收到 Ping ACK 回复的往返耗时 RTT。
	return time.Now().Sub(start), nil
}


// keepalive is a long running goroutine that periodically does
// a ping to keep the connection alive.
//
// 长连接保持和检测
// 1. 按 s.config.KeepAliveInterval 时间间隔不断调用 s.Ping() 发送 Ping SYN 消息
// 2. 如果 s.Ping() 执行政策会返回 Ping-Pong 的 RTT 往返耗时，若出错则退出 session
func (s *Session) keepalive() {

	// 无限循环，直到遇错后 return ，来结束 keepalive goroutine。
	for {
		select {
		case <-time.After(s.config.KeepAliveInterval):
			// 1. 定时发送 Ping 消息，维持长连接
			_, err := s.Ping()
			if err != nil {
				// 2. 如果 Session 已经关闭，则报错
				if err != ErrSessionShutdown {
					s.logger.Printf("[ERR] yamux: keepalive failed: %v", err)
					s.exitErr(ErrKeepAliveTimeout)
				}
				return
			}
		case <-s.shutdownCh:
			return
		}
	}
}

// waitForSendErr waits to send a header, checking for a potential shutdown
func (s *Session) waitForSend(hdr header, body io.Reader) error {
	// 构造一个接收网络发送过程中错误信息的管道，若为出错会收到 nil 否则收到具体 error，这里实际上并未监听 errCh，没啥用。
	errCh := make(chan error, 1)
	// 阻塞式发送
	return s.waitForSendErr(hdr, body, errCh)
}

// waitForSendErr waits to send a header with optional data, checking for a potential shutdown.
// Since there's the expectation that sends can happen in a timely manner,
// we enforce the connection write timeout here.
//
// waitForSendErr() 函数阻塞式的发送消息 Msg(header + body) 给对端，网络发送的结果以 error 形式发送到 errCh 管道中，
// 调用方可以监听 errCh 获取网络发送结果，或者直接获取返回值 error 来判断整个函数的执行结果。
//
func (s *Session) waitForSendErr(hdr header, body io.Reader, errCh chan error) error {

	// 1. 从 pool 中获取 timer 并重置超时为 ConnectionWriteTimeout
	t := timerPool.Get()
	timer := t.(*time.Timer)
	timer.Reset(s.config.ConnectionWriteTimeout)

	// 如何安全的关闭 timer？
	defer func() {
		// 1. 先关闭 timer，但是关闭后可能还会有异步的定时事件到达，未避免影响后续的重用，需要等待这个事件到达后，再放回 pool 中。
		timer.Stop()
		// 2. 等待异步事件
		select {
		case <-timer.C:
		default:
		}
		// 3. 将 timer 放回 pool 中
		timerPool.Put(t)
	}()

	// 2. 构造待发送的消息 ready ，发送过程中出现的错误会写入到 errCh 中。
	ready := sendReady{Hdr: hdr, Body: body, Err: errCh}


	// 3. 消息发送
	//
	// 把消息 ready 写入到带缓冲的发送管道 s.sendCh 中，若缓冲已满则阻塞式等待，直到 `写入成功` 或者 `超时`、`流关闭` 等事件发生。
	select {
	// 把消息写入发送管道
	case s.sendCh <- ready:
	// 监听关闭信令，在任何阻塞式操作中都需要监听这个管道
	case <-s.shutdownCh:
		return ErrSessionShutdown
	// 超时事件
	case <-timer.C:
		return ErrConnectionWriteTimeout
	}

	// 4. 等待响应
	//
	// 在将消息 ready 写入到发送管道 s.sendCh 之后，在 `go send()` 的发送协程中，会取出消息 ready 并发往对端，
	// 发送过程中发生的任何错误会被写入到 errCh 中，这里监听 errCh 获取这个错误信息，如果为 nil 则发送成功，否则则出错。
	select {
	// 监听发送过程中的错误信息，阻塞式
	case err := <-errCh:
		return err
	// 监听关闭信令，在任何阻塞式操作中都需要监听这个管道
	case <-s.shutdownCh:
		return ErrSessionShutdown
	// 监听超时事件
	case <-timer.C:
		return ErrConnectionWriteTimeout
	}
}

// sendNoWait does a send without waiting. Since there's the expectation that
// the send happens right here, we enforce the connection write timeout if we
// can't queue the header to be sent.
func (s *Session) sendNoWait(hdr header) error {

	// 1. 从 pool 中获取 timer 并重置超时为 ConnectionWriteTimeout
	t := timerPool.Get()
	timer := t.(*time.Timer)
	timer.Reset(s.config.ConnectionWriteTimeout)
	// 如何安全的关闭 timer？
	defer func() {
		timer.Stop()
		select {
		case <-timer.C:
		default:
		}
		timerPool.Put(t)
	}()

	// 2. 消息发送
	//
	// 把消息 ready 写入到带缓冲的发送管道 s.sendCh 中，若缓冲已满则阻塞式等待，直到 `写入成功` 或者 `超时`、`流关闭` 等事件发生。
	select {
	case s.sendCh <- sendReady{Hdr: hdr}:
		return nil
	case <-s.shutdownCh:
		return ErrSessionShutdown
	case <-timer.C:
		return ErrConnectionWriteTimeout
	}

	// 3. 不等待网络发送响应，若出现超时或者 session 关闭则直接返回 error
}




// send is a long running goroutine that sends data
//
// 不断的从 s.sendCh 中读取消息 ready 并发往对端
func (s *Session) send() {
	for {

		select {

		// 1. 从 s.sendCh 中获取消息 ready 并发往对端
		case ready := <-s.sendCh:

			// Send a header if ready

			// 1.1 如果存在 header 就发送 header
			if ready.Hdr != nil {
				// 循环的分片发送，sent 保存每次发送的字节，用来保证完整发送完 len(ready.Hdr) 的数据
				sent := 0
				for sent < len(ready.Hdr) {
					n, err := s.conn.Write(ready.Hdr[sent:])
					if err != nil {
						s.logger.Printf("[ERR] yamux: Failed to write header: %v", err)
						// 如果发送过程中出错，就把 err 写入到 ready.Err 管道中。
						asyncSendErr(ready.Err, err)
						s.exitErr(err)
						return
					}
					sent += n
				}
			}

			// Send data from a body if given

			// 1.2 如果存在 body 就发送 body
			if ready.Body != nil {
				// 一次性发送
				_, err := io.Copy(s.conn, ready.Body)
				if err != nil {
					s.logger.Printf("[ERR] yamux: Failed to write body: %v", err)
					// 如果发送过程中出错，就把 err 写入到 ready.Err 管道中。
					asyncSendErr(ready.Err, err)
					s.exitErr(err)
					return
				}
			}

			// No error, successful send

			// 1.3 如果发送过程中没有错误发生，就写 nil 到 ready.Err 管道中。
			asyncSendErr(ready.Err, nil)

		// 2. 监听关闭信令
		case <-s.shutdownCh:
			return
		}
	}
}

// recv is a long running goroutine that accepts new data
func (s *Session) recv() {
	if err := s.recvLoop(); err != nil {
		s.exitErr(err)
	}
}

// Ensure that the index of the handler (typeData/typeWindowUpdate/etc) matches the message type
var (

	// 每种 type 都有配置指定的 handler，通过数组下标映射起来
	handlers = []func(*Session, header) error{
		typeData:         (*Session).handleStreamMessage,
		typeWindowUpdate: (*Session).handleStreamMessage,
		typePing:         (*Session).handlePing,
		typeGoAway:       (*Session).handleGoAway,
	}

)

// recvLoop continues to receive data until a fatal error is encountered

// 不断从底层网络连接 conn 中读取消息头 header，做参数检查，然后选择匹配的 handler 进行处理，
// 在 handler 处理完后会将响应消息写入到 s.sendCh 中，等待被 `go send()` 协程发往对端。
//
//
func (s *Session) recvLoop() error {

	// 若在从底层网络连接 conn 中读取消息的过程中，发生任何错误，都会调用 return 中止 For 无限循环，
	// 此时 recvLoop() 在退出前会调用 close(s.recvDoneCh) 广播退出信号，而 s.Close() 函数在监听到
	// 该信号之后，才会真正的去关闭这个 Session。
	defer close(s.recvDoneCh)

	// 回忆一下 header 的格式:
	// |<- version(1) ->|<- type(1) ->|<- flags(2) ->|<- streamID(4) ->|<- length(4) ->|

	// 1. 创建 12B 的 header buffer，用于存储每个请求开头的 12 字节 header
	hdr := header(make([]byte, headerSize)) // headerSize == 16B

	for {

		// 2. bufRead 是底层网络连接 conn 的带缓冲读封装，这里从其中读取消息 header
		if _, err := io.ReadFull(s.bufRead, hdr); err != nil {
			// 错误检查
			if err != io.EOF && !strings.Contains(err.Error(), "closed") && !strings.Contains(err.Error(), "reset by peer") {
				s.logger.Printf("[ERR] yamux: Failed to read header: %v", err)
			}
			return err
		}

		// 3. 校验 version 版本号
		if hdr.Version() != protoVersion {
			s.logger.Printf("[ERR] yamux: Invalid protocol version: %d", hdr.Version())
			return ErrInvalidVersion
		}

		// 4. 校验 type 的数值范围 [0,3]
		mt := hdr.MsgType()
		if mt < typeData || mt > typeGoAway {
			return ErrInvalidMsgType
		}

		// 5. 根据 type 获取对应的 handler ，对 header 进行处理
		if err := handlers[mt](s, hdr); err != nil {
			return err
		}

	}
}



/////////////
// 下面是具体的 header 处理函数
/////////////






// handleStreamMessage handles either a data or window update frame
//
//
func (s *Session) handleStreamMessage(hdr header) error {


	// Check for a new stream creation
	id := hdr.StreamID()
	flags := hdr.Flags()


	// 1. SYN
	// 如果收到对端的 "SYN" 连接建立消息，会新建 streamSYNReceived` 状态的 Stream 对象，
	// 并回复给对端一个 SYN ACK 消息，然后将 Stream 流转至 streamEstablished 状态。
	if flags&flagSYN == flagSYN {

		// incomingStream() 函数中会创建并初始化一个 `streamSYNReceived` 状态的 Stream 对象，
		// 然后会将 Session 对象放到 s.acceptCh 通道，它会被 s.AcceptStream() 获取并做后续处理。
		//
		// s.AcceptStream() 中通过调用 sendWindowUpdate() 函数，发现当前 Stream 处于 streamSYNReceived 状态，
		// 会回复给对端一个 SYN ACK 消息，并将 Stream 流转至 streamEstablished 状态，此时 Stream 为已连接状态。
		if err := s.incomingStream(id); err != nil {
			return err
		}
	}


	// Get the stream
	// 取出标识当前流的 Stream 对象
	s.streamLock.Lock()
	stream := s.streams[id]
	s.streamLock.Unlock()


	// If we do not have a stream, likely we sent a RST
	// 正常流程下，stream 不会为 nil，如果为 nil 意味着连接尚未建立或者已经被关闭，此时收到的任何数据都应该被丢弃。
	if stream == nil {
		// Drain any data on the wire
		// 如果消息类型是 Data 且已经收到了部分数据，则把这部分数据丢弃。
		if hdr.MsgType() == typeData && hdr.Length() > 0 {
			s.logger.Printf("[WARN] yamux: Discarding data for stream: %d", id)
			// 把 s.bufRead 里面的 hdr.Length() 字节的数据删除
			if _, err := io.CopyN(ioutil.Discard, s.bufRead, int64(hdr.Length())); err != nil {
				s.logger.Printf("[ERR] yamux: Failed to discard data: %v", err)
				return nil
			}
		} else {
			s.logger.Printf("[WARN] yamux: frame for missing stream: %v", hdr)
		}
		return nil
	}

	// Check if this is a window update

	//【重要】如果当前消息是 WindowUpdate 类型，意味着收到了控制信息，需要检查其所含的 flags
	if hdr.MsgType() == typeWindowUpdate {

		// FROM 端的 state 是在这个方法中变成 streamEstablished 的
		if err := stream.incrSendWindow(hdr, flags); err != nil {

			if sendErr := s.sendNoWait(s.goAway(goAwayProtoErr)); sendErr != nil {
				s.logger.Printf("[WARN] yamux: failed to send go away: %v", sendErr)
			}

			return err
		}
		//
		return nil
	}

	// Read the new data

	// 读取数据:
	// 1. 根据 flags 标识判断连接状态，来更新 stream 的连接状态 state。
	// 2. 如果 hdr.length 为 0，则无数据可读，直接返回
	// 3. 如果待读取的数据长度超过可用窗口大小，则直接报错
	// 4. 如果接收缓存 stream.recvBuf 为空，则初始化
	// 5. 从 s.bufRead 读取 length 个字节到 stream.recvBuf 中
	// 6. 因为新读取的数据占用了窗口空间，需要减少接收窗口的大小
	if err := stream.readData(hdr, flags, s.bufRead); err != nil {
		if sendErr := s.sendNoWait(s.goAway(goAwayProtoErr)); sendErr != nil {
			s.logger.Printf("[WARN] yamux: failed to send go away: %v", sendErr)
		}
		return err
	}
	return nil
}





// handlePing is invokde for a typePing frame

// 处理对端发来的 Ping ACK 消息
func (s *Session) handlePing(hdr header) error {


	flags := hdr.Flags()
	pingID := hdr.Length()

	// Check if this is a query, respond back in a separate context so we
	// don't interfere with the receiving thread blocking for the write.

	// 1. 如果收到 Ping 消息是 Syn 类型，则需要回复 Ping ACK
	if flags&flagSYN == flagSYN {
		// 启动后台 goroutine 发送 Ping 的 ACK 回复给对端
		go func() {
			hdr := header(make([]byte, headerSize))
			hdr.encode(typePing, flagACK, 0, pingID)
			if err := s.sendNoWait(hdr); err != nil {
				s.logger.Printf("[WARN] yamux: failed to send ping reply: %v", err)
			}
		}()

		// 直接返回 nil
		return nil
	}

	// Handle a response

	// 2. 如果收到 Ping 消息是 ACK 类型，则需要重置和清理在发送 Ping SYN 消息时设置的相关变量。
	//
	// 	2.1 获取发送 Ping SYN 消息时创建的用于接收 Ping ACK 的管道 ch
	// 	2.2 把该管道从 s.pings[] 删除
	//	2.3 关闭该管道 close(ch) 以通知阻塞在该管道的发送协程，

	s.pingLock.Lock()
	ch := s.pings[pingID] 		// 获取发送 Ping SYN 消息时创建的用于接收 Ping ACK 的管道 ch
	if ch != nil {				// 发送函数 Ping() 在超时时，会主动删除 ch，所以此时需要判断这个情况，若 ch 为 nil 则不做处理
		delete(s.pings, pingID) // 把管道从 s.pings[] 删除
		close(ch) 				// 广播关闭信号，知会发送 Ping SYN 的协程进行后续处理
	}
	s.pingLock.Unlock()
	return nil
}


// handleGoAway is invokde for a typeGoAway frame
func (s *Session) handleGoAway(hdr header) error {

	// 对于接收到对端发来的 GoAway 类型消息，其 hdr.Length() 存储了错误码，
	// 这里根据不同的错误码，执行不同的操作，以 goAwayNormal 错误码为例，它意味着
	// 对端不会再接受任何新的 stream 连接，所以这里会设置 s.remoteGoAway 标记位，
	// 在 OpenStream 时会检查该标记位，若其非 0 则会停止创建新 stream 连接，直接报错。
	// 对于其它错误码，这里知识简单的转化成标准 error 并返回，没做特殊处理。

	code := hdr.Length()
	switch code {
	case goAwayNormal:
		atomic.SwapInt32(&s.remoteGoAway, 1)
	case goAwayProtoErr:
		s.logger.Printf("[ERR] yamux: received protocol error go away")
		return fmt.Errorf("yamux protocol error")
	case goAwayInternalErr:
		s.logger.Printf("[ERR] yamux: received internal error go away")
		return fmt.Errorf("remote yamux internal error")
	default:
		s.logger.Printf("[ERR] yamux: received unexpected go away")
		return fmt.Errorf("unexpected go away received")
	}
	return nil
}



// incomingStream is used to create a new incoming stream
//
// incomingStream() 函数负责为一个新到达的连接请求进行 stream 的创建、初始化、注册
//
// 1. 如果设置了 s.localGoAway 变量，则拒绝一切新到达的连接请求，直接返回 RST
// 2. 根据 From 端发来的 streamID 创建一个 `Stream` 对象并设置状态为 `streamSYNReceived`
// 3. 检查 From 端发来的 streamID 是否已经存在，如果已经存在，则无需重新创建，报 goAwayProtoErr 错给 From 端。  ---- 感觉和第二步顺序可以换一下
// 4. 若 streamID 尚未存在，就将 `Stream` 对象注册到 s.streams[] 里
// 5. 最后，把 `Stream` 对象扔进 s.acceptCh 通道，后面会由 s.AcceptStream() 做后续请求处理。
// 6. 如果 s.acceptCh 已满，无法接受新的 stream，则拒绝当前 stream 连接并发送 RST 给 From 端。
func (s *Session) incomingStream(id uint32) error {


	// Reject immediately if we are doing a go away
	// 1. 如果设置了 s.localGoAway 变量，则拒绝一切新到达的连接请求，直接返回 RST 消息
	if atomic.LoadInt32(&s.localGoAway) == 1 {
		hdr := header(make([]byte, headerSize))
		hdr.encode(typeWindowUpdate, flagRST, id, 0)
		return s.sendNoWait(hdr)
	}

	// Allocate a new stream
	// 2. 创建一个 `Stream` 对象并设置状态为 `streamSYNReceived`
	stream := newStream(s, id, streamSYNReceived)

	s.streamLock.Lock()
	defer s.streamLock.Unlock()

	// Check if stream already exists
	//
	// 3. 检查 From 端发来的 streamID 是否已经存在，如果已经存在，则无需重新创建，报错给 From 端。
	if _, ok := s.streams[id]; ok {
		s.logger.Printf("[ERR] yamux: duplicate stream declared")
		if sendErr := s.sendNoWait(s.goAway(goAwayProtoErr)); sendErr != nil {
			s.logger.Printf("[WARN] yamux: failed to send go away: %v", sendErr)
		}
		return ErrDuplicateStream
	}

	// Register the stream
	// 4. 如果新 streamID 尚未存在，就注册到 s.streams[] 里
	s.streams[id] = stream

	// Check if we've exceeded the backlog
	select {
	// 5. 就是这里把 stream 对象扔进 acceptCh 通道，然后由 AcceptStream() 去做后续处理。
	case s.acceptCh <- stream:
		return nil
	default:

		// Backlog exceeded! RST the stream

		// 6. 如果管道 s.acceptCh 已经满了，无法接受新的 stream，则拒绝当前 stream 连接并发送 RST 给 From 端。
		s.logger.Printf("[WARN] yamux: backlog exceeded, forcing connection reset")
		delete(s.streams, id)
		stream.sendHdr.encode(typeWindowUpdate, flagRST, id, 0)
		return s.sendNoWait(stream.sendHdr)
	}
}


// closeStream is used to close a stream once both sides have issued a close.
// If there was an in-flight SYN and the stream was not yet established,
// then this will give the credit back.
func (s *Session) closeStream(id uint32) {
	s.streamLock.Lock()

	// 如果 id 存在于 s.inflight[] 中，意味着当前 stream 尚未完成连接建立，
	// 关闭一个尚未完成连接的 stream，需要释放条件变量 s.synCh 中的一个资源。
	if _, ok := s.inflight[id]; ok {
		select {
		case <-s.synCh: // 从 s.synCh 中读取一个变量，相当于释放一个资源。
		default:
			s.logger.Printf("[ERR] yamux: SYN tracking out of sync")
		}
	}

	// 从 s.streams[] 中删除 id。
	delete(s.streams, id)

	s.streamLock.Unlock()
}


// establishStream is used to mark a stream that was in the SYN Sent state as established.
func (s *Session) establishStream(id uint32) {
	s.streamLock.Lock()

	// 如果 id 存在于 s.inflight[] 中，意味着当前 stream 尚未完成连接建立，而此时该 stream 已经
	// 完成建立，故需要从 s.inflight[] 中删除 id。
	if _, ok := s.inflight[id]; ok {
		delete(s.inflight, id)
	} else {
		// 如果 id 不存在于 s.inflight[] 中，则报错。
		s.logger.Printf("[ERR] yamux: established stream without inflight SYN (no tracking entry)")
	}

	// 因为当前连接已经建立，也需要释放条件变量 s.synCh 中的资源，
	select {
	case <-s.synCh: // 从 s.synCh 中读取一个变量，相当于释放一个资源。
	default:
		s.logger.Printf("[ERR] yamux: established stream without inflight SYN (didn't have semaphore)")
	}
	s.streamLock.Unlock()
}
