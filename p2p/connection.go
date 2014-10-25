package p2p

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"net"
	"runtime/debug"
	"sync/atomic"
	"time"

	flow "code.google.com/p/mxk/go1/flowcontrol"
	"github.com/op/go-logging"
	. "github.com/tendermint/tendermint/binary"
	. "github.com/tendermint/tendermint/common"
)

const (
	numBatchPackets           = 10
	minReadBufferSize         = 1024
	minWriteBufferSize        = 1024
	flushThrottleMS           = 50
	idleTimeoutMinutes        = 5
	updateStatsSeconds        = 2
	pingTimeoutMinutes        = 2
	defaultSendRate           = 51200 // 5Kb/s
	defaultRecvRate           = 51200 // 5Kb/s
	defaultSendQueueCapacity  = 1
	defaultRecvBufferCapacity = 4096
)

type receiveCbFunc func(chId byte, msgBytes []byte)
type errorCbFunc func(interface{})

/*
A MConnection wraps a network connection and handles buffering and multiplexing.
Binary messages are sent with ".Send(channelId, msg)".
Inbound message bytes are handled with an onReceive callback function.
*/
type MConnection struct {
	conn         net.Conn
	bufReader    *bufio.Reader
	bufWriter    *bufio.Writer
	sendMonitor  *flow.Monitor
	recvMonitor  *flow.Monitor
	sendRate     int64
	recvRate     int64
	flushTimer   *ThrottleTimer // flush writes as necessary but throttled.
	send         chan struct{}
	quit         chan struct{}
	pingTimer    *RepeatTimer // send pings periodically
	pong         chan struct{}
	chStatsTimer *RepeatTimer // update channel stats periodically
	channels     []*Channel
	channelsIdx  map[byte]*Channel
	onReceive    receiveCbFunc
	onError      errorCbFunc
	started      uint32
	stopped      uint32
	errored      uint32

	LocalAddress  *NetAddress
	RemoteAddress *NetAddress
}

func NewMConnection(conn net.Conn, chDescs []*ChannelDescriptor, onReceive receiveCbFunc, onError errorCbFunc) *MConnection {

	mconn := &MConnection{
		conn:          conn,
		bufReader:     bufio.NewReaderSize(conn, minReadBufferSize),
		bufWriter:     bufio.NewWriterSize(conn, minWriteBufferSize),
		sendMonitor:   flow.New(0, 0),
		recvMonitor:   flow.New(0, 0),
		sendRate:      defaultSendRate,
		recvRate:      defaultRecvRate,
		flushTimer:    NewThrottleTimer(flushThrottleMS * time.Millisecond),
		send:          make(chan struct{}, 1),
		quit:          make(chan struct{}),
		pingTimer:     NewRepeatTimer(pingTimeoutMinutes * time.Minute),
		pong:          make(chan struct{}),
		chStatsTimer:  NewRepeatTimer(updateStatsSeconds * time.Second),
		onReceive:     onReceive,
		onError:       onError,
		LocalAddress:  NewNetAddress(conn.LocalAddr()),
		RemoteAddress: NewNetAddress(conn.RemoteAddr()),
	}

	// Create channels
	var channelsIdx = map[byte]*Channel{}
	var channels = []*Channel{}

	for _, desc := range chDescs {
		channel := newChannel(mconn, desc)
		channelsIdx[channel.id] = channel
		channels = append(channels, channel)
	}
	mconn.channels = channels
	mconn.channelsIdx = channelsIdx

	return mconn
}

// .Start() begins multiplexing packets to and from "channels".
func (c *MConnection) Start() {
	if atomic.CompareAndSwapUint32(&c.started, 0, 1) {
		log.Debug("Starting %v", c)
		go c.sendRoutine()
		go c.recvRoutine()
	}
}

func (c *MConnection) Stop() {
	if atomic.CompareAndSwapUint32(&c.stopped, 0, 1) {
		log.Debug("Stopping %v", c)
		close(c.quit)
		c.conn.Close()
		c.flushTimer.Stop()
		c.chStatsTimer.Stop()
		c.pingTimer.Stop()
		// We can't close pong safely here because
		// recvRoutine may write to it after we've stopped.
		// Though it doesn't need to get closed at all,
		// we close it @ recvRoutine.
		// close(c.pong)
	}
}

func (c *MConnection) String() string {
	return fmt.Sprintf("MConn{%v}", c.conn.RemoteAddr())
}

