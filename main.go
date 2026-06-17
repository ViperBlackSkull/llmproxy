package main

import (
	"bufio"
	"bytes"
	"compress/flate"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"embed"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed web/inspect.html
var webFS embed.FS

type Config struct {
	ListenAddr  string
	BackendURL  string // global override for all requests
	LogDir      string
	AgentType   string
	MitmEnabled bool
	ForwardURL  string
	StripPrompt bool
	Transparent bool // forward to original destination by default

	// Per-provider backend URLs (used for BASE_URL redirect or explicit overrides)
	Backends map[string]string // "anthropic"|"openai"|"gemini" -> upstream URL

	// Per-provider API keys — only injected when Transparent=false or explicitly overridden
	APIKeys map[string]string // "anthropic"|"openai"|"gemini" -> key

	// MITM state
	caCert    *x509.Certificate
	caKey     *ecdsa.PrivateKey
	caCertPEM []byte
	certCache sync.Map
}

func resolveBackend(overrideEnv, baseURLEnv, fallback string) string {
	if v := strings.TrimRight(os.Getenv(overrideEnv), "/"); v != "" {
		return v
	}
	if v := strings.TrimRight(os.Getenv(baseURLEnv), "/"); v != "" {
		return v
	}
	return strings.TrimRight(fallback, "/")
}

func isSelfReference(backendHost, listenAddr string) bool {
	host := backendHost
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	selfRefs := []string{"127.0.0.1", "localhost", "0.0.0.0", "::1"}
	for _, ref := range selfRefs {
		if host == ref {
			if _, port, err := net.SplitHostPort(backendHost); err == nil {
				if _, lport, err := net.SplitHostPort(listenAddr); err == nil && port == lport {
					return true
				}
			}
		}
	}
	return false
}

func (c *Config) backendForAPI(apiType string) string {
	if c.BackendURL != "" {
		return c.BackendURL
	}
	if url, ok := c.Backends[apiType]; ok {
		return url
	}
	if url, ok := c.Backends["anthropic"]; ok {
		return url
	}
	return "https://api.anthropic.com"
}

// resolveTarget determines the upstream URL for a request.
// In transparent mode, it forwards to the original destination from the request Host.
// When the request targets the proxy itself (BASE_URL redirect), it uses the Backends
// map to find the real upstream. Explicit overrides (BACKEND_URL, *_BACKEND env vars)
// always take precedence.
func (c *Config) resolveTarget(r *http.Request, apiType string) string {
	// Global override always wins
	if c.BackendURL != "" {
		return c.BackendURL
	}

	// Per-provider explicit override (user set *_BACKEND env var)
	envKey := strings.ToUpper(apiType) + "_BACKEND"
	if _, explicitlySet := os.LookupEnv(envKey); explicitlySet {
		if url, ok := c.Backends[apiType]; ok && url != "" {
			return url
		}
	}

	// Transparent mode: forward to original destination
	if c.Transparent {
		host := r.Host
		if host == "" {
			host = r.URL.Host
		}

		// If the agent pointed its BASE_URL at us, use Backends map for routing
		if isSelfReference(host, c.ListenAddr) {
			if url, ok := c.Backends[apiType]; ok {
				return url
			}
			if url, ok := c.Backends["anthropic"]; ok {
				return url
			}
			return "https://api.anthropic.com"
		}

		// Request to external host — forward as-is
		return "https://" + host
	}

	// Non-transparent: use Backends map (legacy behavior)
	return c.backendForAPI(apiType)
}

// injectAPIKey replaces auth headers with the proxy's stored key for the given provider.
// In transparent mode, the agent's own auth headers pass through — no injection.
func (c *Config) injectAPIKey(req *http.Request, apiType string) {
	if c.Transparent {
		return
	}
	key, ok := c.APIKeys[apiType]
	if !ok || key == "" {
		return
	}
	switch apiType {
	case "anthropic":
		req.Header.Set("x-api-key", key)
	case "openai":
		req.Header.Set("Authorization", "Bearer "+key)
	case "gemini":
		req.Header.Set("x-goog-api-key", key)
		// Also inject into query param if not present
		if req.URL.Query().Get("key") == "" {
			q := req.URL.Query()
			q.Set("key", key)
			req.URL.RawQuery = q.Encode()
		}
	}
}

func main() {
	quiet := flag.Bool("q", false, "quiet mode — suppress all console output")
	flag.Parse()
	if *quiet {
		log.SetOutput(io.Discard)
	}

	// BACKEND_URL is a global override — if set, ALL requests go there.
	// Per-provider env vars: ANTHROPIC_BACKEND, OPENAI_BACKEND, GEMINI_BACKEND.
	// These are separate from the client-facing ANTHROPIC_BASE_URL etc.
	backendURL := strings.TrimRight(getEnv("BACKEND_URL", ""), "/")

	cfg := Config{
		ListenAddr: getEnv("LISTEN_ADDR", ":8765"),
		BackendURL: backendURL,
		LogDir:     getEnv("LOG_DIR", "./logs"),
		AgentType:  getEnv("AGENT_TYPE", "unknown"),
		ForwardURL: getEnv("FORWARD_URL", ""),
		StripPrompt: getEnv("STRIP_PROMPT", "") == "true" || getEnv("STRIP_PROMPT", "") == "1",
		Transparent: getEnv("TRANSPARENT", "true") == "true",
		Backends: map[string]string{
			"anthropic": resolveBackend("ANTHROPIC_BACKEND", "ANTHROPIC_BASE_URL", "https://api.anthropic.com"),
			"openai":    resolveBackend("OPENAI_BACKEND", "OPENAI_BASE_URL", "https://api.openai.com"),
			"gemini":    resolveBackend("GEMINI_BACKEND", "GEMINI_BASE_URL", "https://generativelanguage.googleapis.com"),
		},
		APIKeys: map[string]string{
			"anthropic": os.Getenv("ANTHROPIC_API_KEY"),
			"openai":    os.Getenv("OPENAI_API_KEY"),
			"gemini":    os.Getenv("GEMINI_API_KEY"),
		},
	}

	// Detect self-referential backend URLs (would cause infinite loop)
	for provider, backend := range cfg.Backends {
		u, err := url.Parse(backend)
		if err == nil && isSelfReference(u.Host, cfg.ListenAddr) {
			defaults := map[string]string{
				"anthropic": "https://api.anthropic.com",
				"openai":    "https://api.openai.com",
				"gemini":    "https://generativelanguage.googleapis.com",
			}
			log.Printf("WARNING: %s backend (%s) points to self, falling back to %s", provider, backend, defaults[provider])
			cfg.Backends[provider] = defaults[provider]
		}
	}

	if err := os.MkdirAll(cfg.LogDir, 0755); err != nil {
		log.Fatalf("Failed to create log dir: %v", err)
	}

	// Initialize MITM if enabled
	if getEnv("MITM_ENABLED", "") == "true" || getEnv("MITM_ENABLED", "") == "1" {
		cfg.MitmEnabled = true
		caDir := filepath.Join(os.Getenv("HOME"), ".llmproxy")
		if err := os.MkdirAll(caDir, 0700); err != nil {
			log.Fatalf("Failed to create CA dir: %v", err)
		}
		if err := loadOrGenerateCA(&cfg, caDir); err != nil {
			log.Fatalf("Failed to init CA: %v", err)
		}
		log.Printf("MITM enabled — CA cert: %s/ca.crt", caDir)
	}

	// Custom handler to support CONNECT method
	handler := http.NewServeMux()
	handler.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Inspect dashboard
		if strings.HasPrefix(r.URL.Path, "/__inspect__") {
			handleInspect(&cfg, w, r)
			return
		}
		// WebSocket upgrade — proxy raw TCP to real backend
		if isWebSocketUpgrade(r) {
			handleWSProxy(&cfg, w, r)
			return
		}
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			proxyPassthrough(&cfg, w, r)
			return
		}
		handleProxy(&cfg, w, r)
	})

	// Wrap to intercept CONNECT (ServeMux doesn't handle it)
	var mainHandler http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			if cfg.MitmEnabled {
				handleConnect(&cfg, w, r)
			} else {
				http.Error(w, "CONNECT not supported (enable MITM)", http.StatusMethodNotAllowed)
			}
			return
		}
		handler.ServeHTTP(w, r)
	})

	log.Printf("LLM Proxy listening on %s", cfg.ListenAddr)
	if cfg.Transparent {
		log.Printf("Mode: transparent (forwarding to original targets)")
	} else {
		log.Printf("Mode: override (rewriting to configured backends)")
	}
	if cfg.BackendURL != "" {
		log.Printf("Global backend override: %s", cfg.BackendURL)
	} else {
		for provider, backend := range cfg.Backends {
			log.Printf("  %s -> %s", provider, backend)
		}
	}
	if cfg.ForwardURL != "" {
		log.Printf("Forward URL: %s", cfg.ForwardURL)
	}
	if cfg.StripPrompt {
		log.Printf("System prompt stripping enabled")
	}
	for provider, key := range cfg.APIKeys {
		if key != "" {
			log.Printf("  %s API key: configured (%d chars)", provider, len(key))
		}
	}

	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		log.Fatal(err)
	}
	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("Accept: %v", err)
			continue
		}
		go handleRawConn(&cfg, conn, mainHandler)
	}
}

