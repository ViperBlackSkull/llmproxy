# Phase 02: Live Real-Time Inspect Dashboard

Today the `/__inspect__` dashboard is a static server-rendered page: you see requests only after reloading. This phase makes it live — new requests stream into the page via Server-Sent Events the moment they hit the proxy, show an in-flight indicator while the model is generating, and flip to "done" with latency when the response lands. While an agent works, you watch its traffic arrive in real time. All work happens on the `dev` branch and builds on the Phase 01 credential-free proxy.

**Context you need:** the dashboard is `web/inspect.html`, embedded into the Go binary via `//go:embed` (changes require a rebuild to take effect — forgetting this is the classic trap). The daemon already centralizes request handling in `handleProxy` (which writes logs via `writeRequestLog` and finishes via `proxyAndCapture` + `writeMetadata`), and auth for inspect endpoints lives in `checkInspectAuth`, which accepts `?token=` query params — essential because `EventSource` cannot set headers. Search for these symbols before adding anything new.

## Tasks

- [x] Create an in-process event hub (new file `events.go`, same `main` package):
  - A `hub` struct with a mutex-protected set of subscriber channels, plus `Subscribe() (chan Event, func())`, and `Broadcast(evt Event)`; subscriber channels are buffered (~64) and drop the oldest event on overflow so a slow dashboard never blocks request handling
  - An `Event` struct marshaling to JSON with fields: `type` (`request` | `response` | `ws`), `ts` (RFC3339), `agent`, `api`, `model`, `mapped_model`, `file` (log basename for dashboard lookup), `status` (`in_flight` | `done` | `error`), `latency_ms`, and `preview` (≤200 chars of system prompt or response)
  - Instantiate one global hub in `main()` — no goroutine needed if `Broadcast` sends synchronously under the mutex with non-blocking sends

  *Done 2026-09-03.* `events.go` + `events_test.go` (6 tests, race-detector clean). One deviation: the global `eventHub` is a package-level `var eventHub = NewHub()` instead of being constructed inside `main()` — same single global hub, no goroutine, but tests can exercise handlers without running `main()`. Also added `host`/`direction` fields (omitempty) for ws events per the ws task, and `eventPreview()` which redacts via `redactSecrets` BEFORE truncating so a truncated secret prefix can never leak.

- [x] Publish events from the request path in `main.go`:
  - In `handleProxy`, after `writeRequestLog`: broadcast a `request` event with `status: in_flight` carrying agent, api, model, file, and preview
  - After `proxyAndCapture` + `writeMetadata`: broadcast a `response` event for the same file with `status: done` (or `error` when the upstream returned ≥400), `latency_ms`, and a response preview
  - From the WebSocket logging path (`saveWSTraffic`/`logFrame`): broadcast a `ws` event with host and direction so Codex realtime traffic is visible live too
  - Never include key material or full headers in any event — previews only, same policy as the logs

  *Done 2026-09-03.* `proxyAndCapture` now returns the captured response text (single caller updated) so the `response` event carries a real preview; the no-flusher fallback path also captures via `MultiWriter`. Added `broadcastError` on the two early-return failure paths (request-build error, backend round-trip error) so a failed request's entry flips to `error` instead of hanging in-flight forever. Request preview falls back to the first user message when there is no system prompt.

- [x] Add the SSE endpoint `/__inspect__/events` in `handleInspect` (route it before the generic dashboard handler):
  - Authenticate with the existing `checkInspectAuth` (the `?token=` path makes `EventSource` work with a URL like `/__inspect__/events?token=devtoken`)
  - Set `Content-Type: text/event-stream`, `Cache-Control: no-cache`, disable implicit flushing; write each hub event as `data: {json}\n\n` followed by an explicit `Flush()`
  - Register a subscriber on connect, send a keepalive comment (`: ping\n\n`) every ~15 seconds of silence, and unsubscribe cleanly when the request context is cancelled (client closes the tab)
  - Verify with `curl -N "http://localhost:8765/__inspect__/events?token=devtoken"` — it must hang open and print events as requests flow (and must 404/401 without a valid token)

  *Done 2026-09-03.* `serveInspectEvents` in `events.go`, routed first in `handleInspect` after auth. Keepalive uses a 15s `time.Timer` reset on every event, so pings only fire during silence. Verified live: `curl -N` hung open and printed `request`/`response` frames plus `: ping`; missing/bad token → 401, unset token → 404 (also covered by unit tests `TestInspectEvents*`).