func (c *MConnection) flush() {
	err := c.bufWriter.Flush()
	if err != nil {
		if atomic.LoadUint32(&c.stopped) != 1 {
			log.Warning("MConnection flush failed: %v", err)
		}
	}
}

// Catch panics, usually caused by remote disconnects.
func (c *MConnection) _recover() {
	if r := recover(); r != nil {
		stack := debug.Stack()
		err := StackError{r, stack}
		c.stopForError(err)
	}
}

func (c *MConnection) stopForError(r interface{}) {
	c.Stop()
	if atomic.CompareAndSwapUint32(&c.errored, 0, 1) {
		if c.onError != nil {
			c.onError(r)
		}
	}
}

// Queues a message to be sent to channel.
func (c *MConnection) Send(chId byte, msg Binary) bool {
	if atomic.LoadUint32(&c.stopped) == 1 {
		return false
	}

	log.Debug("[%X][%v] Send: %v", chId, c, msg)

	// Send message to channel.
	channel, ok := c.channelsIdx[chId]
	if !ok {
		log.Error("Cannot send bytes, unknown channel %X", chId)
		return false
	}

	channel.sendBytes(BinaryBytes(msg))

	// Wake up sendRoutine if necessary
	select {
	case c.send <- struct{}{}:
	default:
	}

	return true
}

// Queues a message to be sent to channel.
// Nonblocking, returns true if successful.
func (c *MConnection) TrySend(chId byte, msg Binary) bool {
	if atomic.LoadUint32(&c.stopped) == 1 {
		return false
	}

	log.Debug("[%X][%v] TrySend: %v", chId, c, msg)

	// Send message to channel.
	channel, ok := c.channelsIdx[chId]
	if !ok {
		log.Error("Cannot send bytes, unknown channel %X", chId)
		return false
	}

	ok = channel.trySendBytes(BinaryBytes(msg))
	if ok {
		// Wake up sendRoutine if necessary
		select {
		case c.send <- struct{}{}:
		default:
		}
	}

	return ok
}

func (c *MConnection) CanSend(chId byte) bool {
	if atomic.LoadUint32(&c.stopped) == 1 {
		return false
	}

	channel, ok := c.channelsIdx[chId]
	if !ok {
		log.Error("Unknown channel %X", chId)
		return false
	}
	return channel.canSend()
}

// sendRoutine polls for packets to send from channels.
func (c *MConnection) sendRoutine() {
	defer c._recover()

FOR_LOOP:
	for {
		var n int64
		var err error
		select {
		case <-c.flushTimer.Ch:
			// NOTE: flushTimer.Set() must be called every time
			// something is written to .bufWriter.
			c.flush()
		case <-c.chStatsTimer.Ch:
			for _, channel := range c.channels {
				channel.updateStats()
			}
		case <-c.pingTimer.Ch:
			WriteByte(c.bufWriter, packetTypePing, &n, &err)
			c.sendMonitor.Update(int(n))
			c.flush()
		case <-c.pong:
			WriteByte(c.bufWriter, packetTypePong, &n, &err)
			c.sendMonitor.Update(int(n))
			c.flush()
		case <-c.quit:
			break FOR_LOOP
		case <-c.send:
			// Send some packets
			eof := c.sendSomePackets()
			if !eof {
				// Keep sendRoutine awake.
				select {
				case c.send <- struct{}{}:
				default:
				}
			}
		}

		if atomic.LoadUint32(&c.stopped) == 1 {
			break FOR_LOOP
		}
		if err != nil {
			log.Info("%v failed @ sendRoutine:\n%v", c, err)
			c.Stop()
			break FOR_LOOP
		}
	}

	// Cleanup
}

// Returns true if messages from channels were exhausted.
// Blocks in accordance to .sendMonitor throttling.
func (c *MConnection) sendSomePackets() bool {
	// Block until .sendMonitor says we can write.
	// Once we're ready we send more than we asked for,
	// but amortized it should even out.
	c.sendMonitor.Limit(maxPacketSize, atomic.LoadInt64(&c.sendRate), true)

	// Now send some packets.
	for i := 0; i < numBatchPackets; i++ {
		if c.sendPacket() {
			return true
		}
	}
	return false
}