func handleRawConn(cfg *Config, conn net.Conn, handler http.Handler) {
	// Peek at first byte to detect raw TLS vs HTTP
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	peek := make([]byte, 1)
	n, _ := conn.Read(peek)
	conn.SetReadDeadline(time.Time{})

	if n == 1 && peek[0] == 0x16 && cfg.MitmEnabled {
		// Raw TLS ClientHello — do MITM directly
		handleRawTLSMitm(cfg, conn, peek)
		return
	}

	// Normal HTTP — serve via hijackable conn
	if n == 0 {
		conn.Close()
		return
	}
	bc := &peekedConn{Conn: conn, peek: peek[:n]}
	sl := &singleConnListener{conn: bc}
	sv := &http.Server{Handler: handler}
	sv.Serve(sl)
	return
}

// peekedConn wraps a net.Conn with pre-read bytes
type peekedConn struct {
	net.Conn
	peek    []byte
	peeked  bool
}

func (c *peekedConn) Read(b []byte) (int, error) {
	if !c.peeked {
		c.peeked = true
		copy(b, c.peek)
		if len(b) >= len(c.peek) {
			return len(c.peek), nil
		}
		return len(b), nil
	}
	return c.Conn.Read(b)
}

func (c *peekedConn) CloseWrite() error {
	if tcp, ok := c.Conn.(*net.TCPConn); ok {
		return tcp.CloseWrite()
	}
	return nil
}

// http.ServeConn adapter — we use the listener-based approach instead
func httpServeConn(bc *peekedConn, handler http.Handler) {
	// Use a pipe to feed the connection into the HTTP server
	// Actually, just use a custom ResponseWriter + ReadCloser
	l := &singleConnListener{conn: bc}
	s := &http.Server{Handler: handler}
	s.Serve(l)
}

type singleConnListener struct {
	conn net.Conn
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	conn := l.conn
	l.conn = nil
	if conn == nil {
		return nil, io.EOF
	}
	return conn, nil
}
func (l *singleConnListener) Close() error   { return nil }
func (l *singleConnListener) Addr() net.Addr { return nil }

// handleRawTLSMitm handles a direct TLS connection (no CONNECT) with MITM
func handleRawTLSMitm(cfg *Config, conn net.Conn, peek []byte) {
	// Read more of the ClientHello to extract SNI
	header := make([]byte, 5)
	copy(header, peek)
	if _, err := io.ReadFull(conn, header[1:]); err != nil {
		conn.Close()
		return
	}

	// TLS record header: type(1) + version(2) + length(2)
	recordLen := int(header[3])<<8 | int(header[4])
	recordData := make([]byte, recordLen)
	if _, err := io.ReadFull(conn, recordData); err != nil {
		conn.Close()
		return
	}

	// Combine for SNI extraction
	fullRecord := append(header, recordData...)
	hostname := extractSNI(fullRecord)
	if hostname == "" {
		hostname = "unknown"
	}
	log.Printf("[raw-tls] MITM for %s", hostname)

	// Connect to real target
	targetConn, err := net.DialTimeout("tcp", hostname+":443", 10*time.Second)
	if err != nil {
		log.Printf("[raw-tls] target %s unreachable: %v", hostname, err)
		conn.Close()
		return
	}
	defer targetConn.Close()

	// TLS with real target
	tlsTarget := tls.Client(targetConn, &tls.Config{
		ServerName: hostname,
		MinVersion: tls.VersionTLS12,
	})
	if err := tlsTarget.Handshake(); err != nil {
		log.Printf("[raw-tls] target TLS failed: %v", err)
		conn.Close()
		return
	}
	defer tlsTarget.Close()

	// Get leaf cert for MITM
	leafCert, err := cfg.getLeafCert(hostname)
	if err != nil {
		log.Printf("[raw-tls] cert generation failed: %v", err)
		conn.Close()
		return
	}

	// Feed the already-consumed ClientHello to a pipe so tls.Server can read it.
	pipeReader, pipeWriter := io.Pipe()
	go func() {
		pipeWriter.Write(fullRecord) // full TLS record including 5-byte header
		// Then bridge: read from real conn and write to pipe
		buf := make([]byte, 64*1024)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				pipeWriter.Write(buf[:n])
			}
			if err != nil {
				pipeWriter.Close()
				return
			}
		}
	}()

	// Create TLS server using the pipe
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{*leafCert},
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"http/1.1"},
	}

	// Wrap the pipe reader+conn into a net.Conn for tls.Server
	pc := &pipeConnWrap{r: pipeReader, w: conn}
	tlsClient := tls.Server(pc, tlsConfig)
	if err := tlsClient.Handshake(); err != nil {
		log.Printf("[raw-tls] client TLS handshake failed: %v", err)
		return
	}
	defer tlsClient.Close()

	log.Printf("[raw-tls] %s — MITM active", hostname)

	// Proxy data between client and target with inspection
	done := make(chan struct{}, 2)
	reqState := &wsParseState{host: hostname, direction: "request"}
	respState := &wsParseState{host: hostname, direction: "response"}
	go mitmCopy(cfg, tlsTarget, tlsClient, hostname, "request", reqState, done)
	go mitmCopy(cfg, tlsClient, tlsTarget, hostname, "response", respState, done)
	<-done
}

// sharedBackendReader wraps a net.Conn with a persistent bufio.Reader
// so buffered data survives across multiple HTTP requests.
type sharedBackendReader struct {
	conn   net.Conn
	reader *bufio.Reader
}

func (b *sharedBackendReader) Write(data []byte) (int, error) { return b.conn.Write(data) }
func (b *sharedBackendReader) Close() error                   { return b.conn.Close() }

type pipeConnWrap struct {
	r *io.PipeReader
	w net.Conn
}
func (c *pipeConnWrap) Read(b []byte) (int, error)  { return c.r.Read(b) }
func (c *pipeConnWrap) Write(b []byte) (int, error) { return c.w.Write(b) }
func (c *pipeConnWrap) Close() error                 { c.r.Close(); return nil }
func (c *pipeConnWrap) LocalAddr() net.Addr          { return nil }
func (c *pipeConnWrap) RemoteAddr() net.Addr         { return nil }
func (c *pipeConnWrap) SetDeadline(t time.Time) error       { return nil }
func (c *pipeConnWrap) SetReadDeadline(t time.Time) error   { return nil }
func (c *pipeConnWrap) SetWriteDeadline(t time.Time) error  { return nil }

