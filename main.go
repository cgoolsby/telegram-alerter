package main

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	telegramMaxMessageLen = 4096
	maxAlertsListed       = 10
	previewMaxLen         = 120
)

// errTelegramThrottled marks a Telegram 429 (rate limited) so callers can
// distinguish throttling from other failures.
var errTelegramThrottled = errors.New("telegram rate limited")

type config struct {
	botToken       string
	chatID         string
	authToken      string
	port           string
	apiBase        string
	throttleWindow time.Duration
	logContent     bool
}

type server struct {
	cfg      config
	client   *http.Client
	metrics  *metrics
	throttle *throttler
}

type sendRequest struct {
	Message   string `json:"message"`
	ParseMode string `json:"parse_mode,omitempty"`
	Silent    bool   `json:"silent,omitempty"`
	ChatID    string `json:"chat_id,omitempty"`
}

type telegramSendMessage struct {
	ChatID              string `json:"chat_id"`
	Text                string `json:"text"`
	ParseMode           string `json:"parse_mode,omitempty"`
	DisableNotification bool   `json:"disable_notification,omitempty"`
}

type telegramResponse struct {
	OK          bool   `json:"ok"`
	Description string `json:"description"`
}

// alertmanagerPayload is the fixed webhook body Alertmanager POSTs. Grafana's
// webhook is a superset of this shape, so it parses too.
type alertmanagerPayload struct {
	Status            string            `json:"status"`
	Receiver          string            `json:"receiver"`
	ExternalURL       string            `json:"externalURL"`
	CommonLabels      map[string]string `json:"commonLabels"`
	CommonAnnotations map[string]string `json:"commonAnnotations"`
	Alerts            []alert           `json:"alerts"`
}

