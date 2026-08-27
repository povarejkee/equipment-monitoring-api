package main

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// gorilla/websocket allows only one concurrent writer per connection, so
// every write for a client goes through its own writePump goroutine and
// reaches it via the buffered send channel. Nothing else may write to
// client.conn.
const (
	// writeWait is the deadline for a single frame write.
	writeWait = 10 * time.Second
	// pongWait is how long we'll wait for a pong before declaring the
	// connection dead; readPump extends its read deadline by this on each
	// pong.
	pongWait = 60 * time.Second
	// pingPeriod must be shorter than pongWait so a ping always lands
	// before the reader times out.
	pingPeriod = (pongWait * 9) / 10
	// sendBuffer is how many broadcasts a client may fall behind by before
	// we drop it rather than stall the broadcaster.
	sendBuffer = 16
)

// wsMessage is the envelope pushed to clients. topic mirrors the
// WebSocketService.subscribe(topic) contract on the frontend.
type wsMessage struct {
	Topic   string      `json:"topic"`
	Payload interface{} `json:"payload"`
}

type client struct {
	conn *websocket.Conn
	send chan []byte
	// closeOnce guards send against a double close when the read and
	// write pumps both finish.
	closeOnce sync.Once
}

// close unregisters nothing — it only shuts down this client's plumbing.
// The hub drops the entry separately.
func (c *client) close() {
	c.closeOnce.Do(func() {
		close(c.send)
		_ = c.conn.Close()
	})
}

type Hub struct {
	mu       sync.RWMutex
	clients  map[*client]bool
	upgrader websocket.Upgrader
}

func NewHub() *Hub {
	return &Hub{
		clients: make(map[*client]bool),
		upgrader: websocket.Upgrader{
			// The API is public demo infrastructure; the browser origin
			// is already constrained by CORS on the REST side, and /ws
			// requires a valid bearer token.
			CheckOrigin: func(r *http.Request) bool { return true },
		},
	}
}

func (h *Hub) HandleWS(w http.ResponseWriter, r *http.Request, initial []*Machine) {
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		logger.Error("ws upgrade failed", "error", err)
		return
	}
	c := &client{conn: conn, send: make(chan []byte, sendBuffer)}

	h.mu.Lock()
	h.clients[c] = true
	h.mu.Unlock()

	// Queue the initial snapshot rather than writing it here — writePump
	// owns the connection's write side.
	if data, err := json.Marshal(wsMessage{Topic: "machines", Payload: initial}); err == nil {
		c.send <- data
	}

	go h.writePump(c)
	go h.readPump(c)
}

// readPump drains inbound frames (we expect none) so the connection stays
// healthy, refreshes the read deadline on every pong, and tears the client
// down when the peer goes away.
func (h *Hub) readPump(c *client) {
	defer h.remove(c)

	c.conn.SetReadLimit(512)
	_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(pongWait))
	})

	for {
		if _, _, err := c.conn.ReadMessage(); err != nil {
			return
		}
	}
}

// writePump is the only goroutine that writes to c.conn.
func (h *Hub) writePump(c *client) {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		h.remove(c)
	}()

	for {
		select {
		case data, ok := <-c.send:
			if !ok {
				// Hub closed the channel — say goodbye politely.
				_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
				_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.TextMessage, data); err != nil {
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// Broadcast fans a payload out to every connected client. A client whose
// buffer is full is dropped instead of stalling the broadcast — with a
// tick every few seconds, a client that far behind is effectively gone.
func (h *Hub) Broadcast(topic string, payload interface{}) {
	data, err := json.Marshal(wsMessage{Topic: topic, Payload: payload})
	if err != nil {
		logger.Error("ws marshal failed", "topic", topic, "error", err)
		return
	}

	// The read lock is held across the whole fan-out: remove() closes
	// c.send under the write lock, so holding RLock here is what
	// guarantees we never send on a closed channel. Slow clients are
	// collected and removed after the lock is released, since remove()
	// needs the write lock itself.
	var slow []*client
	h.mu.RLock()
	for c := range h.clients {
		select {
		case c.send <- data:
		default:
			slow = append(slow, c)
		}
	}
	h.mu.RUnlock()

	for _, c := range slow {
		logger.Warn("ws client too slow, dropping", "topic", topic)
		h.remove(c)
	}
}

func (h *Hub) remove(c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, present := h.clients[c]; !present {
		return
	}
	delete(h.clients, c)
	// Closed under the write lock so it can't race a Broadcast send.
	c.close()
}
