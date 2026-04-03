package websocket

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	ws "nhooyr.io/websocket"
)

// Event is a message sent to connected clients over the websocket.
type Event struct {
	Type  string `json:"type"` // "agent_message" | "agent_run_update" | "tasks_updated" | "scoring_started" | "scoring_completed"
	RunID string `json:"run_id,omitempty"`
	Data  any    `json:"data"`
}

// Hub manages websocket connections and broadcasts events to all clients.
type Hub struct {
	mu      sync.RWMutex
	clients map[*client]struct{}
}

type client struct {
	conn *ws.Conn
	send chan []byte
}

// NewHub creates a new websocket hub.
func NewHub() *Hub {
	return &Hub{
		clients: make(map[*client]struct{}),
	}
}

// HandleWS is the HTTP handler for websocket upgrade requests.
func (h *Hub) HandleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := ws.Accept(w, r, &ws.AcceptOptions{
		// Allow all origins for localhost dev
		InsecureSkipVerify: true,
	})
	if err != nil {
		log.Printf("[ws] accept error: %v", err)
		return
	}

	c := &client{
		conn: conn,
		send: make(chan []byte, 64),
	}

	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()

	log.Printf("[ws] client connected (%d total)", h.clientCount())

	// Start write pump in background
	go h.writePump(c)

	// Read pump (blocks until disconnect) — we don't expect client messages,
	// but we need to read to detect close frames.
	h.readPump(c)

	// Cleanup
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
	close(c.send)
	conn.Close(ws.StatusNormalClosure, "")

	log.Printf("[ws] client disconnected (%d total)", h.clientCount())
}

// Broadcast sends an event to all connected clients.
func (h *Hub) Broadcast(evt Event) {
	data, err := json.Marshal(evt)
	if err != nil {
		log.Printf("[ws] marshal error: %v", err)
		return
	}

	h.mu.RLock()
	defer h.mu.RUnlock()

	for c := range h.clients {
		select {
		case c.send <- data:
		default:
			// Client buffer full, skip
			log.Println("[ws] dropping message for slow client")
		}
	}
}

func (h *Hub) readPump(c *client) {
	for {
		_, _, err := c.conn.Read(context.Background())
		if err != nil {
			return
		}
		// Discard any client messages — we're server-push only
	}
}

func (h *Hub) writePump(c *client) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case msg, ok := <-c.send:
			if !ok {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := c.conn.Write(ctx, ws.MessageText, msg)
			cancel()
			if err != nil {
				return
			}
		case <-ticker.C:
			// Send ping to keep connection alive
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := c.conn.Ping(ctx)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

func (h *Hub) clientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}
