package main

import (
	"bufio"
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

//go:embed web/inspect.html
var webFS embed.FS

type Config struct {
	ListenAddr string
	BackendURL string
	LogDir     string
}

func main() {
	backendURL := getEnv("BACKEND_URL", "")
	if backendURL == "" {
		backendURL = getEnv("ANTHROPIC_BASE_URL", "https://api.anthropic.com")
	}
	// Strip trailing slash
	backendURL = strings.TrimRight(backendURL, "/")

	cfg := Config{
		ListenAddr: getEnv("LISTEN_ADDR", ":8765"),
		BackendURL: backendURL,
		LogDir:     getEnv("LOG_DIR", "./logs"),
	}

	if err := os.MkdirAll(cfg.LogDir, 0755); err != nil {
		log.Fatalf("Failed to create log dir: %v", err)
	}

	// Single catch-all handler — transparent reverse proxy
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Inspect dashboard
		if strings.HasPrefix(r.URL.Path, "/__inspect__") {
			handleInspect(cfg, w, r)
			return
		}
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			proxyPassthrough(cfg, w, r)
			return
		}
		handleProxy(cfg, w, r)
	})

	log.Printf("LLM Proxy listening on %s, forwarding to %s", cfg.ListenAddr, cfg.BackendURL)
	log.Fatal(http.ListenAndServe(cfg.ListenAddr, nil))
}

func proxyPassthrough(cfg Config, w http.ResponseWriter, r *http.Request) {
	url := cfg.BackendURL + r.URL.Path
	if r.URL.RawQuery != "" {
		url += "?" + r.URL.RawQuery
	}

	req, err := http.NewRequest(r.Method, url, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	copyHeaders(req.Header, r.Header)

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

func handleProxy(cfg Config, w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read body", http.StatusBadRequest)
		return
	}

	path := r.URL.Path
	log.Printf("-> %s %s (%d bytes)", r.Method, path, len(body))

	// Extract structured info from request
	apiType := detectAPIType(path, body)
	model := extractModel(body)
	system := extractSystemPrompt(body)
	messages := extractMessages(body)

	// Write request log
	timestamp := time.Now().Format("20060102-150405")
	logFile := fmt.Sprintf("%s/%s-%s", cfg.LogDir, timestamp, apiType)
	writeRequestLog(logFile, apiType, model, system, messages, body)

	// Forward request
	url := cfg.BackendURL + path
	if r.URL.RawQuery != "" {
		url += "?" + r.URL.RawQuery
	}

	req, err := http.NewRequest(r.Method, url, bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	copyHeaders(req.Header, r.Header)
	req.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, fmt.Sprintf("Backend error: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	log.Printf("<- %d %s", resp.StatusCode, path)

	// Stream response to client while capturing it
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	isStream := isStreamingRequest(body)
	proxyAndCapture(w, resp, logFile, isStream)
}

type LogEntry struct {
	Filename string
	Time     string
	API      string
	Model    string
	System   string
	Messages []string
	Response string
}

func handleInspect(cfg Config, w http.ResponseWriter, r *http.Request) {
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
	matches, _ := filepath.Glob(filepath.Join(logDir, "*.md"))
	sort.Sort(sort.Reverse(sort.StringSlice(matches)))

	var entries []LogEntry
	for _, f := range matches {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		content := string(data)
		entry := LogEntry{
			Filename: filepath.Base(f),
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
	if strings.Contains(filename, "..") || strings.Contains(filename, "/") {
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

func proxyAndCapture(w http.ResponseWriter, resp *http.Response, logBase string, isStream bool) {
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

	appendResponse(logBase, extractStreamedText(captured.Bytes()))
}

func detectAPIType(path string, body []byte) string {
	if strings.Contains(path, "chat/completions") {
		return "openai"
	}
	if strings.Contains(path, "/messages") {
		return "anthropic"
	}
	return "unknown"
}

func extractModel(body []byte) string {
	var r struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &r) == nil {
		return r.Model
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
	var r2 struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &r2) == nil {
		for _, m := range r2.Messages {
			if m.Role == "system" {
				return m.Content
			}
		}
	}
	return ""
}

func extractMessages(body []byte) []string {
	type msg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}

	// Try as struct with messages array
	var r struct {
		Messages []msg `json:"messages"`
	}
	if json.Unmarshal(body, &r) == nil {
		var out []string
		for _, m := range r.Messages {
			if m.Role != "system" {
				out = append(out, fmt.Sprintf("**%s**: %s", m.Role, m.Content))
			}
		}
		return out
	}
	return nil
}

func isStreamingRequest(body []byte) bool {
	var r struct {
		Stream bool `json:"stream"`
	}
	if json.Unmarshal(body, &r) == nil {
		return r.Stream
	}
	return false
}

func extractStreamedText(data []byte) string {
	var parts []string
	for _, line := range bytes.Split(data, []byte("\n")) {
		line := bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		payload := bytes.TrimPrefix(line, []byte("data: "))

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

		// OpenAI chunk
		var d2 struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal(payload, &d2) == nil && len(d2.Choices) > 0 && d2.Choices[0].Delta.Content != "" {
			parts = append(parts, d2.Choices[0].Delta.Content)
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
