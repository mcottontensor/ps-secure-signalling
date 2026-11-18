// Package wsconn wraps a websocket in the resource limits every signalling connection needs: a read
// size limit, an inbound rate limit, a bounded outbound queue so one slow peer can never stall
// another, keepalive pings, and a close that flushes what was queued before it.
package wsconn

import (
	"context"
	"errors"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
	"golang.org/x/time/rate"

	"github.com/epicgames-ps/ps-secure-signalling/pkg/protocol"
)

// Close codes used by this project, beyond the standard ones in websocket.StatusCode.
const (
	StatusUnauthorized websocket.StatusCode = 4001 // credentials did not allow what was attempted
	StatusReplaced     websocket.StatusCode = 4002 // another connection took over this identity
	StatusExpired      websocket.StatusCode = 4003 // the credential the connection was opened with expired
)

// Options are the per-connection limits. Zero values take the defaults below.
type Options struct {
	MaxMessageBytes   int64         // largest frame accepted (default 256 KiB)
	SendQueue         int           // outbound messages buffered before the peer is dropped (default 256)
	MessagesPerSecond float64       // sustained inbound rate; faster peers are slowed down (default 50)
	Burst             int           // inbound burst allowance (default 200)
	MaxInvalid        int           // undecodable messages tolerated before closing (default 20)
	PingInterval      time.Duration // websocket ping interval (default 20s, negative disables)
	PongTimeout       time.Duration // time allowed for the pong (default 15s)
	WriteTimeout      time.Duration // time allowed for one write (default 10s)
}

func (o *Options) defaults() {
	def := func(v *int, d int) {
		if *v <= 0 {
			*v = d
		}
	}
	if o.MaxMessageBytes <= 0 {
		o.MaxMessageBytes = 256 << 10
	}
	def(&o.SendQueue, 256)
	def(&o.Burst, 200)
	def(&o.MaxInvalid, 20)
	if o.MessagesPerSecond <= 0 {
		o.MessagesPerSecond = 50
	}
	if o.PingInterval == 0 {
		o.PingInterval = 20 * time.Second
	}
	if o.PongTimeout <= 0 {
		o.PongTimeout = 15 * time.Second
	}
	if o.WriteTimeout <= 0 {
		o.WriteTimeout = 10 * time.Second
	}
}

// Conn is a signalling connection. Send and Close are safe to call from any goroutine.
type Conn struct {
	ws      *websocket.Conn
	opts    Options
	send    chan []byte
	closing chan struct{}
	done    chan struct{}

	closeOnce   sync.Once
	closeCode   websocket.StatusCode
	closeReason string
}

// New wraps an accepted or dialled websocket. Call Run to start it.
func New(ws *websocket.Conn, opts Options) *Conn {
	opts.defaults()
	ws.SetReadLimit(opts.MaxMessageBytes)
	return &Conn{
		ws:      ws,
		opts:    opts,
		send:    make(chan []byte, opts.SendQueue),
		closing: make(chan struct{}),
		done:    make(chan struct{}),
	}
}

// Send queues a message. It never blocks: a peer that lets its queue fill is disconnected, because
// waiting on it would let one slow connection hold up whoever is sending to it.
func (c *Conn) Send(m protocol.Message) bool {
	data, err := protocol.Encode(m)
	if err != nil {
		return false
	}
	select {
	case <-c.closing:
		return false
	default:
	}
	select {
	case c.send <- data:
		return true
	default:
		c.Close(websocket.StatusTryAgainLater, "send queue full")
		return false
	}
}

// Close sends what is already queued, then closes with the given code and reason.
func (c *Conn) Close(code websocket.StatusCode, reason string) {
	c.closeOnce.Do(func() {
		for len(reason) > 123 { // the most a close frame can carry, cut on a character boundary
			_, size := utf8.DecodeLastRuneInString(reason)
			reason = reason[:len(reason)-size]
		}
		c.closeCode, c.closeReason = code, reason
		close(c.closing)
	})
}

// Done is closed once the connection has fully shut down.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Run reads messages until the connection ends, handing each decoded message to onMessage on the
// calling goroutine, so handlers for one connection never run concurrently. onInvalid, if set, is
// told about each frame that fails to decode. Run returns once the connection is closed.
func (c *Conn) Run(ctx context.Context, onMessage func(protocol.Message), onInvalid func(error)) {
	ctx, cancel := context.WithCancel(ctx)
	defer func() {
		cancel()
		c.Close(websocket.StatusNormalClosure, "")
		<-c.done
	}()
	go c.writeLoop(ctx, cancel)
	if c.opts.PingInterval > 0 {
		go c.pingLoop(ctx)
	}

	limiter := rate.NewLimiter(rate.Limit(c.opts.MessagesPerSecond), c.opts.Burst)
	invalid := 0
	for {
		typ, data, err := c.ws.Read(ctx)
		if err != nil {
			if websocket.CloseStatus(err) == websocket.StatusMessageTooBig || errors.Is(err, websocket.ErrMessageTooBig) {
				c.Close(websocket.StatusMessageTooBig, "message too big")
			}
			return
		}
		// Throttle rather than drop: a fast peer is slowed by TCP backpressure, not punished.
		if err := limiter.Wait(ctx); err != nil {
			return
		}
		var m protocol.Message
		// Once closing, nothing more from this peer is acted on: a replaced or expired
		// connection must not keep relaying while its close handshake completes.
		select {
		case <-c.closing:
			return
		default:
		}
		if typ != websocket.MessageText {
			err = protocol.ErrNotObject
		} else {
			m, err = protocol.Decode(data)
		}
		if err != nil {
			invalid++
			if onInvalid != nil {
				onInvalid(err)
			}
			if invalid > c.opts.MaxInvalid {
				c.Close(websocket.StatusPolicyViolation, "too many invalid messages")
				return
			}
			continue
		}
		onMessage(m)
	}
}

func (c *Conn) writeLoop(ctx context.Context, cancel context.CancelFunc) {
	defer close(c.done)
	defer cancel()
	write := func(data []byte) bool {
		wctx, wcancel := context.WithTimeout(context.Background(), c.opts.WriteTimeout)
		defer wcancel()
		return c.ws.Write(wctx, websocket.MessageText, data) == nil
	}
	for {
		select {
		case data := <-c.send:
			if !write(data) {
				c.ws.CloseNow()
				return
			}
		case <-ctx.Done():
			// The reader stopped: the peer went away. Nothing more can be delivered.
			c.ws.CloseNow()
			return
		case <-c.closing:
			for len(c.send) > 0 {
				if !write(<-c.send) {
					c.ws.CloseNow()
					return
				}
			}
			_ = c.ws.Close(c.closeCode, c.closeReason)
			return
		}
	}
}

func (c *Conn) pingLoop(ctx context.Context) {
	t := time.NewTicker(c.opts.PingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, cancel := context.WithTimeout(ctx, c.opts.PongTimeout)
			err := c.ws.Ping(pctx)
			cancel()
			if err != nil && ctx.Err() == nil {
				// A dead peer never completes a close handshake, so do not wait for one.
				c.ws.CloseNow()
				return
			}
		}
	}
}
