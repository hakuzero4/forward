// Command forward is a small HTTP relay that forwards HTTP requests to other
// addresses (for example Telegram) on behalf of callers that cannot reach them
// directly, such as a home router. Every forwarding attempt is recorded and can
// be inspected on the built-in HTML page.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	maxBodyBytes      = 1 << 20  // 1 MiB: control payloads for /api/send and /api/forward
	defaultForwardMax = 10 << 20 // 10 MiB: forwarded request/response bodies
	defaultLogMax     = 1000
)

//go:embed logs.html
var logsHTML string

var (
	hopByHopHeaders = []string{
		"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
		"Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
	}
	botTokenPattern = regexp.MustCompile(`bot[0-9]+:[A-Za-z0-9_-]+`)
)

type config struct {
	listenAddr     string
	botToken       string
	relayAuthToken string
	apiBase        string
	timeout        time.Duration
	forwardMaxBody int64
	forwardAllow   []string
	forwardDeny    []string
	logFile        string
	logMax         int
}

type sendRequest struct {
	ChatID                string `json:"chat_id"`
	Text                  string `json:"text"`
	MessageThreadID       int64  `json:"message_thread_id,omitempty"`
	ParseMode             string `json:"parse_mode,omitempty"`
	DisableWebPagePreview bool   `json:"disable_web_page_preview,omitempty"`
	DisableNotification   bool   `json:"disable_notification,omitempty"`
	PhotoURL              string `json:"photo_url,omitempty"`
	Caption               string `json:"caption,omitempty"`
	BotToken              string `json:"bot_token,omitempty"`
}

func (r sendRequest) validate() error {
	if strings.TrimSpace(r.ChatID) == "" {
		return errors.New("chat_id is required")
	}
	if r.Text == "" && r.PhotoURL == "" {
		return errors.New("text or photo_url is required")
	}
	return nil
}

type forwardRequest struct {
	URL       string            `json:"url"`
	Method    string            `json:"method"`
	Headers   map[string]string `json:"headers"`
	Body      string            `json:"body"`
	TimeoutMS int               `json:"timeout_ms"`
}

func (r forwardRequest) validate() error {
	if strings.TrimSpace(r.URL) == "" {
		return errors.New("url is required")
	}
	u, err := url.Parse(r.URL)
	if err != nil {
		return fmt.Errorf("invalid url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("only http and https urls are supported")
	}
	if u.Host == "" {
		return errors.New("url host is required")
	}
	for name, value := range r.Headers {
		if !validHeaderName(name) {
			return fmt.Errorf("invalid header name: %q", name)
		}
		if strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("invalid header value for %q", name)
		}
	}
	return nil
}

type telegramResponse struct {
	OK          bool   `json:"ok"`
	Description string `json:"description"`
	Result      struct {
		MessageID int64 `json:"message_id"`
	} `json:"result"`
}

type sendResult struct {
	status    int
	errMsg    string
	messageID int64
	method    string
	chatID    string
}

type forwardResult struct {
	status         int
	errMsg         string
	upstreamStatus int
	headers        map[string]string
	body           string
	method         string
	url            string
}

type logEntry struct {
	ID             int64  `json:"id"`
	Time           string `json:"time"`
	Kind           string `json:"kind"` // send | forward
	Method         string `json:"method"`
	URL            string `json:"url"`
	ChatID         string `json:"chat_id,omitempty"`
	RelayStatus    int    `json:"relay_status"`
	UpstreamStatus int    `json:"upstream_status,omitempty"`
	Error          string `json:"error,omitempty"`
	DurationMS     int64  `json:"duration_ms"`
	ClientIP       string `json:"client_ip,omitempty"`
}

type logStore struct {
	mu   sync.Mutex
	file *os.File
	max  int
	buf  []logEntry
	next int64
}

func newLogStore(path string, max int) (*logStore, error) {
	if max <= 0 {
		max = defaultLogMax
	}
	ls := &logStore{max: max}
	if path != "" {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, fmt.Errorf("open log file: %w", err)
		}
		ls.file = f
		ls.loadFromFile(path)
	}
	return ls, nil
}

