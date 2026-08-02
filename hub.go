package main

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// wsMessage is the envelope pushed to clients. topic mirrors the v2
// WebSocketService.subscribe(topic) contract on the frontend.
type wsMessage struct {
	Topic   string      `json:"topic"`
	Payload interface{} `json:"payload"`
}

type Hub struct {
	mu       sync.RWMutex
	clients  map[*websocket.Conn]bool
	upgrader websocket.Upgrader
}

func NewHub() *Hub {
	return &Hub{
		clients: make(map[*websocket.Conn]bool),
		upgrader: websocket.Upgrader{
			// The API is public demo infrastructure; the browser origin
			// is already constrained by CORS on the REST side.
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
	h.mu.Lock()
	h.clients[conn] = true
	h.mu.Unlock()

	// Send an immediate snapshot so the UI paints without waiting for
	// the next tick.
	_ = conn.WriteJSON(wsMessage{Topic: "machines", Payload: initial})

	// Reader loop: we don't expect inbound frames, but reading keeps the
	// connection alive and lets us detect disconnects to clean up.
	go func() {
		defer h.remove(conn)
		conn.SetReadLimit(512)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
}

func (h *Hub) Broadcast(topic string, payload interface{}) {
	msg := wsMessage{Topic: topic, Payload: payload}
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	h.mu.RLock()
	conns := make([]*websocket.Conn, 0, len(h.clients))
	for c := range h.clients {
		conns = append(conns, c)
	}
	h.mu.RUnlock()

	for _, c := range conns {
		_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if err := c.WriteMessage(websocket.TextMessage, data); err != nil {
			h.remove(c)
		}
	}
}

func (h *Hub) remove(conn *websocket.Conn) {
	h.mu.Lock()
	if h.clients[conn] {
		delete(h.clients, conn)
		_ = conn.Close()
	}
	h.mu.Unlock()
}