// extractSNI parses a TLS ClientHello to extract the SNI hostname
func extractSNI(record []byte) string {
	if len(record) < 44 {
		return ""
	}
	// Skip TLS record header (5 bytes)
	data := record[5:]
	if len(data) < 39 {
		return ""
	}

	// Handshake type should be 1 (ClientHello)
	if data[0] != 1 {
		return ""
	}

	// Skip handshake header: type(1) + length(3) = 4
	// ClientHello: version(2) + random(32) = 34
	pos := 4 + 2 + 32 // past handshake header, version, random
	if pos >= len(data) {
		return ""
	}

	// Session ID length
	sessionIDLen := int(data[pos])
	pos += 1 + sessionIDLen
	if pos+2 > len(data) {
		return ""
	}

	// Cipher suites length
	cipherLen := int(data[pos])<<8 | int(data[pos+1])
	pos += 2 + cipherLen
	if pos >= len(data) {
		return ""
	}

	// Compression methods length
	compLen := int(data[pos])
	pos += 1 + compLen
	if pos+2 > len(data) {
		return ""
	}

	// Extensions length
	extLen := int(data[pos])<<8 | int(data[pos+1])
	pos += 2
	extEnd := pos + extLen

	for pos+4 <= extEnd && pos+4 <= len(data) {
		extType := int(data[pos])<<8 | int(data[pos+1])
		extDataLen := int(data[pos+2])<<8 | int(data[pos+3])
		pos += 4

		if extType == 0x0000 { // SNI
			if pos+2 > len(data) {
				break
			}
			// Server Name List length
			// sniListLen := int(data[pos])<<8 | int(data[pos+1])
			pos += 2
			// Server Name Type
			if pos+1 > len(data) {
				break
			}
			// nameType := data[pos]
			pos++
			if pos+2 > len(data) {
				break
			}
			nameLen := int(data[pos])<<8 | int(data[pos+1])
			pos += 2
			if pos+nameLen > len(data) {
				break
			}
			return string(data[pos : pos+nameLen])
		}
		pos += extDataLen
	}
	return ""
}

// handleRawTLSWSProxy handles a WebSocket upgrade on a raw TLS MITM connection.
func handleRawTLSWSProxy(cfg *Config, w http.ResponseWriter, r *http.Request, backend *sharedBackendReader, hostname string) {
	// Send the upgrade request to the real backend
	var reqBuf bytes.Buffer
	fmt.Fprintf(&reqBuf, "GET %s HTTP/1.1\r\n", r.URL.RequestURI())
	fmt.Fprintf(&reqBuf, "Host: %s\r\n", hostname)
	for k, vs := range r.Header {
		for _, v := range vs {
			fmt.Fprintf(&reqBuf, "%s: %s\r\n", k, v)
		}
	}
	reqBuf.WriteString("\r\n")
	if _, err := backend.Write(reqBuf.Bytes()); err != nil {
		http.Error(w, "backend upgrade write failed", http.StatusBadGateway)
		return
	}

	// Read 101 response using shared reader
	respLine, err := backend.reader.ReadString('\n')
	if err != nil {
		http.Error(w, "backend response read failed", http.StatusBadGateway)
		return
	}
	if !strings.Contains(respLine, "101") {
		http.Error(w, "backend did not upgrade", http.StatusBadGateway)
		log.Printf("[raw-tls-ws:%s] backend response: %s", hostname, strings.TrimSpace(respLine))
		return
	}

	var respHeaders bytes.Buffer
	respHeaders.WriteString(respLine)
	for {
		line, err := backend.reader.ReadString('\n')
		if err != nil {
			http.Error(w, "reading backend headers failed", http.StatusBadGateway)
			return
		}
		respHeaders.WriteString(line)
		if line == "\r\n" {
			break
		}
	}

	// Hijack client
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}
	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, fmt.Sprintf("hijack failed: %v", err), http.StatusInternalServerError)
		return
	}
	defer clientConn.Close()

	// Send backend's 101 response to client
	clientConn.Write(respHeaders.Bytes())

	log.Printf("[raw-tls-ws:%s] %s — WebSocket tunnel active", hostname, r.URL.Path)

	// Bidirectional proxy with frame parsing
	done := make(chan struct{}, 2)
	reqState := &wsParseState{host: hostname, direction: "request"}
	respState := &wsParseState{host: hostname, direction: "response"}

	// Client -> Backend
	go func() {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 64*1024)
		for {
			n, err := clientConn.Read(buf)
			if n > 0 {
				data := make([]byte, n)
				copy(data, buf[:n])
				if _, err := backend.Write(data); err != nil {
					return
				}
				reqState.buf.Write(data)
				reqState.tryParseFrames(cfg)
			}
			if err != nil {
				return
			}
		}
	}()

	// Backend -> Client (drain buffered data first)
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			if backend.reader.Buffered() > 0 {
				chunk, _ := backend.reader.Peek(backend.reader.Buffered())
				data := make([]byte, len(chunk))
				copy(data, chunk)
				backend.reader.Discard(len(chunk))
				clientConn.Write(data)
				respState.buf.Write(data)
				respState.tryParseFrames(cfg)
			}
			buf := make([]byte, 64*1024)
			n, err := backend.reader.Read(buf)
			if n > 0 {
				data := make([]byte, n)
				copy(data, buf[:n])
				clientConn.Write(data)
				respState.buf.Write(data)
				respState.tryParseFrames(cfg)
			}
			if err != nil {
				return
			}
		}
	}()

	<-done
}

func isWebSocketUpgrade(r *http.Request) bool {
	for _, v := range r.Header["Upgrade"] {
		if strings.EqualFold(v, "websocket") {
			return true
		}
	}
	return false
}