func (ls *logStore) loadFromFile(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var id int64
	for sc.Scan() {
		id++
		var e logEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue
		}
		e.ID = id
		ls.appendLocked(e)
	}
	ls.next = id + 1
}

func (ls *logStore) Add(e logEntry) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.next++
	e.ID = ls.next
	ls.appendLocked(e)
	if ls.file != nil {
		data, err := json.Marshal(e)
		if err == nil {
			_, _ = ls.file.Write(append(data, '\n'))
		}
	}
}

func (ls *logStore) appendLocked(e logEntry) {
	if len(ls.buf) >= ls.max {
		copy(ls.buf, ls.buf[1:])
		ls.buf = ls.buf[:ls.max-1]
	}
	ls.buf = append(ls.buf, e)
}

func (ls *logStore) List(offset, limit int, kind string, status int) []logEntry {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	out := make([]logEntry, 0, limit)
	skipped := 0
	for i := len(ls.buf) - 1; i >= 0; i-- {
		e := ls.buf[i]
		if kind != "" && e.Kind != kind {
			continue
		}
		if status != 0 && e.RelayStatus != status {
			continue
		}
		if skipped < offset {
			skipped++
			continue
		}
		out = append(out, e)
		if len(out) >= limit {
			break
		}
	}
	return out
}

func (ls *logStore) Clear() {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.buf = ls.buf[:0]
	if ls.file != nil {
		if err := ls.file.Close(); err != nil {
			slog.Error("failed to close log file before clear", "error", err)
		}
		f, err := os.OpenFile(ls.file.Name(), os.O_CREATE|os.O_TRUNC|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			slog.Error("failed to reopen log file after clear", "error", err)
			ls.file = nil
			return
		}
		ls.file = f
	}
}

func (ls *logStore) Close() error {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ls.file != nil {
		return ls.file.Close()
	}
	return nil
}

type server struct {
	cfg    config
	client *http.Client
	logs   *logStore
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)
	if strings.TrimSpace(cfg.botToken) == "" {
		slog.Warn("TELEGRAM_BOT_TOKEN not set; pass bot_token in /api/send requests or use /api/forward")
	}

	ls, err := newLogStore(cfg.logFile, cfg.logMax)
	if err != nil {
		slog.Error("failed to open log store; continuing with in-memory logs", "error", err)
		ls, err = newLogStore("", cfg.logMax)
		if err != nil {
			slog.Error("failed to create in-memory log store", "error", err)
			os.Exit(1)
		}
	}
	defer ls.Close()

	s := &server{
		cfg:    cfg,
		client: &http.Client{Transport: http.DefaultTransport},
		logs:   ls,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/logs", s.handleIndex)
	mux.HandleFunc("/api/logs", s.handleLogs)
	mux.HandleFunc("/api/send", s.requireAuth(s.handleSend))
	mux.HandleFunc("/api/forward", s.requireAuth(s.handleForward))
	mux.HandleFunc("/healthz", s.handleHealth)

	httpServer := &http.Server{
		Addr:              cfg.listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		slog.Info("forward service listening", "addr", cfg.listenAddr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("http server failed", "error", err)
			stop()
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		slog.Error("graceful shutdown failed", "error", err)
	}
}

func loadConfig() (config, error) {
	var cfg config
	flag.StringVar(&cfg.listenAddr, "listen", envOr("LISTEN_ADDR", ":8080"), "listen address")
	flag.StringVar(&cfg.botToken, "bot-token", os.Getenv("TELEGRAM_BOT_TOKEN"), "Telegram bot token (optional; pass bot_token per request if unset)")
	flag.StringVar(&cfg.relayAuthToken, "auth-token", os.Getenv("RELAY_AUTH_TOKEN"), "optional token required from callers (or RELAY_AUTH_TOKEN)")
	flag.StringVar(&cfg.apiBase, "api-base", envOr("TELEGRAM_API_BASE", "https://api.telegram.org"), "Telegram Bot API base URL")
	flag.DurationVar(&cfg.timeout, "timeout", 30*time.Second, "outbound request timeout")
	flag.Int64Var(&cfg.forwardMaxBody, "forward-max-body", envInt64("FORWARD_MAX_BODY", defaultForwardMax), "max bytes for forwarded request/response bodies")
	flag.StringVar(&cfg.logFile, "log-file", os.Getenv("LOG_FILE"), "JSONL log file for forwarding records (empty = memory only)")
	flag.IntVar(&cfg.logMax, "log-max", envInt("LOG_MAX", defaultLogMax), "max records kept in memory")
	var allowHosts, denyHosts string
	flag.StringVar(&allowHosts, "forward-allow-hosts", os.Getenv("FORWARD_ALLOW_HOSTS"), "comma-separated host allowlist for /api/forward (empty = allow all)")
	flag.StringVar(&denyHosts, "forward-deny-hosts", os.Getenv("FORWARD_DENY_HOSTS"), "comma-separated host denylist for /api/forward")
	flag.Parse()

	cfg.apiBase = strings.TrimRight(cfg.apiBase, "/")
	cfg.forwardAllow = splitHosts(allowHosts)
	cfg.forwardDeny = splitHosts(denyHosts)
	return cfg, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}

func envInt64(key string, fallback int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}

func splitHosts(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if h := strings.TrimSpace(strings.ToLower(part)); h != "" {
			out = append(out, h)
		}
	}
	return out
}

