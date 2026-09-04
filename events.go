package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// hubBufferSize is the per-subscriber event buffer. When a subscriber falls
// this far behind, the oldest events are dropped rather than blocking the
// request path — a slow dashboard must never stall proxying.
const hubBufferSize = 64

// Event is a live dashboard update streamed over /__inspect__/events.
// Previews are short and secret-redacted — same policy as the on-disk logs;
// never key material or full headers.
type Event struct {
	Type        string `json:"type"` // "request" | "response" | "ws"
	TS          string `json:"ts"`   // RFC3339
	Agent       string `json:"agent,omitempty"`
	API         string `json:"api,omitempty"`
	Model       string `json:"model,omitempty"`
	MappedModel string `json:"mapped_model,omitempty"`
	File        string `json:"file,omitempty"`  // log basename for dashboard lookup
	Status      string `json:"status,omitempty"` // "in_flight" | "done" | "error"
	LatencyMS   int64  `json:"latency_ms,omitempty"`
	Preview     string `json:"preview,omitempty"` // ≤200 chars, single line, redacted
	Host        string `json:"host,omitempty"`      // ws events
	Direction   string `json:"direction,omitempty"` // ws events: "request" | "response"
}

// hub fans out Events to subscribers. Broadcast sends synchronously under the
// mutex with non-blocking sends, so no background goroutine is needed.
type hub struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}
}

// NewHub returns a ready-to-use hub.
func NewHub() *hub {
	return &hub{subs: make(map[chan Event]struct{})}
}

// eventHub is the single process-wide hub feeding /__inspect__/events.
var eventHub = NewHub()

// Subscribe registers a buffered event channel and returns it with an
// unsubscribe function. The channel is never closed — after unsubscribe it is
// simply removed from the fan-out and left for the garbage collector.
func (h *hub) Subscribe() (chan Event, func()) {
	ch := make(chan Event, hubBufferSize)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	unsub := func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
	return ch, unsub
}

// Broadcast delivers evt to every subscriber, stamping TS if unset. Sends are
// non-blocking; a full subscriber loses its oldest buffered event instead of
// delaying request handling.
func (h *hub) Broadcast(evt Event) {
	if evt.TS == "" {
		evt.TS = time.Now().Format(time.RFC3339)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		for {
			select {
			case ch <- evt:
			default:
				// Buffer full: drop the oldest event and retry. All senders
				// hold the mutex, so this is guaranteed to make room.
				select {
				case <-ch:
				default:
				}
				continue
			}
			break
		}
	}
}

// eventPreview builds a ≤200-char, single-line, secret-redacted preview for
// live events (system prompt or response excerpt).
func eventPreview(s string) string {
	s = string(redactSecrets([]byte(s)))
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

// serveInspectEvents streams hub events to the dashboard over SSE. Each event
// is written as "data: {json}\n\n" with an explicit Flush; a ": ping" comment
// goes out after ~15s of silence to keep intermediaries from timing the
// connection out. Unsubscribes when the request context is cancelled (client
// closed the tab).
func serveInspectEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	events, unsubscribe := eventHub.Subscribe()
	defer unsubscribe()

	ctx := r.Context()
	keepalive := time.NewTimer(15 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case evt := <-events:
			data, err := json.Marshal(evt)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
			keepalive.Reset(15 * time.Second)
		case <-keepalive.C:
			fmt.Fprint(w, ": ping\n\n") // comment line; ignored by EventSource
			flusher.Flush()
			keepalive.Reset(15 * time.Second)
		}
	}
}