type alert struct {
	Status      string            `json:"status"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	StartsAt    string            `json:"startsAt"`
}

// metrics is a tiny stdlib Prometheus counter for messages by endpoint/result.
// Series are pre-seeded so rate() works from the first scrape.
type metrics struct {
	mu     sync.Mutex
	counts map[[2]string]int64
}

var (
	metricEndpoints = []string{"send", "alertmanager"}
	metricResults   = []string{"sent", "failed", "throttled", "rejected"}
)

func newMetrics() *metrics {
	m := &metrics{counts: make(map[[2]string]int64)}
	for _, e := range metricEndpoints {
		for _, r := range metricResults {
			m.counts[[2]string{e, r}] = 0
		}
	}
	return m
}

func (m *metrics) inc(endpoint, result string) {
	m.mu.Lock()
	m.counts[[2]string{endpoint, result}]++
	m.mu.Unlock()
}

func (m *metrics) write(w io.Writer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fmt.Fprintln(w, "# HELP telegram_alerter_messages_total Messages processed by endpoint and result.")
	fmt.Fprintln(w, "# TYPE telegram_alerter_messages_total counter")
	for _, e := range metricEndpoints {
		for _, r := range metricResults {
			fmt.Fprintf(w, "telegram_alerter_messages_total{endpoint=%q,result=%q} %d\n", e, r, m.counts[[2]string{e, r}])
		}
	}
}

// throttler suppresses duplicate messages (same chat+text) seen within a
// window, so a flapping alert source can't spam Telegram. A zero window
// disables it.
type throttler struct {
	window time.Duration
	mu     sync.Mutex
	seen   map[string]time.Time
}

func newThrottler(window time.Duration) *throttler {
	return &throttler{window: window, seen: make(map[string]time.Time)}
}

// allow reports whether a message may be sent now, recording it if so.
func (t *throttler) allow(key string) bool {
	if t.window <= 0 {
		return true
	}
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, ts := range t.seen {
		if now.Sub(ts) > t.window {
			delete(t.seen, k)
		}
	}
	if ts, ok := t.seen[key]; ok && now.Sub(ts) <= t.window {
		return false
	}
	t.seen[key] = now
	return true
}

func throttleKey(chatID, text string) string {
	sum := sha256.Sum256([]byte(chatID + "\x00" + text))
	return hex.EncodeToString(sum[:])
}

func loadConfig() (config, error) {
	cfg := config{
		botToken:   os.Getenv("TELEGRAM_BOT_TOKEN"),
		chatID:     os.Getenv("TELEGRAM_CHAT_ID"),
		authToken:  os.Getenv("AUTH_TOKEN"),
		port:       os.Getenv("PORT"),
		apiBase:    os.Getenv("TELEGRAM_API_BASE"),
		logContent: true,
	}
	if cfg.botToken == "" {
		return cfg, fmt.Errorf("TELEGRAM_BOT_TOKEN is required")
	}
	if cfg.chatID == "" {
		return cfg, fmt.Errorf("TELEGRAM_CHAT_ID is required")
	}
	if cfg.authToken == "" {
		return cfg, fmt.Errorf("AUTH_TOKEN is required")
	}
	if cfg.port == "" {
		cfg.port = "8080"
	}
	if cfg.apiBase == "" {
		cfg.apiBase = "https://api.telegram.org"
	}
	if v := os.Getenv("THROTTLE_WINDOW_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.throttleWindow = time.Duration(n) * time.Second
		}
	}
	if v := os.Getenv("LOG_MESSAGE_CONTENT"); v != "" {
		cfg.logContent, _ = strconv.ParseBool(v)
	}
	return cfg, nil
}

func (s *server) authorized(r *http.Request) bool {
	auth := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.authToken)) == 1
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": msg})
}

// sendMessage posts text to Telegram. The returned error is safe to surface to
// callers (the bot token is never included). Falls back to the default chat.
// A Telegram 429 is returned wrapped in errTelegramThrottled.
func (s *server) sendMessage(chatID, text, parseMode string, silent bool) error {
	if chatID == "" {
		chatID = s.cfg.chatID
	}
	if len(text) > telegramMaxMessageLen {
		text = text[:telegramMaxMessageLen]
	}
	payload, err := json.Marshal(telegramSendMessage{
		ChatID:              chatID,
		Text:                text,
		ParseMode:           parseMode,
		DisableNotification: silent,
	})
	if err != nil {
		return fmt.Errorf("failed to encode request")
	}

	url := fmt.Sprintf("%s/bot%s/sendMessage", s.cfg.apiBase, s.cfg.botToken)
	resp, err := s.client.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		log.Printf("telegram request failed: %v", redactToken(err.Error(), s.cfg.botToken))
		return fmt.Errorf("failed to reach telegram")
	}
	defer resp.Body.Close()

	var tgResp telegramResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tgResp); err != nil {
		log.Printf("failed to decode telegram response (status %d): %v", resp.StatusCode, err)
		return fmt.Errorf("invalid response from telegram")
	}
	if !tgResp.OK {
		log.Printf("telegram rejected message (status %d): %s", resp.StatusCode, tgResp.Description)
		if resp.StatusCode == http.StatusTooManyRequests {
			return fmt.Errorf("%w: %s", errTelegramThrottled, tgResp.Description)
		}
		return fmt.Errorf("telegram error: %s", tgResp.Description)
	}
	return nil
}

// deliver runs the throttle check, sends, records metrics, and writes the HTTP
// response. endpoint labels the metrics ("send" or "alertmanager").
func (s *server) deliver(w http.ResponseWriter, endpoint, chatID, text, parseMode string, silent bool) {
	if !s.throttle.allow(throttleKey(chatID, text)) {
		s.metrics.inc(endpoint, "throttled")
		log.Printf("event=message_throttled endpoint=%s chat=%s", endpoint, effectiveChat(chatID, s.cfg.chatID))
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "throttled": true})
		return
	}
	if err := s.sendMessage(chatID, text, parseMode, silent); err != nil {
		if errors.Is(err, errTelegramThrottled) {
			s.metrics.inc(endpoint, "throttled")
			writeError(w, http.StatusTooManyRequests, err.Error())
			return
		}
		s.metrics.inc(endpoint, "failed")
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	s.metrics.inc(endpoint, "sent")
	s.logSent(endpoint, chatID, text)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *server) logSent(endpoint, chatID, text string) {
	chat := effectiveChat(chatID, s.cfg.chatID)
	if s.cfg.logContent {
		log.Printf("event=message_sent endpoint=%s chat=%s chars=%d preview=%q", endpoint, chat, len(text), previewText(text))
	} else {
		log.Printf("event=message_sent endpoint=%s chat=%s chars=%d", endpoint, chat, len(text))
	}
}

func effectiveChat(chatID, fallback string) string {
	if chatID == "" {
		return fallback
	}
	return chatID
}

func previewText(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > previewMaxLen {
		s = s[:previewMaxLen] + "…"
	}
	return s
}

func (s *server) handleSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.authorized(r) {
		s.metrics.inc("send", "rejected")
		writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
		return
	}

	var req sendRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		s.metrics.inc("send", "rejected")
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		s.metrics.inc("send", "rejected")
		writeError(w, http.StatusBadRequest, "message is required")
		return
	}

	s.deliver(w, "send", req.ChatID, req.Message, req.ParseMode, req.Silent)
}

// handleAlertmanager accepts Alertmanager's (and Grafana's) webhook payload,
// formats a readable page, and forwards it to Telegram.
func (s *server) handleAlertmanager(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.authorized(r) {
		s.metrics.inc("alertmanager", "rejected")
		writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
		return
	}

	var payload alertmanagerPayload
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&payload); err != nil {
		s.metrics.inc("alertmanager", "rejected")
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	text := formatAlertmanager(payload)
	if text == "" {
		s.metrics.inc("alertmanager", "rejected")
		writeError(w, http.StatusBadRequest, "no alerts in payload")
		return
	}

	// Plain text (no parse_mode) so arbitrary label/annotation content can't
	// produce malformed markup that Telegram would reject.
	s.deliver(w, "alertmanager", "", text, "", false)
}

// formatAlertmanager renders a webhook payload into a concise plain-text page.
func formatAlertmanager(p alertmanagerPayload) string {
	if len(p.Alerts) == 0 {
		return ""
	}

	emoji := "🔥"
	if strings.EqualFold(p.Status, "resolved") {
		emoji = "✅"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s %s: %d alert(s)\n", emoji, strings.ToUpper(p.Status), len(p.Alerts))

	for i, a := range p.Alerts {
		if i >= maxAlertsListed {
			fmt.Fprintf(&b, "… and %d more\n", len(p.Alerts)-maxAlertsListed)
			break
		}
		name := a.Labels["alertname"]
		if name == "" {
			name = "alert"
		}
		if sev := a.Labels["severity"]; sev != "" {
			fmt.Fprintf(&b, "[%s] %s", sev, name)
		} else {
			b.WriteString(name)
		}
		if inst := a.Labels["instance"]; inst != "" {
			fmt.Fprintf(&b, " on %s", inst)
		}
		b.WriteString("\n")
		if summary := firstNonEmpty(a.Annotations["summary"], a.Annotations["description"]); summary != "" {
			fmt.Fprintf(&b, "  %s\n", summary)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func (s *server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func (s *server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	s.metrics.write(w)
}

// redactToken keeps the bot token out of logs; Telegram URLs embed it,
// so transport errors can leak it via the request URL.
func redactToken(msg, token string) string {
	return strings.ReplaceAll(msg, token, "<redacted>")
}

func newServer(cfg config) *server {
	return &server{
		cfg:      cfg,
		client:   &http.Client{Timeout: 10 * time.Second},
		metrics:  newMetrics(),
		throttle: newThrottler(cfg.throttleWindow),
	}
}

func (s *server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/send", s.handleSend)
	mux.HandleFunc("/webhook/alertmanager", s.handleAlertmanager)
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/metrics", s.handleMetrics)
	return mux
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("config error: %v", err)
	}
	s := newServer(cfg)
	addr := ":" + cfg.port
	log.Printf("telegram-alerter listening on %s (throttle=%s logContent=%t)", addr, cfg.throttleWindow, cfg.logContent)
	if err := http.ListenAndServe(addr, s.routes()); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