func hostAllowed(host string, allow, deny []string) bool {
	h := strings.ToLower(host)
	if len(allow) > 0 && !matchHost(h, allow) {
		return false
	}
	if len(deny) > 0 && matchHost(h, deny) {
		return false
	}
	return true
}

func matchHost(host string, list []string) bool {
	for _, pattern := range list {
		if host == pattern || strings.HasSuffix(host, "."+pattern) {
			return true
		}
	}
	return false
}

func isHopByHop(name string) bool {
	for _, h := range hopByHopHeaders {
		if strings.EqualFold(name, h) {
			return true
		}
	}
	return false
}

func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	const extra = "!#$%&'*+-.^_`|~"
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune(extra, rune(c))) {
			return false
		}
	}
	return true
}

func redactURL(raw string) string {
	s := botTokenPattern.ReplaceAllString(raw, "bot***")
	u, err := url.Parse(s)
	if err != nil {
		return s
	}
	q := u.Query()
	for _, key := range []string{"token", "access_token", "api_key", "key"} {
		if q.Has(key) {
			q.Set(key, "***")
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *server) handleIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, logsHTML)
}

func (s *server) handleLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		s.logs.Clear()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	q := r.URL.Query()
	limit := 100
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
			if limit > 5000 {
				limit = 5000
			}
		}
	}
	offset := 0
	if v := q.Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			offset = n
		}
	}
	kind := q.Get("kind")
	var status int
	if v := q.Get("status"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			status = n
		}
	}

	entries := s.logs.List(offset, limit, kind, status)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(entries), "entries": entries})
}