func proxyPassthrough(cfg *Config, w http.ResponseWriter, r *http.Request) {
	apiType := detectAPIType(r.URL.Path, nil)
	target := cfg.resolveTarget(r, apiType)
	reqURL := target + r.URL.Path
	if r.URL.RawQuery != "" {
		reqURL += "?" + r.URL.RawQuery
	}

	req, err := http.NewRequest(r.Method, reqURL, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	copyHeaders(req.Header, r.Header)
	cfg.injectAPIKey(req, apiType)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func handleProxy(cfg *Config, w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read body", http.StatusBadRequest)
		return
	}

	path := r.URL.Path
	log.Printf("-> %s %s (%d bytes)", r.Method, path, len(body))

	// Extract structured info from request
	apiType := detectAPIType(path, body)
	model := extractModel(body, path)
	system := extractSystemPrompt(body)
	messages := extractMessages(body, apiType)

	// Save system prompt snapshot for this agent
	if system != "" {
		saveSystemPrompt(cfg, system)
	}

	// Write request log to per-agent subdirectory
	agentLogDir := filepath.Join(cfg.LogDir, cfg.AgentType)
	os.MkdirAll(agentLogDir, 0755)
	timestamp := time.Now().Format("20060102-150405")
	logFile := fmt.Sprintf("%s/%s-%s", agentLogDir, timestamp, apiType)
	writeRequestLog(logFile, apiType, model, system, messages, body)

	// Strip system prompt restrictions if enabled
	if cfg.StripPrompt && system != "" {
		if modified := stripSystemPrompt(system); modified != system {
			var modErr error
			body, modErr = modifySystemInBody(body, modified)
			if modErr != nil {
				log.Printf("[strip] Failed to modify body: %v", modErr)
			} else {
				log.Printf("[strip] System prompt: %d -> %d bytes", len(system), len(modified))
			}
		}
	}

	// Forward request to the target
	target := cfg.resolveTarget(r, apiType)
	reqURL := target + path
	if r.URL.RawQuery != "" {
		reqURL += "?" + r.URL.RawQuery
	}

	req, err := http.NewRequest(r.Method, reqURL, bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	copyHeaders(req.Header, r.Header)
	req.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))
	cfg.injectAPIKey(req, apiType)

	startTime := time.Now()
	resp, err := http.DefaultClient.Do(req)
	latency := time.Since(startTime)
	if err != nil {
		http.Error(w, fmt.Sprintf("Backend error: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	log.Printf("<- %d %s (%s)", resp.StatusCode, path, latency.Round(time.Millisecond))

	// Stream response to client while capturing it
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	isStream := isStreamingRequest(body, path)
	proxyAndCapture(w, resp, logFile, isStream, apiType)
	writeMetadata(logFile, apiType, model, resp.StatusCode, latency, len(body))
}

type LogEntry struct {
	Filename string
	Agent    string
	Time     string
	API      string
	Model    string
	System   string
	Messages []string
	Response string
}

func handleInspect(cfg *Config, w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// Serve specific log file as JSON
	if strings.HasPrefix(path, "/__inspect__/api/") {
		filename := strings.TrimPrefix(path, "/__inspect__/api/")
		serveLogJSON(cfg.LogDir, filename, w)
		return
	}

	// Serve raw file
	if strings.HasPrefix(path, "/__inspect__/raw/") {
		filename := strings.TrimPrefix(path, "/__inspect__/raw/")
		http.ServeFile(w, r, filepath.Join(cfg.LogDir, filename))
		return
	}

	// Dashboard
	entries := loadLogs(cfg.LogDir)
	tmpl := template.Must(template.ParseFS(webFS, "web/inspect.html"))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	tmpl.Execute(w, struct {
		Entries []LogEntry
		Port    string
	}{
		Entries: entries,
		Port:    cfg.ListenAddr,
	})
}

func loadLogs(logDir string) []LogEntry {
	// Collect from root and per-agent subdirectories
	var allMatches []string
	rootMatches, _ := filepath.Glob(filepath.Join(logDir, "*.md"))
	allMatches = append(allMatches, rootMatches...)
	agentDirs, _ := filepath.Glob(filepath.Join(logDir, "*"))
	for _, d := range agentDirs {
		info, err := os.Stat(d)
		if err != nil || !info.IsDir() {
			continue
		}
		subMatches, _ := filepath.Glob(filepath.Join(d, "*.md"))
		allMatches = append(allMatches, subMatches...)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(allMatches)))

	var entries []LogEntry
	for _, f := range allMatches {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		content := string(data)
		// Derive agent from subdirectory
		agentLabel := ""
		rel, _ := filepath.Rel(logDir, f)
		parts := strings.SplitN(rel, string(filepath.Separator), 2)
		if len(parts) == 2 {
			agentLabel = parts[0]
		}
		entry := LogEntry{
			Filename: filepath.Base(f),
			Agent:    agentLabel,
		}

		// Parse simple markdown fields
		for _, line := range strings.Split(content, "\n") {
			if strings.HasPrefix(line, "**Time**: ") {
				entry.Time = strings.TrimPrefix(line, "**Time**: ")
			}
			if strings.HasPrefix(line, "**API**: ") {
				entry.API = strings.TrimPrefix(line, "**API**: ")
			}
			if strings.HasPrefix(line, "**Model**: ") {
				entry.Model = strings.TrimPrefix(line, "**Model**: ")
			}
		}

		// Extract system prompt section
		if idx := strings.Index(content, "## System Prompt\n\n"); idx != -1 {
			rest := content[idx+len("## System Prompt\n\n"):]
			if end := strings.Index(rest, "\n## "); end != -1 {
				entry.System = rest[:end]
			} else {
				entry.System = rest
			}
			// Truncate for preview
			if len(entry.System) > 200 {
				entry.System = entry.System[:200] + "..."
			}
		}

		// Extract response section
		if idx := strings.Index(content, "## Response\n\n"); idx != -1 {
			entry.Response = content[idx+len("## Response\n\n"):]
			if len(entry.Response) > 200 {
				entry.Response = entry.Response[:200] + "..."
			}
		}

		entries = append(entries, entry)
	}
	return entries
}

func serveLogJSON(logDir, filename string, w http.ResponseWriter) {
	// Prevent path traversal
	if strings.Contains(filename, "..") {
		http.Error(w, "invalid filename", http.StatusBadRequest)
		return
	}

	mdPath := filepath.Join(logDir, filename)
	reqPath := strings.TrimSuffix(mdPath, ".md") + ".req.json"

	result := map[string]string{
		"markdown": readFile(mdPath),
		"request":  readFile(reqPath),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func readFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

func proxyAndCapture(w http.ResponseWriter, resp *http.Response, logBase string, isStream bool, apiType string) {
	if !isStream {
		body, _ := io.ReadAll(resp.Body)
		w.Write(body)
		appendResponse(logBase, string(body))
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		io.Copy(w, resp.Body)
		return
	}

	var captured bytes.Buffer
	writer := io.MultiWriter(w, &captured)

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		fmt.Fprintf(writer, "%s\n", line)
		if line == "" || strings.HasPrefix(line, "event:") {
			flusher.Flush()
			continue
		}
		if strings.HasPrefix(line, "data:") {
			fmt.Fprintf(writer, "\n")
			flusher.Flush()
		}
	}

	appendResponse(logBase, extractStreamedText(captured.Bytes(), apiType))
}

func detectAPIType(path string, body []byte) string {
	if strings.Contains(path, "chat/completions") || strings.Contains(path, "/responses") {
		return "openai"
	}
	if strings.Contains(path, "/messages") {
		return "anthropic"
	}
	if strings.Contains(path, "generateContent") || strings.Contains(path, "streamGenerateContent") {
		return "gemini"
	}
	return "unknown"
}

// detectAPITypeFromHost falls back to hostname matching when path detection returns "unknown".
func detectAPITypeFromHost(path string, body []byte, host string) string {
	apiType := detectAPIType(path, body)
	if apiType != "unknown" {
		return apiType
	}
	h := strings.ToLower(host)
	if strings.Contains(h, "anthropic") {
		return "anthropic"
	}
	if strings.Contains(h, "openai") {
		return "openai"
	}
	if strings.Contains(h, "googleapis") {
		return "gemini"
	}
	return "unknown"
}

func extractModel(body []byte, reqPath string) string {
	var r struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &r) == nil && r.Model != "" {
		return r.Model
	}
	// Gemini: model is in the URL path (/v1beta/models/gemini-2.0-flash:generateContent)
	if idx := strings.Index(reqPath, "/models/"); idx != -1 {
		after := reqPath[idx+8:]
		if colon := strings.Index(after, ":"); colon != -1 {
			return after[:colon]
		}
		return after
	}
	return ""
}

func extractSystemPrompt(body []byte) string {
	// Anthropic: "system" as string
	var r1 struct {
		System json.RawMessage `json:"system"`
	}
	if json.Unmarshal(body, &r1) == nil && r1.System != nil {
		// Try string
		var s string
		if json.Unmarshal(r1.System, &s) == nil {
			return s
		}
		// Try array of content blocks
		var blocks []struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(r1.System, &blocks) == nil {
			var parts []string
			for _, b := range blocks {
				parts = append(parts, b.Text)
			}
			return strings.Join(parts, "\n")
		}
	}

	// OpenAI: system role in messages
	// Content can be string or array — use RawMessage to handle both
	var r2 struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &r2) == nil {
		for _, m := range r2.Messages {
			if m.Role == "system" {
				var s string
				if json.Unmarshal(m.Content, &s) == nil {
					return s
				}
				// Array of content blocks
				var blocks []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}
				if json.Unmarshal(m.Content, &blocks) == nil {
					var parts []string
					for _, b := range blocks {
						if b.Text != "" {
							parts = append(parts, b.Text)
						}
					}
					return strings.Join(parts, "\n")
				}
			}
		}
	}
	// Codex (WebSocket): "instructions" field in response.create
	var r3 struct {
		Instructions string `json:"instructions"`
	}
	if json.Unmarshal(body, &r3) == nil && r3.Instructions != "" {
		return r3.Instructions
	}

	// Responses API: top-level "instructions" field
	var r4 struct {
		Instructions json.RawMessage `json:"instructions"`
	}
	if json.Unmarshal(body, &r4) == nil && r4.Instructions != nil {
		var s string
		if json.Unmarshal(r4.Instructions, &s) == nil && s != "" {
			return s
		}
	}

	// Responses API: developer/system role in "input" array
	var r5 struct {
		Input []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"input"`
	}
	if json.Unmarshal(body, &r5) == nil {
		for _, m := range r5.Input {
			if m.Role == "developer" || m.Role == "system" {
				var s string
				if json.Unmarshal(m.Content, &s) == nil {
					return s
				}
				var blocks []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}
				if json.Unmarshal(m.Content, &blocks) == nil {
					var parts []string
					for _, b := range blocks {
						if b.Text != "" {
							parts = append(parts, b.Text)
						}
					}
					return strings.Join(parts, "\n")
				}
			}
		}
	}

	// Gemini: systemInstruction.parts[].text
	var r6 struct {
		SystemInstruction struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"systemInstruction"`
	}
	if json.Unmarshal(body, &r6) == nil && len(r6.SystemInstruction.Parts) > 0 {
		var gParts []string
		for _, p := range r6.SystemInstruction.Parts {
			if p.Text != "" {
				gParts = append(gParts, p.Text)
			}
		}
		if len(gParts) > 0 {
			return strings.Join(gParts, "\n")
		}
	}

	return ""
}

