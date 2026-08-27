package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// dialTestHub starts a test server around the hub and returns a dialer for it.
func dialTestHub(t *testing.T, h *Hub) (*httptest.Server, string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.HandleWS(w, r, []*Machine{{ID: "m1", Name: "ЧПУ-01"}})
	}))
	return srv, "ws" + strings.TrimPrefix(srv.URL, "http")
}

// TestHub_ConcurrentConnectAndBroadcast reproduces the crash where a client
// was registered before its initial snapshot was written, letting the
// broadcast goroutine and the HTTP handler write to the same connection at
// once (gorilla panics on concurrent writes). Run with -race.
func TestHub_ConcurrentConnectAndBroadcast(t *testing.T) {
	h := NewHub()
	srv, wsURL := dialTestHub(t, h)
	defer srv.Close()

	stop := make(chan struct{})
	var broadcaster sync.WaitGroup
	broadcaster.Add(1)
	go func() {
		defer broadcaster.Done()
		for {
			select {
			case <-stop:
				return
			default:
				h.Broadcast("machines", []*Machine{{ID: "m1", Name: "ЧПУ-01"}})
			}
		}
	}()

	var clients sync.WaitGroup
	for i := 0; i < 25; i++ {
		clients.Add(1)
		go func() {
			defer clients.Done()
			conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			for j := 0; j < 3; j++ {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
	}

	clients.Wait()
	close(stop)
	broadcaster.Wait()
}

// TestHub_SlowClientDoesNotStallBroadcast verifies that a client which never
// reads is dropped rather than blocking delivery to everyone else.
func TestHub_SlowClientDoesNotStallBroadcast(t *testing.T) {
	h := NewHub()
	srv, wsURL := dialTestHub(t, h)
	defer srv.Close()

	// A client that connects and then never reads.
	slow, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial slow client: %v", err)
	}
	defer slow.Close()

	fast, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial fast client: %v", err)
	}
	defer fast.Close()

	// Overflow the slow client's buffer several times over.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < sendBuffer*5; i++ {
			h.Broadcast("machines", []*Machine{{ID: "m1"}})
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Broadcast stalled on a client that never reads")
	}

	// The fast client should still be able to read what it was sent.
	_ = fast.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := fast.ReadMessage(); err != nil {
		t.Fatalf("fast client got no data: %v", err)
	}
}

// TestHub_RemoveIsIdempotent covers both pumps tearing the same client down.
func TestHub_RemoveIsIdempotent(t *testing.T) {
	h := NewHub()
	srv, wsURL := dialTestHub(t, h)
	defer srv.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	conn.Close()

	// Give both pumps time to notice and each call remove().
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.RLock()
		n := len(h.clients)
		h.mu.RUnlock()
		if n == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("client was not unregistered after the peer disconnected")
}