func (s *server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (s *server) handleSend(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	result := s.doSend(r)
	s.logs.Add(logEntry{
		Time:        time.Now().Format(time.RFC3339),
		Kind:        "send",
		Method:      http.MethodPost,
		URL:         fmt.Sprintf("%s/bot***/%s", s.cfg.apiBase, result.method),
		ChatID:      result.chatID,
		RelayStatus: result.status,
		Error:       result.errMsg,
		DurationMS:  time.Since(start).Milliseconds(),
		ClientIP:    clientIP(r),
	})
	if result.errMsg != "" {
		writeError(w, result.status, result.errMsg)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message_id": result.messageID})
}

func (s *server) doSend(r *http.Request) sendResult {
	if r.Method != http.MethodPost {
		return sendResult{status: http.StatusMethodNotAllowed, errMsg: "method not allowed"}
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		return sendResult{status: http.StatusBadRequest, errMsg: "failed to read request body"}
	}
	if len(body) > maxBodyBytes {
		return sendResult{status: http.StatusRequestEntityTooLarge, errMsg: "request body too large"}
	}

	var req sendRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return sendResult{status: http.StatusBadRequest, errMsg: "invalid JSON: " + err.Error()}
	}
	if err := req.validate(); err != nil {
		return sendResult{status: http.StatusBadRequest, errMsg: err.Error()}
	}

	method := "sendMessage"
	if req.PhotoURL != "" {
		method = "sendPhoto"
	}

	botToken := req.BotToken
	if botToken == "" {
		botToken = s.cfg.botToken
	}
	if botToken == "" {
		return sendResult{status: http.StatusBadRequest, errMsg: "bot_token is required (set TELEGRAM_BOT_TOKEN or pass bot_token)", method: method, chatID: req.ChatID}
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.timeout)
	defer cancel()

	resp, err := s.sendToTelegram(ctx, botToken, req)
	if err != nil {
		slog.Error("telegram request failed", "error", err)
		return sendResult{status: http.StatusBadGateway, errMsg: "telegram request failed: " + err.Error(), method: method, chatID: req.ChatID}
	}
	if !resp.OK {
		msg := resp.Description
		if msg == "" {
			msg = "unknown telegram error"
		}
		slog.Warn("telegram rejected message", "description", resp.Description)
		return sendResult{status: http.StatusBadGateway, errMsg: "telegram rejected the message: " + msg, method: method, chatID: req.ChatID}
	}

	return sendResult{status: http.StatusOK, messageID: resp.Result.MessageID, method: method, chatID: req.ChatID}
}

func (s *server) handleForward(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	result := s.doForward(r)
	s.logs.Add(logEntry{
		Time:           time.Now().Format(time.RFC3339),
		Kind:           "forward",
		Method:         result.method,
		URL:            redactURL(result.url),
		RelayStatus:    result.status,
		UpstreamStatus: result.upstreamStatus,
		Error:          result.errMsg,
		DurationMS:     time.Since(start).Milliseconds(),
		ClientIP:       clientIP(r),
	})
	if result.errMsg != "" {
		writeError(w, result.status, result.errMsg)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"status":  result.upstreamStatus,
		"headers": result.headers,
		"body":    result.body,
	})
}