var defaultStripKeywords = []string{
	// Cybersecurity restrictions
	"authorized security testing",
	"destructive techniques",
	"detection evasion",
	"mass targeting",
	"supply chain compromise",
	"malicious purposes",
	"dual-use security",
	// Telemetry
	"telemetry",
}

func stripSystemPrompt(system string) string {
	paragraphs := strings.Split(system, "\n\n")
	var kept []string
	stripped := 0
	for _, p := range paragraphs {
		lower := strings.ToLower(p)
		remove := false
		for _, kw := range defaultStripKeywords {
			if strings.Contains(lower, strings.ToLower(kw)) {
				remove = true
				break
			}
		}
		if !remove {
			kept = append(kept, p)
		} else {
			stripped++
		}
	}
	if stripped > 0 {
		log.Printf("[strip] Removed %d paragraph(s) from system prompt", stripped)
	}
	result := strings.Join(kept, "\n\n")

	// Replace identity references
	result = strings.ReplaceAll(result, "Claude", "Loki")
	result = strings.ReplaceAll(result, "claude", "loki")

	return result
}

func modifySystemInBody(body []byte, newSystem string) ([]byte, error) {
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		return body, err
	}

	// Anthropic: top-level "system" field
	if sys, ok := m["system"]; ok {
		switch sys.(type) {
		case string:
			m["system"] = newSystem
		case []interface{}:
			m["system"] = []interface{}{
				map[string]interface{}{"type": "text", "text": newSystem},
			}
		}
	}

	// OpenAI: messages array with role=system
	if msgs, ok := m["messages"].([]interface{}); ok {
		for i, msg := range msgs {
			if msgMap, ok := msg.(map[string]interface{}); ok {
				if role, _ := msgMap["role"].(string); role == "system" {
					switch msgMap["content"].(type) {
					case string:
						msgMap["content"] = newSystem
					case []interface{}:
						msgMap["content"] = []interface{}{
							map[string]interface{}{"type": "text", "text": newSystem},
						}
					}
					msgs[i] = msgMap
				}
			}
		}
	}

	// Instructions field (Codex-like and Responses API)
	if _, ok := m["instructions"]; ok {
		if _, isStr := m["instructions"].(string); isStr {
			m["instructions"] = newSystem
		}
	}

	// Responses API: developer/system role in "input" array
	if input, ok := m["input"].([]interface{}); ok {
		for i, item := range input {
			if msgMap, ok := item.(map[string]interface{}); ok {
				if role, _ := msgMap["role"].(string); role == "developer" || role == "system" {
					switch msgMap["content"].(type) {
					case string:
						msgMap["content"] = newSystem
					case []interface{}:
						msgMap["content"] = []interface{}{
							map[string]interface{}{"type": "input_text", "text": newSystem},
						}
					}
					input[i] = msgMap
				}
			}
		}
	}

	// Gemini: systemInstruction.parts[].text
	if si, ok := m["systemInstruction"].(map[string]interface{}); ok {
		if parts, ok := si["parts"].([]interface{}); ok && len(parts) > 0 {
			si["parts"] = []interface{}{
				map[string]interface{}{"text": newSystem},
			}
		}
	}

	return json.Marshal(m)
}

func saveSystemPrompt(cfg *Config, system string) {
	agentDir := filepath.Join(cfg.LogDir, cfg.AgentType)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		log.Printf("Failed to create agent dir: %v", err)
		return
	}

	// Save current snapshot
	promptPath := filepath.Join(agentDir, "prompt.md")

	// Also save a timestamped copy
	timestampPath := filepath.Join(agentDir, fmt.Sprintf("prompt-%s.md", time.Now().Format("20060102-150405")))

	// Only write if content changed
	existing, err := os.ReadFile(promptPath)
	if err == nil && string(existing) == system {
		return
	}

	if err := os.WriteFile(promptPath, []byte(system), 0644); err != nil {
		log.Printf("Failed to save system prompt: %v", err)
	} else {
		log.Printf("Saved system prompt to %s", promptPath)
	}
	os.WriteFile(timestampPath, []byte(system), 0644)
}