// Returns true if messages from channels were exhausted.
func (c *MConnection) sendPacket() bool {
	// Choose a channel to create a packet from.
	// The chosen channel will be the one whose recentlySent/priority is the least.
	var leastRatio float32 = math.MaxFloat32
	var leastChannel *Channel
	for _, channel := range c.channels {
		// If nothing to send, skip this channel
		if !channel.isSendPending() {
			continue
		}
		// Get ratio, and keep track of lowest ratio.
		ratio := float32(channel.recentlySent) / float32(channel.priority)
		if ratio < leastRatio {
			leastRatio = ratio
			leastChannel = channel
		}
	}

	// Nothing to send?
	if leastChannel == nil {
		return true
	} else {
		// log.Debug("Found a packet to send")
	}

	// Make & send a packet from this channel
	n, err := leastChannel.writePacketTo(c.bufWriter)
	if err != nil {
		log.Warning("Failed to write packet. Error: %v", err)
		c.stopForError(err)
		return true
	}
	c.sendMonitor.Update(int(n))
	c.flushTimer.Set()
	return false
}

// recvRoutine reads packets and reconstructs the message using the channels' "recving" buffer.
// After a whole message has been assembled, it's pushed to onReceive().
// Blocks depending on how the connection is throttled.
func (c *MConnection) recvRoutine() {
	defer c._recover()

FOR_LOOP:
	for {
		// Block until .recvMonitor says we can read.
		c.recvMonitor.Limit(maxPacketSize, atomic.LoadInt64(&c.recvRate), true)

		// Read packet type
		var n int64
		var err error
		pktType := ReadByte(c.bufReader, &n, &err)
		c.recvMonitor.Update(int(n))
		if err != nil {
			if atomic.LoadUint32(&c.stopped) != 1 {
				log.Info("%v failed @ recvRoutine with err: %v", c, err)
				c.Stop()
			}
			break FOR_LOOP
		}

		// Peek into bufReader for debugging
		if log.IsEnabledFor(logging.DEBUG) {
			numBytes := c.bufReader.Buffered()
			bytes, err := c.bufReader.Peek(MinInt(numBytes, 100))
			if err != nil {
				log.Debug("recvRoutine packet type %X, peeked: %X", pktType, bytes)
			}
		}

		// Read more depending on packet type.
		switch pktType {
		case packetTypePing:
			// TODO: prevent abuse, as they cause flush()'s.
			c.pong <- struct{}{}
		case packetTypePong:
			// do nothing
		case packetTypeMessage:
			pkt, n, err := readPacketSafe(c.bufReader)
			c.recvMonitor.Update(int(n))
			if err != nil {
				if atomic.LoadUint32(&c.stopped) != 1 {
					log.Info("%v failed @ recvRoutine", c)
					c.Stop()
				}
				break FOR_LOOP
			}
			channel, ok := c.channelsIdx[pkt.ChannelId]
			if !ok || channel == nil {
				Panicf("Unknown channel %X", pkt.ChannelId)
			}
			msgBytes := channel.recvPacket(pkt)
			if msgBytes != nil {
				c.onReceive(pkt.ChannelId, msgBytes)
			}
		default:
			Panicf("Unknown message type %X", pktType)
		}

		// TODO: shouldn't this go in the sendRoutine?
		// Better to send a packet when *we* haven't sent anything for a while.
		c.pingTimer.Reset()
	}

	// Cleanup
	close(c.pong)
	for _ = range c.pong {
		// Drain
	}
}

//-----------------------------------------------------------------------------

type ChannelDescriptor struct {
	Id       byte
	Priority uint
}

// TODO: lowercase.
// NOTE: not goroutine-safe.
type Channel struct {
	conn          *MConnection
	desc          *ChannelDescriptor
	id            byte
	sendQueue     chan []byte
	sendQueueSize uint32
	recving       []byte
	sending       []byte
	priority      uint
	recentlySent  int64 // exponential moving average
}

func newChannel(conn *MConnection, desc *ChannelDescriptor) *Channel {
	if desc.Priority <= 0 {
		panic("Channel default priority must be a postive integer")
	}
	return &Channel{
		conn:      conn,
		desc:      desc,
		id:        desc.Id,
		sendQueue: make(chan []byte, defaultSendQueueCapacity),
		recving:   make([]byte, 0, defaultRecvBufferCapacity),
		priority:  desc.Priority,
	}
}

// Queues message to send to this channel.
// Goroutine-safe
func (ch *Channel) sendBytes(bytes []byte) {
	ch.sendQueue <- bytes
	atomic.AddUint32(&ch.sendQueueSize, 1)
}

