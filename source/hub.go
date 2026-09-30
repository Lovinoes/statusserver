package main

import (
	"context"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// client is one websocket subscriber. send holds at most one pending
// snapshot: a slow client skips stale snapshots instead of queueing them or
// holding up anyone else.
type client struct {
	conn *websocket.Conn
	ip   string
	send chan []byte
}

func newClient(conn *websocket.Conn, ip string) *client {
	return &client{conn: conn, ip: ip, send: make(chan []byte, 1)}
}

// offer queues data, replacing any snapshot the client hasn't picked up yet.
// Callers serialize through Hub.mu, so there's only ever one producer.
func (c *client) offer(data []byte) {
	for {
		select {
		case c.send <- data:
			return
		default:
		}
		select {
		case <-c.send: // drop the stale one
		default:
		}
	}
}

// Hub tracks websocket clients, fans out snapshots, and caps connections per
// IP (optional).
type Hub struct {
	mu            sync.Mutex
	clients       map[*client]struct{}
	perIP         map[string]int
	maxConnsPerIP int
	latest        []byte // last published snapshot; sent to new clients
	closed        bool
	handlers      sync.WaitGroup // live connection handlers, for shutdown
}

func newHub(maxConnsPerIP int) *Hub {
	return &Hub{
		clients:       make(map[*client]struct{}),
		perIP:         make(map[string]int),
		maxConnsPerIP: maxConnsPerIP,
	}
}

// add registers c and queues the latest snapshot for it. It fails when the
// per-IP cap is reached or the hub is shutting down. On success the caller
// must call remove when the connection ends.
func (h *Hub) add(c *client) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || (h.maxConnsPerIP > 0 && h.perIP[c.ip] >= h.maxConnsPerIP) {
		return false
	}
	h.clients[c] = struct{}{}
	h.perIP[c.ip]++
	h.handlers.Add(1)
	if h.latest != nil {
		c.offer(h.latest)
	}
	return true
}

func (h *Hub) remove(c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.clients[c]; !ok {
		return
	}
	delete(h.clients, c)
	if h.perIP[c.ip] <= 1 {
		delete(h.perIP, c.ip)
	} else {
		h.perIP[c.ip]--
	}
	h.handlers.Done()
}

// publish records data as the latest snapshot and queues it for every
// client. It never blocks on the network.
func (h *Hub) publish(data []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.latest = data
	for c := range h.clients {
		c.offer(data)
	}
}

func (h *Hub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// close stops accepting clients and waits (up to ctx) for the connection
// handlers, which notice the shutdown themselves and send a close frame.
func (h *Hub) close(ctx context.Context) error {
	h.mu.Lock()
	h.closed = true
	h.mu.Unlock()

	done := make(chan struct{})
	go func() {
		h.handlers.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

const (
	wsWriteTimeout = 10 * time.Second
	wsPingInterval = 30 * time.Second
	wsPingTimeout  = 10 * time.Second
)

// serve pumps snapshots and keepalive pings to the client until it goes
// away or shutdown is closed. It owns all writes to the connection.
func (c *client) serve(shutdown <-chan struct{}) {
	// CloseRead answers control frames (pong, close) and cancels readCtx
	// when the peer goes away. It deliberately isn't tied to shutdown: on
	// shutdown we want to send a proper close frame first.
	readCtx := c.conn.CloseRead(context.Background())

	ping := time.NewTicker(wsPingInterval)
	defer ping.Stop()

	for {
		select {
		case <-readCtx.Done():
			c.conn.CloseNow()
			return
		case <-shutdown:
			c.conn.Close(websocket.StatusGoingAway, "server shutting down")
			return
		case data := <-c.send:
			ctx, cancel := context.WithTimeout(readCtx, wsWriteTimeout)
			err := c.conn.Write(ctx, websocket.MessageText, data)
			cancel()
			if err != nil {
				debugf("ws write to %s failed: %v", c.ip, err)
				c.conn.CloseNow()
				return
			}
		case <-ping.C:
			ctx, cancel := context.WithTimeout(readCtx, wsPingTimeout)
			err := c.conn.Ping(ctx)
			cancel()
			if err != nil {
				debugf("ws ping to %s failed: %v", c.ip, err)
				c.conn.CloseNow()
				return
			}
		}
	}
}