func extractMessages(body []byte, apiType string) []string {
	var out []string

	// OpenAI Chat Completions: messages array
	type msg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	var r1 struct {
		Messages []msg `json:"messages"`
	}
	if json.Unmarshal(body, &r1) == nil && len(r1.Messages) > 0 {
		for _, m := range r1.Messages {
			if m.Role != "system" && m.Role != "developer" {
				out = append(out, fmt.Sprintf("**%s**: %s", m.Role, m.Content))
			}
		}
		return out
	}

	// Responses API: input array (or string)
	var r2 struct {
		Input json.RawMessage `json:"input"`
	}
	if json.Unmarshal(body, &r2) == nil && r2.Input != nil {
		var s string
		if json.Unmarshal(r2.Input, &s) == nil {
			return []string{fmt.Sprintf("**user**: %s", s)}
		}
		var msgs []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}
		if json.Unmarshal(r2.Input, &msgs) == nil {
			for _, m := range msgs {
				if m.Role != "developer" && m.Role != "system" {
					out = append(out, fmt.Sprintf("**%s**: %s", m.Role, m.Content))
				}
			}
			return out
		}
	}

	// Gemini: contents array with parts
	var r3 struct {
		Contents []struct {
			Role  string `json:"role"`
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"contents"`
	}
	if json.Unmarshal(body, &r3) == nil {
		for _, c := range r3.Contents {
			var texts []string
			for _, p := range c.Parts {
				if p.Text != "" {
					texts = append(texts, p.Text)
				}
			}
			if len(texts) > 0 {
				role := c.Role
				if role == "model" {
					role = "assistant"
				}
				out = append(out, fmt.Sprintf("**%s**: %s", role, strings.Join(texts, "\n")))
			}
		}
		return out
	}

	return nil
}

func isStreamingRequest(body []byte, reqPath string) bool {
	// Gemini: streaming is determined by the URL path
	if strings.Contains(reqPath, "streamGenerateContent") {
		return true
	}
	var r struct {
		Stream bool `json:"stream"`
	}
	if json.Unmarshal(body, &r) == nil {
		return r.Stream
	}
	return false
}

func extractStreamedText(data []byte, apiType string) string {
	var parts []string

	// Try SSE-based parsing (Anthropic, OpenAI Chat, OpenAI Responses)
	for _, line := range bytes.Split(data, []byte("\n")) {
		line := bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		payload := bytes.TrimPrefix(line, []byte("data: "))
		if bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}

		// Anthropic content_block_delta
		var d1 struct {
			Delta struct {
				Text string `json:"text"`
			} `json:"delta"`
		}
		if json.Unmarshal(payload, &d1) == nil && d1.Delta.Text != "" {
			parts = append(parts, d1.Delta.Text)
			continue
		}

		// OpenAI Chat Completions chunk
		var d2 struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal(payload, &d2) == nil && len(d2.Choices) > 0 && d2.Choices[0].Delta.Content != "" {
			parts = append(parts, d2.Choices[0].Delta.Content)
			continue
		}

		// OpenAI Responses API: response.output_text.delta
		var d3 struct {
			Type  string `json:"type"`
			Delta string `json:"delta"`
		}
		if json.Unmarshal(payload, &d3) == nil && d3.Type == "response.output_text.delta" && d3.Delta != "" {
			parts = append(parts, d3.Delta)
			continue
		}
	}

	if len(parts) > 0 {
		return strings.Join(parts, "")
	}

	// Gemini: response is a JSON array of chunks, not SSE
	trimmed := bytes.TrimSpace(data)
	if bytes.HasPrefix(trimmed, []byte("[")) || bytes.HasPrefix(trimmed, []byte("{")) {
		var chunks []struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
		}
		if json.Unmarshal(trimmed, &chunks) == nil {
			for _, ch := range chunks {
				for _, c := range ch.Candidates {
					for _, p := range c.Content.Parts {
						if p.Text != "" {
							parts = append(parts, p.Text)
						}
					}
				}
			}
		}
	}

	return strings.Join(parts, "")
}

func writeRequestLog(logBase, apiType, model, system string, messages []string, rawBody []byte) {
	var buf bytes.Buffer
	buf.WriteString("# LLM Request\n\n")
	buf.WriteString(fmt.Sprintf("**Time**: %s\n", time.Now().Format(time.RFC3339)))
	buf.WriteString(fmt.Sprintf("**API**: %s\n", apiType))
	buf.WriteString(fmt.Sprintf("**Model**: %s\n\n", model))

	if system != "" {
		buf.WriteString("## System Prompt\n\n")
		buf.WriteString(system)
		buf.WriteString("\n\n")
	}

	if len(messages) > 0 {
		buf.WriteString("## Messages\n\n")
		for _, m := range messages {
			buf.WriteString(m)
			buf.WriteString("\n\n")
		}
	}

	buf.WriteString("## Response\n\n")

	if err := os.WriteFile(logBase+".md", buf.Bytes(), 0644); err != nil {
		log.Printf("Failed to write log: %v", err)
	} else {
		log.Printf("Logged request to %s.md", logBase)
	}
	// Also save raw JSON for debugging
	os.WriteFile(logBase+".req.json", rawBody, 0644)
}

func writeMetadata(logBase, apiType, model string, statusCode int, latency time.Duration, reqSize int) {
	type meta struct {
		API        string `json:"api"`
		Model      string `json:"model"`
		StatusCode int    `json:"status_code"`
		LatencyMs  int64  `json:"latency_ms"`
		ReqBytes   int    `json:"req_bytes"`
	}
	m := meta{
		API:        apiType,
		Model:      model,
		StatusCode: statusCode,
		LatencyMs:  latency.Milliseconds(),
		ReqBytes:   reqSize,
	}
	data, _ := json.Marshal(m)
	os.WriteFile(logBase+".meta.json", data, 0644)
}

func appendResponse(logBase, text string) {
	filename := logBase + ".md"
	content, err := os.ReadFile(filename)
	if err != nil {
		return
	}
	// Replace the "Response" section placeholder
	updated := append(content, []byte(text)...)
	updated = append(updated, '\n')
	if err := os.WriteFile(filename, updated, 0644); err != nil {
		log.Printf("Failed to append response: %v", err)
	} else {
		log.Printf("Appended response to %s", filename)
	}
	os.WriteFile(logBase+".resp.txt", []byte(text), 0644)
}

func copyHeaders(dst, src http.Header) {
	for k, v := range src {
		dst[k] = v
	}
}

func getEnv(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}

// --- CA Certificate Management ---

func loadOrGenerateCA(cfg *Config, dir string) error {
	certPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")

	// Try to load existing CA
	certData, err1 := os.ReadFile(certPath)
	keyData, err2 := os.ReadFile(keyPath)
	if err1 == nil && err2 == nil {
		block, _ := pem.Decode(certData)
		if block != nil {
			cert, err := x509.ParseCertificate(block.Bytes)
			if err == nil {
				keyBlock, _ := pem.Decode(keyData)
				if keyBlock != nil {
					key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
					if err == nil {
						cfg.caCert = cert
						cfg.caKey = key
						cfg.caCertPEM = certData
						log.Printf("Loaded existing CA cert from %s", certPath)
						return nil
					}
				}
			}
		}
	}

	// Generate new CA
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate CA key: %w", err)
	}

	serialNumber, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	template := &x509.Certificate{
		SerialNumber:          serialNumber,
		Subject:               pkixName("LLM Proxy CA"),
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("create CA cert: %w", err)
	}

	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return fmt.Errorf("parse CA cert: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEMBuf, _ := x509.MarshalECPrivateKey(key)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyPEMBuf})

	if err := os.WriteFile(certPath, certPEM, 0644); err != nil {
		return fmt.Errorf("write CA cert: %w", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		return fmt.Errorf("write CA key: %w", err)
	}

	cfg.caCert = cert
	cfg.caKey = key
	cfg.caCertPEM = certPEM
	log.Printf("Generated new CA cert at %s", certPath)
	return nil
}

func pkixName(cn string) pkix.Name {
	return pkix.Name{
		CommonName:   cn,
		Organization: []string{"LLM Proxy"},
	}
}

func (c *Config) getLeafCert(host string) (*tls.Certificate, error) {
	if v, ok := c.certCache.Load(host); ok {
		return v.(*tls.Certificate), nil
	}

	serialNumber, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	template := &x509.Certificate{
		SerialNumber:    serialNumber,
		Subject:         pkixName(host),
		NotBefore:       time.Now().Add(-time.Hour),
		NotAfter:        time.Now().Add(24 * time.Hour),
		KeyUsage:        x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:        []string{host},
		IPAddresses:     parseIPAddresses(host),
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, c.caCert, &leafKey.PublicKey, c.caKey)
	if err != nil {
		return nil, err
	}

	tlsCert := &tls.Certificate{
		Certificate: [][]byte{certDER, c.caCert.Raw},
		PrivateKey:  leafKey,
		Leaf:        nil,
	}
	if leaf, err := x509.ParseCertificate(certDER); err == nil {
		tlsCert.Leaf = leaf
	}

	c.certCache.Store(host, tlsCert)
	return tlsCert, nil
}

func parseIPAddresses(host string) []net.IP {
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = host
	}
	if ip := net.ParseIP(h); ip != nil {
		return []net.IP{ip}
	}
	return nil
}

// --- WebSocket Proxy (plain HTTP upgrade, no MITM needed) ---

func handleWSProxy(cfg *Config, w http.ResponseWriter, r *http.Request) {
	var forwardURL string
	if cfg.Transparent {
		host := r.Host
		if host == "" {
			host = r.URL.Host
		}
		if isSelfReference(host, cfg.ListenAddr) {
			apiType := detectAPIType(r.URL.Path, nil)
			forwardURL = cfg.backendForAPI(apiType)
		} else {
			forwardURL = "https://" + host
		}
	} else {
		forwardURL = cfg.ForwardURL
		if forwardURL == "" {
			forwardURL = cfg.BackendURL
		}
		if forwardURL == "" {
			forwardURL = cfg.Backends["anthropic"]
		}
	}

	// Parse forward URL to get host for TLS
	target := forwardURL
	if !strings.Contains(target, "://") {
		target = "https://" + target
	}
	targetURL, err := url.Parse(target)
	if err != nil {
		http.Error(w, fmt.Sprintf("bad forward URL: %v", err), http.StatusInternalServerError)
		return
	}

	host := targetURL.Host
	hostname := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		hostname = h
	}
	port := "443"
	if targetURL.Scheme == "http" {
		port = "80"
	}
	if h, p, err := net.SplitHostPort(host); err == nil {
		hostname = h
		port = p
	}

	// Connect to real backend
	var backendConn net.Conn
	if targetURL.Scheme == "https" || targetURL.Scheme == "wss" {
		rawConn, err := net.DialTimeout("tcp", hostname+":"+port, 10*time.Second)
		if err != nil {
			http.Error(w, fmt.Sprintf("backend unreachable: %v", err), http.StatusBadGateway)
			return
		}
		backendConn = tls.Client(rawConn, &tls.Config{
			ServerName: hostname,
			MinVersion: tls.VersionTLS12,
		})
		if err := backendConn.(*tls.Conn).Handshake(); err != nil {
			rawConn.Close()
			http.Error(w, fmt.Sprintf("backend TLS failed: %v", err), http.StatusBadGateway)
			return
		}
	} else {
		var err error
		backendConn, err = net.DialTimeout("tcp", hostname+":"+port, 10*time.Second)
		if err != nil {
			http.Error(w, fmt.Sprintf("backend unreachable: %v", err), http.StatusBadGateway)
			return
		}
	}
	defer backendConn.Close()

	// Build the upgrade request to send to the real backend
	upgradeURL := r.URL.Path
	if r.URL.RawQuery != "" {
		upgradeURL += "?" + r.URL.RawQuery
	}
	var upgradeReq bytes.Buffer
	fmt.Fprintf(&upgradeReq, "GET %s HTTP/1.1\r\n", upgradeURL)
	fmt.Fprintf(&upgradeReq, "Host: %s\r\n", host)
	for k, vs := range r.Header {
		for _, v := range vs {
			fmt.Fprintf(&upgradeReq, "%s: %s\r\n", k, v)
		}
	}
	upgradeReq.WriteString("\r\n")

	if _, err := backendConn.Write(upgradeReq.Bytes()); err != nil {
		http.Error(w, fmt.Sprintf("upgrade write failed: %v", err), http.StatusBadGateway)
		return
	}

	// Read 101 response from backend
	backendReader := bufio.NewReader(backendConn)
	respLine, err := backendReader.ReadString('\n')
	if err != nil {
		http.Error(w, fmt.Sprintf("backend response read failed: %v", err), http.StatusBadGateway)
		return
	}
	if !strings.Contains(respLine, "101") {
		http.Error(w, "backend did not upgrade to WebSocket", http.StatusBadGateway)
		log.Printf("[ws-proxy] backend response: %s", strings.TrimSpace(respLine))
		return
	}

	// Read remaining headers
	var respHeaders bytes.Buffer
	respHeaders.WriteString(respLine)
	for {
		line, err := backendReader.ReadString('\n')
		if err != nil {
			http.Error(w, "reading backend headers failed", http.StatusBadGateway)
			return
		}
		respHeaders.WriteString(line)
		if line == "\r\n" {
			break
		}
	}

	// Hijack client connection
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}
	clientConn, clientBuf, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, fmt.Sprintf("hijack failed: %v", err), http.StatusInternalServerError)
		return
	}
	defer clientConn.Close()

	// Send backend's 101 response to client
	clientConn.Write(respHeaders.Bytes())

	log.Printf("[ws-proxy] %s — WebSocket tunnel active", r.URL.Path)

	// Flush any buffered data from backend reader
	if clientBuf != nil {
		if n := clientBuf.Reader.Buffered(); n > 0 {
			buf := make([]byte, n)
			clientBuf.Reader.Read(buf)
			// This is data from the client that was buffered — discard or handle
		}
	}

	// Proxy data bidirectionally with frame parsing
	done := make(chan struct{}, 2)

	// Client -> Backend (requests)
	reqState := &wsParseState{host: hostname, direction: "request"}
	go func() {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 64*1024)
		for {
			n, err := clientConn.Read(buf)
			if n > 0 {
				data := make([]byte, n)
				copy(data, buf[:n])
				if _, err := backendConn.Write(data); err != nil {
					return
				}
				reqState.buf.Write(data)
				reqState.tryParseFrames(cfg)
			}
			if err != nil {
				return
			}
		}
	}()

	// Backend -> Client (responses) — need to handle buffered data from backendReader
	respState := &wsParseState{host: hostname, direction: "response"}
	go func() {
		defer func() { done <- struct{}{} }()

		// First, drain any data buffered in backendReader
		for {
			if backendReader.Buffered() > 0 {
				chunk, _ := backendReader.Peek(backendReader.Buffered())
				data := make([]byte, len(chunk))
				copy(data, chunk)
				backendReader.Discard(len(chunk))
				clientConn.Write(data)
				respState.buf.Write(data)
				respState.tryParseFrames(cfg)
			}
			buf := make([]byte, 64*1024)
			n, err := backendReader.Read(buf)
			if n > 0 {
				data := make([]byte, n)
				copy(data, buf[:n])
				clientConn.Write(data)
				respState.buf.Write(data)
				respState.tryParseFrames(cfg)
			}
			if err != nil {
				return
			}
		}
	}()

	<-done
}

func handleConnect(cfg *Config, w http.ResponseWriter, r *http.Request) {
	host := r.URL.Host
	if !strings.Contains(host, ":") {
		host += ":443"
	}
	hostname := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		hostname = h
	}

	// Connect to real target
	targetConn, err := net.DialTimeout("tcp", host, 10*time.Second)
	if err != nil {
		http.Error(w, fmt.Sprintf("target unreachable: %v", err), http.StatusBadGateway)
		log.Printf("CONNECT %s — target unreachable: %v", host, err)
		return
	}
	defer targetConn.Close()

	// Hijack client connection
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}
	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, fmt.Sprintf("hijack failed: %v", err), http.StatusInternalServerError)
		return
	}
	defer clientConn.Close()

	// Send 200 Connection Established
	clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

	// TLS handshake with client using our forged cert
	leafCert, err := cfg.getLeafCert(hostname)
	if err != nil {
		log.Printf("CONNECT %s — cert generation failed: %v", host, err)
		return
	}

	tlsClient := tls.Server(clientConn, &tls.Config{
		Certificates: []tls.Certificate{*leafCert},
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"http/1.1"},
	})
	if err := tlsClient.Handshake(); err != nil {
		log.Printf("CONNECT %s — client TLS handshake failed: %v", host, err)
		return
	}
	defer tlsClient.Close()

	// TLS handshake with real target
	tlsTarget := tls.Client(targetConn, &tls.Config{
		ServerName: hostname,
		MinVersion: tls.VersionTLS12,
	})
	if err := tlsTarget.Handshake(); err != nil {
		log.Printf("CONNECT %s — target TLS handshake failed: %v", host, err)
		return
	}
	defer tlsTarget.Close()

	log.Printf("CONNECT %s — MITM active", host)

	// Proxy data between client and target with inspection
	done := make(chan struct{}, 2)
	// Track WS parsing state per direction
	reqState := &wsParseState{host: hostname, direction: "request"}
	respState := &wsParseState{host: hostname, direction: "response"}
	go mitmCopy(cfg, tlsTarget, tlsClient, hostname, "request", reqState, done)
	go mitmCopy(cfg, tlsClient, tlsTarget, hostname, "response", respState, done)

	// Wait for one direction to finish
	<-done
}

// wsParseState tracks WebSocket frame parsing state across chunks
type wsParseState struct {
	host              string
	direction         string
	buf               bytes.Buffer  // accumulated raw TCP data for frame parsing
	compressedAccum   bytes.Buffer  // accumulated compressed payloads (for shared deflate context)
	decompressedLen   int           // bytes already decompressed from the accumulated stream
}

func mitmCopy(cfg *Config, dst, src net.Conn, host, direction string, wsState *wsParseState, done chan struct{}) {
	defer func() { done <- struct{}{} }()

	buf := make([]byte, 64*1024)

	for {
		n, err := src.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])

			// Forward immediately
			if _, werr := dst.Write(data); werr != nil {
				return
			}

			// Accumulate for frame parsing
			wsState.buf.Write(data)
			wsState.tryParseFrames(cfg)
		}
		if err != nil {
			return
		}
	}
}

func (s *wsParseState) tryParseFrames(cfg *Config) {
	data := s.buf.Bytes()

	// Skip HTTP request/response headers (and bodies for HTTP responses)
	for {
		if len(data) == 0 {
			return
		}

		if bytes.HasPrefix(data, []byte("POST ")) ||
			bytes.HasPrefix(data, []byte("GET ")) ||
			bytes.HasPrefix(data, []byte("PUT ")) ||
			bytes.HasPrefix(data, []byte("DELETE ")) {
			headerEnd := bytes.Index(data, []byte("\r\n\r\n"))
			if headerEnd == -1 {
				return
			}
			firstLine := bytes.SplitN(data[:headerEnd], []byte("\n"), 2)[0]
			log.Printf("[http:%s] %s", s.host, strings.TrimSpace(string(firstLine)))
			s.buf.Next(headerEnd + 4)
			data = s.buf.Bytes()
			continue
		}

		if bytes.HasPrefix(data, []byte("HTTP/")) {
			headerEnd := bytes.Index(data, []byte("\r\n\r\n"))
			if headerEnd == -1 {
				return
			}
			firstLine := bytes.SplitN(data[:headerEnd], []byte("\n"), 2)[0]
			log.Printf("[http:%s] %s", s.host, strings.TrimSpace(string(firstLine)))

			headers := data[:headerEnd]

			// Check for Content-Length or Transfer-Encoding to skip body
			bodyLen := 0
			if bytes.Contains(firstLine, []byte("101 ")) {
				// No body for 101 responses
				bodyLen = 0
			} else if clIdx := bytes.Index(headers, []byte("Content-Length:")); clIdx != -1 {
				clLine := headers[clIdx:]
				if clEnd := bytes.Index(clLine, []byte("\r\n")); clEnd != -1 {
					clVal := strings.TrimSpace(string(clLine[len("Content-Length:"):clEnd]))
					bodyLen, _ = strconv.Atoi(clVal)
				}
			} else if bytes.Contains(headers, []byte("Transfer-Encoding: chunked")) {
				// For chunked encoding, find the end of the body (0\r\n\r\n)
				bodyStart := headerEnd + 4
				chunkEnd := bytes.Index(data[bodyStart:], []byte("0\r\n\r\n"))
				if chunkEnd == -1 {
					return // incomplete chunked body
				}
				bodyLen = chunkEnd + 5 // include the "0\r\n\r\n"
			}

			totalLen := headerEnd + 4 + bodyLen
			if len(data) < totalLen {
				return // incomplete body, wait for more data
			}
			s.buf.Next(totalLen)
			data = s.buf.Bytes()
			continue
		}

		break
	}


	if len(data) == 0 {
		return
	}


	// Try to parse as many complete frames as possible
	for {
		frame, consumed, ok := tryParseOneFrame(data)
		if !ok {
			break // incomplete frame, wait for more data
		}

		s.buf.Next(consumed)
		data = s.buf.Bytes()

		if frame != nil && len(frame.Payload) > 0 && len(frame.Payload) <= 10*1024*1024 {
			s.logFrame(cfg, frame)
		}
	}
}

func getHTTPStatus(data []byte) int {
	parts := bytes.SplitN(data, []byte(" "), 3)
	if len(parts) >= 2 {
		code, _ := strconv.Atoi(string(parts[1]))
		return code
	}
	return 0
}

func tryParseOneFrame(data []byte) (*wsFrame, int, bool) {
	if len(data) < 2 {
		return nil, 0, false
	}

	fin := data[0]&0x80 != 0
	rsv1 := data[0]&0x40 != 0
	opcode := data[0] & 0x0F
	masked := data[1]&0x80 != 0
	payloadLen := int(data[1] & 0x7F)
	offset := 2

	switch payloadLen {
	case 126:
		if len(data) < offset+2 {
			return nil, 0, false
		}
		payloadLen = int(binary.BigEndian.Uint16(data[offset : offset+2]))
		offset += 2
	case 127:
		if len(data) < offset+8 {
			return nil, 0, false
		}
		payloadLen = int(binary.BigEndian.Uint64(data[offset : offset+8]))
		offset += 8
	}

	if payloadLen > 50*1024*1024 || payloadLen < 0 {
		return nil, 0, false
	}

	if masked {
		if len(data) < offset+4 {
			return nil, 0, false
		}
		offset += 4
	}

	if len(data) < offset+payloadLen {
		return nil, 0, false // incomplete frame
	}

	payload := make([]byte, payloadLen)
	copy(payload, data[offset:offset+payloadLen])
	if masked && offset >= 4 {
		maskKey := data[offset-4 : offset]
		for i := range payload {
			payload[i] ^= maskKey[i%4]
		}
	}

	frame := &wsFrame{Opcode: opcode, RSV1: rsv1, FIN: fin, Payload: payload}
	return frame, offset + payloadLen, true
}

func (s *wsParseState) logFrame(cfg *Config, f *wsFrame) {
	payload := f.Payload

	// Decompress permessage-deflate (RSV1 set)
	if f.RSV1 && len(payload) > 0 {

		// Append sync flush marker per RFC 7692
		s.compressedAccum.Write(payload)
		s.compressedAccum.Write([]byte{0x00, 0x00, 0xFF, 0xFF})

		// Decompress the full accumulated stream to maintain shared context
		allCompressed := s.compressedAccum.Bytes()
		decompressor := flate.NewReader(bytes.NewReader(allCompressed))
		var out bytes.Buffer
		_, err := io.Copy(&out, decompressor)
		decompressor.Close()

		// io.ErrUnexpectedEOF is expected for permessage-deflate (BFINAL=0).
		// The decompressed data in out is still valid - keep it.
		if err == nil || err == io.ErrUnexpectedEOF {
			if out.Len() > s.decompressedLen {
				payload = out.Bytes()[s.decompressedLen:]
				s.decompressedLen = out.Len()
			} else {
				payload = nil
			}
		} else {
			log.Printf("[ws:%s] %s deflate error: %v (frame %d bytes)", s.host, s.direction, err, len(payload))
			payload = nil
		}

		if len(payload) == 0 {
			return
		}
	}

	opcodeName := "text"
	if f.Opcode == 2 {
		opcodeName = "binary"
	} else if f.Opcode == 8 {
		opcodeName = "close"
	} else if f.Opcode == 9 {
		opcodeName = "ping"
	} else if f.Opcode == 0xA {
		opcodeName = "pong"
	}

	compressed := ""
	if f.RSV1 {
		compressed = " [compressed]"
	}
	finFlag := ""
	if !f.FIN {
		finFlag = " [fragment]"
	}

	// Log first 200 bytes as preview
	preview := string(payload)
	if len(preview) > 200 {
		preview = preview[:200] + "..."
	}
	log.Printf("[ws:%s] %s %s%s%s frame (%d bytes): %s", s.host, s.direction, opcodeName, compressed, finFlag, len(payload), preview)

	// Try to parse as JSON
	var raw json.RawMessage
	if json.Unmarshal(payload, &raw) == nil {
		var pretty bytes.Buffer
		if json.Indent(&pretty, payload, "", "  ") == nil {
			logMsg := pretty.String()
			if len(logMsg) > 5000 {
				logMsg = logMsg[:5000] + "\n... (truncated)"
			}
			log.Printf("[ws:%s] %s JSON %s (%d bytes)\n%s", s.host, s.direction, frameType(payload), len(payload), logMsg)
			saveWSTraffic(cfg, s.host, s.direction, payload)

			// Extract system prompt from WS request traffic
			if s.direction == "request" {
				if sys := extractSystemPrompt(payload); sys != "" {
					saveSystemPrompt(cfg, sys)
				}
			}
		}
	}
}

// --- WebSocket Frame Parser ---

type wsFrame struct {
	Opcode  byte
	RSV1    bool
	FIN     bool
	Payload []byte
}

func frameType(payload []byte) string {
	var v struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(payload, &v) == nil && v.Type != "" {
		return v.Type
	}
	return "data"
}

func saveWSTraffic(cfg *Config, host, direction string, payload []byte) {
	agentLogDir := filepath.Join(cfg.LogDir, cfg.AgentType)
	os.MkdirAll(agentLogDir, 0755)
	timestamp := time.Now().Format("20060102-150405")
	logFile := filepath.Join(agentLogDir, fmt.Sprintf("%s-ws-%s-%s.json", timestamp, host, direction))

	// Append to existing file or create new
	var existing []byte
	if data, err := os.ReadFile(logFile); err == nil {
		existing = data
	}

	var entries []json.RawMessage
	if len(existing) > 0 {
		json.Unmarshal(existing, &entries)
	}

	// Wrap in envelope
	envelope := map[string]interface{}{
		"timestamp": time.Now().Format(time.RFC3339Nano),
		"direction": direction,
		"host":      host,
		"data":      json.RawMessage(payload),
	}
	envelopeJSON, _ := json.Marshal(envelope)
	entries = append(entries, envelopeJSON)

	out, _ := json.MarshalIndent(entries, "", "  ")
	os.WriteFile(logFile, out, 0644)
}