- [x] Make the dashboard (`web/inspect.html`) subscribe and render live entries:
  - Add JS that opens an `EventSource` using the token from `location.search` (the page is always loaded with `?token=...`)
  - On a `request` event: build an entry node with the exact same markup/classes as the server-rendered ones (agent badge, api badge, model, timestamp, preview) and prepend it to the list — reuse the existing `toggleEntry` lazy-load by setting `data-file`/`data-agent` so click-to-expand keeps working
  - On a `response` event: find the entry by `data-file` and update it — replace the in-flight indicator with the status and latency badge, refresh the preview
  - On a `ws` event: prepend a compact entry labeled with the host and direction that expands to the raw JSON via the existing `/__inspect__/api/` fetch
  - Add a connection status pill fixed near the heading: `● Live` (green) when the EventSource is open, `○ Reconnecting…` (amber) on error — `EventSource` auto-reconnects, so just reflect state

  *Done 2026-09-03.* Entries are built with DOM APIs (no `innerHTML`, so previews can't inject markup) and mirror the server template exactly, including the tabs/detail pane. Response events that arrive with no matching entry (page loaded mid-flight) materialize a completed entry instead of being dropped. The model span updates to `orig → mapped` when a remap lands. **Fixed a pre-existing bug this surfaced:** `toggleEntry`'s lazy-load fetch never carried the `?token=`, so click-to-expand 401'd on any token-loaded page — even before this phase. It now appends `location.search`. Also made `showTab` stop propagation so tab clicks no longer collapse the entry (same pre-existing issue). ws entries reuse `/__inspect__/api/` — the endpoint's `markdown` field already serves the ws `.json` log verbatim.

- [x] Style in-flight vs completed states:
  - CSS in the same file: in-flight entries get a pulsing left border or spinner dot; done entries show a latency badge (e.g. `812ms`) colored green under 2s, amber under 10s, red above; error entries get a red status badge
  - Keep the existing dark theme variables — match the current palette (`#e94560` accents, `#16213e` cards) rather than introducing new colors

  *Done 2026-09-03.* In-flight = amber pulsing left border + pulsing dot (`#ffeaa7`); latency badge green `#00b894` <2s, amber `#ffeaa7` <10s, red `#e94560` ≥10s; error = red badge; ws entries get a teal left border + `badge-ws` (`#00cec9`, already in the palette). No new hues introduced.

- [x] Verify the live flow end-to-end and commit:
  - Rebuild (`make build` — the embedded HTML changed), start `./llmproxy serve` with `LLMPROXY_INSPECT_TOKEN=devtoken`
  - Open `http://localhost:8765/__inspect__?token=devtoken` in a browser if one is available (otherwise verify via the SSE curl from the earlier task) — confirm the `● Live` pill shows
  - Fire one plain and one streaming request (`"stream": true`) at `/v1/messages` with no auth (Phase 01 flow): entries must appear WITHOUT a page reload; the streaming one must show in-flight then flip to done with latency when it finishes
  - Kill the daemon and confirm the pill flips to `○ Reconnecting…` (no JS console errors)
  - Run `go vet ./...`, rebuild clean, and commit on `dev` with a message like `Live dashboard: event hub, SSE /__inspect__/events, streaming entries with in-flight status`

  *Done 2026-09-03.* Ran the daemon from the repo root (loads `.env` → z.ai backend + `MODEL_MAP`), scratch log dir, `LLMPROXY_INSPECT_TOKEN=devtoken`. Browser-verified with Playwright: `● Live` pill; plain + streaming requests appeared **without reload** (in-flight spinner caught mid-generation, then flipped to done with `848ms`/`1586ms` badges and `claude-3-5-haiku → glm-5.3` remap shown); click-to-expand + Raw JSON tab work on live entries; killing the daemon flips the pill to `○ Reconnecting…` with zero JS exceptions (only the browser's own `ERR_CONNECTION_REFUSED` network logs from EventSource auto-reconnect, which is expected). One note: glm-5.3 answered the streaming test with thinking-only deltas, and `extractStreamedText` has never captured `thinking_delta`, so that response event had no preview (empty `.resp.txt` too) — pre-existing, harmless; the entry keeps its request preview. `go vet ./...` clean, full test suite green, committed on `dev` and pushed.
