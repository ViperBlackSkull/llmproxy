package main

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHubBroadcastReachesSubscriber(t *testing.T) {
	h := NewHub()
	ch, unsub := h.Subscribe()
	defer unsub()

	h.Broadcast(Event{Type: "request", Agent: "claude", File: "x.md"})

	select {
	case evt := <-ch:
		if evt.Type != "request" || evt.Agent != "claude" {
			t.Fatalf("unexpected event: %+v", evt)
		}
		if evt.TS == "" {
			t.Fatal("Broadcast should stamp TS when empty")
		}
	case <-time.After(time.Second):
		t.Fatal("no event received within 1s")
	}
}

func TestHubBroadcastDropsOldestOnOverflow(t *testing.T) {
	h := NewHub()
	ch, unsub := h.Subscribe()
	defer unsub()

	// Overwhelm the buffer without reading — oldest events must be dropped,
	// never block, and the newest ones must survive.
	total := hubBufferSize + 40
	for i := 0; i < total; i++ {
		h.Broadcast(Event{Type: "request", File: strings.Repeat("a", i)})
	}

	got := 0
	for {
		select {
		case <-ch:
			got++
			continue
		default:
		}
		break // channel drained
	}
	if got != hubBufferSize {
		t.Fatalf("expected %d buffered events, got %d", hubBufferSize, got)
	}
}

func TestHubUnsubscribeStopsDelivery(t *testing.T) {
	h := NewHub()
	ch, unsub := h.Subscribe()

	h.Broadcast(Event{Type: "request"})
	unsub()
	h.Broadcast(Event{Type: "request"})

	// Drain: only the pre-unsubscribe event may arrive.
	n := 0
	for {
		select {
		case <-ch:
			n++
			continue
		default:
		}
		break // channel drained
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 event before unsubscribe, got %d", n)
	}
}

func TestHubBroadcastWithNoSubscribersDoesNotBlock(t *testing.T) {
	h := NewHub()
	done := make(chan struct{})
	go func() {
		h.Broadcast(Event{Type: "request"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Broadcast with no subscribers blocked")
	}
}

func TestHubConcurrentBroadcastAndSubscribe(t *testing.T) {
	h := NewHub()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				h.Broadcast(Event{Type: "request"})
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				ch, unsub := h.Subscribe()
				h.Broadcast(Event{Type: "request"})
				// drain what we can without blocking
				for {
					select {
					case <-ch:
						continue
					default:
					}
					break
				}
				unsub()
			}
		}()
	}
	wg.Wait()
}

func TestEventPreview(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"plain", "hello world", "hello world"},
		{"collapses whitespace", "line one\nline\ttwo", "line one line two"},
		{
			"truncates to 200 chars",
			strings.Repeat("word ", 60), // fields-joined: 299 chars; first 200 = "word "×40
			strings.Repeat("word ", 40) + "...",
		},
		{
			"redacts api keys",
			"key sk-ant-api03-" + strings.Repeat("A", 40) + " here",
			"key <REDACTED> here",
		},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := eventPreview(tc.in); got != tc.want {
				t.Fatalf("eventPreview(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestInspectEventsRequiresToken(t *testing.T) {
	t.Setenv("LLMPROXY_INSPECT_TOKEN", "devtoken")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleInspect(&Config{LogDir: t.TempDir()}, w, r)
	}))
	defer srv.Close()

	for _, url := range []string{
		srv.URL + "/__inspect__/events",             // no token
		srv.URL + "/__inspect__/events?token=wrong", // bad token
	} {
		resp, err := http.Get(url)
		if err != nil {
			t.Fatalf("GET %s: %v", url, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("GET %s: status %d, want 401", url, resp.StatusCode)
		}
	}
}

func TestInspectEventsDisabledWithoutToken(t *testing.T) {
	t.Setenv("LLMPROXY_INSPECT_TOKEN", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleInspect(&Config{LogDir: t.TempDir()}, w, r)
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/__inspect__/events?token=anything")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d, want 404 (dashboard disabled)", resp.StatusCode)
	}
}

func TestInspectEventsStreamsBroadcast(t *testing.T) {
	t.Setenv("LLMPROXY_INSPECT_TOKEN", "devtoken")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleInspect(&Config{LogDir: t.TempDir()}, w, r)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET",
		srv.URL+"/__inspect__/events?token=devtoken", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type %q, want text/event-stream", ct)
	}

	// Give the handler a moment to subscribe, then broadcast repeatedly.
	go func() {
		for i := 0; i < 20; i++ {
			time.Sleep(50 * time.Millisecond)
			eventHub.Broadcast(Event{Type: "request", Agent: "claude", File: "x.md", Status: "in_flight"})
		}
	}()

	line := make(chan string, 1)
	go func() {
		reader := bufio.NewReader(resp.Body)
		for {
			s, err := reader.ReadString('\n')
			if err != nil {
				line <- ""
				return
			}
			if strings.HasPrefix(s, "data: ") {
				line <- s
				return
			}
		}
	}()

	select {
	case s := <-line:
		if s == "" {
			t.Fatal("stream closed before any data line")
		}
		if !strings.Contains(s, `"type":"request"`) || !strings.Contains(s, `"status":"in_flight"`) {
			t.Fatalf("unexpected data line: %s", s)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("no data line received within 4s")
	}
}
