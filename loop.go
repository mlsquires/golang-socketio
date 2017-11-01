package gosocketio

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/mtfelian/golang-socketio/logging"
	"github.com/mtfelian/golang-socketio/protocol"
	"github.com/mtfelian/golang-socketio/transport"
)

const (
	queueBufferSize = 500
)

var (
	ErrorWrongHeader = errors.New("Wrong header")
)

// engine.io header to send or receive
type Header struct {
	Sid          string   `json:"sid"`
	Upgrades     []string `json:"upgrades"`
	PingInterval int      `json:"pingInterval"`
	PingTimeout  int      `json:"pingTimeout"`
}

// socket.io connection handler
// use IsAlive to check that handler is still working
// use Dial to connect to websocket
// use In and Out channels for message exchange
// Close message means channel is closed
// ping is automatic
type Channel struct {
	conn transport.Connection

	out      chan string
	stub     chan string
	upgraded chan string
	header   Header

	alive     bool
	aliveLock sync.Mutex

	ack ackProcessor

	server        *Server
	ip            string
	requestHeader http.Header
}

// create channel, map, and set active
func (c *Channel) initChannel() {
	//TODO: queueBufferSize from constant to server or client variable
	c.out = make(chan string, queueBufferSize)
	c.stub = make(chan string)
	c.upgraded = make(chan string)
	c.ack.resultWaiters = make(map[int](chan string))
	c.alive = true
}

// Get id of current socket connection
func (c *Channel) Id() string {
	return c.header.Sid
}

// Checks that Channel is still alive
func (c *Channel) IsAlive() bool {
	c.aliveLock.Lock()
	defer c.aliveLock.Unlock()
	return c.alive
}

// Close закрывает соединение для клиента (канала)
func (c *Channel) Close() error {
	return CloseChannel(c, &c.server.methods, nil)
}

// Close закрывает соединение для поллинга (канала) при апгрейде
func (c *Channel) Stub() error {
	return StubChannel(c, &c.server.methods, nil)
}

// Close channel
func CloseChannel(c *Channel, m *methods, args ...interface{}) error {
	switch c.conn.(type) {
	case *transport.PollingConnection:
		logging.Log().Debug("close channel type: PollingConnection")
	case *transport.WebsocketConnection:
		logging.Log().Debug("close channel type: WebsocketConnection")
	}
	c.aliveLock.Lock()
	defer c.aliveLock.Unlock()
	if !c.alive {
		//already closed
		return nil
	}
	c.conn.Close()
	c.alive = false
	//clean outloop
	for len(c.out) > 0 {
		<-c.out
	}
	c.out <- protocol.CloseMessage
	m.callLoopEvent(c, OnDisconnection)
	overfloodedLock.Lock()
	delete(overflooded, c)
	overfloodedLock.Unlock()
	return nil
}

// Stub channel when upgrading transport
func StubChannel(c *Channel, m *methods, args ...interface{}) error {
	switch c.conn.(type) {
	case *transport.PollingConnection:
		logging.Log().Debug("Stub channel type: PollingConnection")
	case *transport.WebsocketConnection:
		logging.Log().Debug("Stub channel type: WebsocketConnection")
	}
	c.aliveLock.Lock()
	defer c.aliveLock.Unlock()
	if !c.alive {
		//already closed
		return nil
	}
	c.conn.Close()
	c.alive = false

	//clean outloop
	for len(c.out) > 0 {
		<-c.out
	}
	c.out <- protocol.StubMessage
	overfloodedLock.Lock()
	delete(overflooded, c)
	overfloodedLock.Unlock()
	return nil
}

//incoming messages loop, puts incoming messages to In channel
func inLoop(c *Channel, m *methods) error {
	for {
		pkg, err := c.conn.GetMessage()
		if err != nil {
			logging.Log().Debug("c.conn.GetMessage err ", err, " pkg: ", pkg)
			return CloseChannel(c, m, err)
		}

		if pkg == transport.StopMessage {
			logging.Log().Debug("inLoop StopMessage")
			return nil
		}

		msg, err := protocol.Decode(pkg)

		if err != nil {
			logging.Log().Debug("Decoding err: ", err)
			CloseChannel(c, m, protocol.ErrorWrongPacket)
			return err
		}

		switch msg.Type {
		case protocol.MessageTypeOpen:
			logging.Log().Debug("protocol.MessageTypeOpen: ", msg)
			if err := json.Unmarshal([]byte(msg.Source[1:]), &c.header); err != nil {
				CloseChannel(c, m, ErrorWrongHeader)
			}
			m.callLoopEvent(c, OnConnection)
		case protocol.MessageTypePing:
			logging.Log().Debug("get MessageTypePing ", msg, " source ", msg.Source)
			if msg.Source == "2probe" {
				logging.Log().Debug("get 2probe")
				c.out <- "3probe"
				c.upgraded <- transport.UpgradedMessage
			} else {
				c.out <- protocol.PongMessage
			}
		case protocol.MessageTypeUpgrade:
		case protocol.MessageTypePong:
		default:
			go m.processIncomingMessage(c, msg)
		}
	}
	return nil
}

var overflooded map[*Channel]struct{} = make(map[*Channel]struct{})
var overfloodedLock sync.Mutex

func AmountOfOverflooded() int64 {
	overfloodedLock.Lock()
	defer overfloodedLock.Unlock()

	return int64(len(overflooded))
}

// outgoing messages loop, sends messages from channel to socket
func outLoop(c *Channel, m *methods) error {
	for {
		outBufferLen := len(c.out)
		logging.Log().Debug("outBufferLen: ", outBufferLen)
		if outBufferLen >= queueBufferSize-1 {
			logging.Log().Debug("outBufferLen >= queueBufferSize-1")
			return CloseChannel(c, m, ErrorSocketOverflood)
		} else if outBufferLen > int(queueBufferSize/2) {
			overfloodedLock.Lock()
			overflooded[c] = struct{}{}
			overfloodedLock.Unlock()
		} else {
			overfloodedLock.Lock()
			delete(overflooded, c)
			overfloodedLock.Unlock()
		}

		msg := <-c.out

		if msg == protocol.CloseMessage {
			return nil
		}
		if msg == protocol.StubMessage {
			return nil
		}
		err := c.conn.WriteMessage(msg)
		if err != nil {
			return CloseChannel(c, m, err)
		}
	}
	return nil
}

// Pinger sends ping messages for keeping connection alive
func pinger(c *Channel) {
	for {
		interval, _ := c.conn.PingParams()
		time.Sleep(interval)
		if !c.IsAlive() {
			return
		}

		c.out <- protocol.PingMessage
	}
}

// Pauses for send http requests
func pollingClientListener(c *Channel, m *methods) {
	time.Sleep(1 * time.Second)
	m.callLoopEvent(c, OnConnection)
}