// Queues message to send to this channel.
// Nonblocking, returns true if successful.
// Goroutine-safe
func (ch *Channel) trySendBytes(bytes []byte) bool {
	select {
	case ch.sendQueue <- bytes:
		atomic.AddUint32(&ch.sendQueueSize, 1)
		return true
	default:
		return false
	}
}

// Goroutine-safe
func (ch *Channel) loadSendQueueSize() (size int) {
	return int(atomic.LoadUint32(&ch.sendQueueSize))
}

// Goroutine-safe
// Use only as a heuristic.
func (ch *Channel) canSend() bool {
	return ch.loadSendQueueSize() < defaultSendQueueCapacity
}

// Returns true if any packets are pending to be sent.
// Call before calling nextPacket()
// Goroutine-safe
func (ch *Channel) isSendPending() bool {
	if len(ch.sending) == 0 {
		if len(ch.sendQueue) == 0 {
			return false
		}
		ch.sending = <-ch.sendQueue
	}
	return true
}

// Creates a new packet to send.
// Not goroutine-safe
func (ch *Channel) nextPacket() packet {
	packet := packet{}
	packet.ChannelId = byte(ch.id)
	packet.Bytes = ch.sending[:MinInt(maxPacketSize, len(ch.sending))]
	if len(ch.sending) <= maxPacketSize {
		packet.EOF = byte(0x01)
		ch.sending = nil
		atomic.AddUint32(&ch.sendQueueSize, ^uint32(0)) // decrement sendQueueSize
	} else {
		packet.EOF = byte(0x00)
		ch.sending = ch.sending[MinInt(maxPacketSize, len(ch.sending)):]
	}
	return packet
}

// Writes next packet to w.
// Not goroutine-safe
func (ch *Channel) writePacketTo(w io.Writer) (n int64, err error) {
	packet := ch.nextPacket()
	WriteByte(w, packetTypeMessage, &n, &err)
	WriteBinary(w, packet, &n, &err)
	if err != nil {
		ch.recentlySent += n
	}
	return
}

// Handles incoming packets. Returns a msg bytes if msg is complete.
// Not goroutine-safe
func (ch *Channel) recvPacket(pkt packet) []byte {
	ch.recving = append(ch.recving, pkt.Bytes...)
	if pkt.EOF == byte(0x01) {
		msgBytes := ch.recving
		ch.recving = make([]byte, 0, defaultRecvBufferCapacity)
		return msgBytes
	}
	return nil
}

// Call this periodically to update stats for throttling purposes.
// Not goroutine-safe
func (ch *Channel) updateStats() {
	// Exponential decay of stats.
	// TODO: optimize.
	ch.recentlySent = int64(float64(ch.recentlySent) * 0.5)
}

//-----------------------------------------------------------------------------

const (
	maxPacketSize     = 1024
	packetTypePing    = byte(0x00)
	packetTypePong    = byte(0x01)
	packetTypeMessage = byte(0x10)
)

// Messages in channels are chopped into smaller packets for multiplexing.
type packet struct {
	ChannelId byte
	EOF       byte // 1 means message ends here.
	Bytes     []byte
}

func (p packet) WriteTo(w io.Writer) (n int64, err error) {
	WriteByte(w, p.ChannelId, &n, &err)
	WriteByte(w, p.EOF, &n, &err)
	WriteByteSlice(w, p.Bytes, &n, &err)
	return
}

func (p packet) String() string {
	return fmt.Sprintf("Packet{%X:%X}", p.ChannelId, p.Bytes)
}

func readPacketSafe(r io.Reader) (pkt packet, n int64, err error) {
	chId := ReadByte(r, &n, &err)
	eof := ReadByte(r, &n, &err)
	bytes := ReadByteSlice(r, &n, &err)
	pkt = packet{chId, eof, bytes}
	return
}

//-----------------------------------------------------------------------------

// Convenience struct for writing typed messages.
// Reading requires a custom decoder that switches on the first type byte of a byteslice.
type TypedMessage struct {
	Type byte
	Msg  Binary
}

func (tm TypedMessage) WriteTo(w io.Writer) (n int64, err error) {
	WriteByte(w, tm.Type, &n, &err)
	WriteBinary(w, tm.Msg, &n, &err)
	return
}

func (tm TypedMessage) String() string {
	return fmt.Sprintf("TMsg{%X:%v}", tm.Type, tm.Msg)
}

func (tm TypedMessage) Bytes() []byte {
	return BinaryBytes(tm)
}