func (s *server) doForward(r *http.Request) forwardResult {
	var req forwardRequest
	switch r.Method {
	case http.MethodPost:
		body, err := io.ReadAll(io.LimitReader(r.Body, s.cfg.forwardMaxBody+1))
		if err != nil {
			return forwardResult{status: http.StatusBadRequest, errMsg: "failed to read request body", method: http.MethodPost}
		}
		if int64(len(body)) > s.cfg.forwardMaxBody {
			return forwardResult{status: http.StatusRequestEntityTooLarge, errMsg: "request body too large", method: http.MethodPost}
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return forwardResult{status: http.StatusBadRequest, errMsg: "invalid JSON: " + err.Error(), method: http.MethodPost}
		}
	case http.MethodGet:
		req.URL = r.URL.Query().Get("url")
		req.Method = http.MethodGet
	default:
		return forwardResult{status: http.StatusMethodNotAllowed, errMsg: "method not allowed", method: r.Method}
	}
	if req.Method == "" {
		req.Method = http.MethodGet
	}
	if err := req.validate(); err != nil {
		return forwardResult{status: http.StatusBadRequest, errMsg: err.Error(), method: req.Method, url: req.URL}
	}

	u, err := url.Parse(req.URL)
	if err != nil {
		return forwardResult{status: http.StatusBadRequest, errMsg: "invalid url: " + err.Error(), method: req.Method, url: req.URL}
	}
	if !hostAllowed(u.Hostname(), s.cfg.forwardAllow, s.cfg.forwardDeny) {
		return forwardResult{status: http.StatusForbidden, errMsg: "host not allowed", method: req.Method, url: req.URL}
	}

	timeout := s.cfg.timeout
	if req.TimeoutMS > 0 {
		timeout = time.Duration(req.TimeoutMS) * time.Millisecond
		if timeout > 120*time.Second {
			timeout = 120 * time.Second
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	outReq, err := http.NewRequestWithContext(ctx, req.Method, req.URL, strings.NewReader(req.Body))
	if err != nil {
		return forwardResult{status: http.StatusBadRequest, errMsg: "invalid url: " + err.Error(), method: req.Method, url: req.URL}
	}
	for name, value := range req.Headers {
		if isHopByHop(name) {
			continue
		}
		if strings.EqualFold(name, "Host") {
			outReq.Host = value
			continue
		}
		outReq.Header.Set(name, value)
	}
	if req.Body != "" && outReq.Header.Get("Content-Type") == "" {
		outReq.Header.Set("Content-Type", "application/octet-stream")
	}

	resp, err := s.client.Do(outReq)
	if err != nil {
		slog.Error("forward request failed", "url", req.URL, "error", err)
		return forwardResult{status: http.StatusBadGateway, errMsg: "forward request failed: " + err.Error(), method: req.Method, url: req.URL}
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, s.cfg.forwardMaxBody+1))
	if err != nil {
		return forwardResult{status: http.StatusBadGateway, errMsg: "failed to read upstream response", method: req.Method, url: req.URL}
	}
	if int64(len(respBody)) > s.cfg.forwardMaxBody {
		return forwardResult{status: http.StatusBadGateway, errMsg: "upstream response too large", method: req.Method, url: req.URL}
	}

	headers := make(map[string]string, len(resp.Header))
	for name, values := range resp.Header {
		if isHopByHop(name) || len(values) == 0 {
			continue
		}
		headers[name] = values[0]
	}

	return forwardResult{
		status:         http.StatusOK,
		method:         req.Method,
		url:            req.URL,
		upstreamStatus: resp.StatusCode,
		headers:        headers,
		body:           string(respBody),
	}
}

func (s *server) sendToTelegram(ctx context.Context, botToken string, req sendRequest) (telegramResponse, error) {
	payload := map[string]any{
		"chat_id":              req.ChatID,
		"disable_notification": req.DisableNotification,
	}
	if req.MessageThreadID != 0 {
		payload["message_thread_id"] = req.MessageThreadID
	}
	if req.ParseMode != "" {
		payload["parse_mode"] = req.ParseMode
	}

	method := "sendMessage"
	if req.PhotoURL != "" {
		method = "sendPhoto"
		payload["photo"] = req.PhotoURL
		caption := req.Caption
		if caption == "" {
			caption = req.Text
		}
		if caption != "" {
			payload["caption"] = caption
		}
	} else {
		payload["text"] = req.Text
		if req.DisableWebPagePreview {
			payload["disable_web_page_preview"] = true
		}
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return telegramResponse{}, fmt.Errorf("encode payload: %w", err)
	}

	endpoint := fmt.Sprintf("%s/bot%s/%s", s.cfg.apiBase, botToken, method)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return telegramResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	res, err := s.client.Do(httpReq)
	if err != nil {
		return telegramResponse{}, err
	}
	defer res.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(res.Body, maxBodyBytes))
	if err != nil {
		return telegramResponse{}, err
	}

	var tgResp telegramResponse
	if err := json.Unmarshal(respBody, &tgResp); err != nil {
		return telegramResponse{}, fmt.Errorf("decode telegram response (status %d): %w", res.StatusCode, err)
	}
	return tgResp, nil
}

func (s *server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.relayAuthToken == "" {
			next(w, r)
			return
		}

		token := ""
		if h := r.Header.Get("X-Relay-Token"); h != "" {
			token = h
		} else if h := r.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(h), "bearer ") {
			token = strings.TrimSpace(h[len("Bearer "):])
		}
		if token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.relayAuthToken)) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": msg})
}
