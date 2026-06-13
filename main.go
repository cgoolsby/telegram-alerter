package main

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	telegramMaxMessageLen = 4096
	maxAlertsListed       = 10
)

type config struct {
	botToken  string
	chatID    string
	authToken string
	port      string
	apiBase   string
}

type server struct {
	cfg    config
	client *http.Client
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

func loadConfig() (config, error) {
	cfg := config{
		botToken:  os.Getenv("TELEGRAM_BOT_TOKEN"),
		chatID:    os.Getenv("TELEGRAM_CHAT_ID"),
		authToken: os.Getenv("AUTH_TOKEN"),
		port:      os.Getenv("PORT"),
		apiBase:   os.Getenv("TELEGRAM_API_BASE"),
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
		return fmt.Errorf("telegram error: %s", tgResp.Description)
	}

	log.Printf("message sent to chat %s (%d chars)", chatID, len(text))
	return nil
}

func (s *server) handleSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.authorized(r) {
		writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
		return
	}

	var req sendRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		writeError(w, http.StatusBadRequest, "message is required")
		return
	}

	if err := s.sendMessage(req.ChatID, req.Message, req.ParseMode, req.Silent); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleAlertmanager accepts Alertmanager's (and Grafana's) webhook payload,
// formats a readable page, and forwards it to Telegram.
func (s *server) handleAlertmanager(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.authorized(r) {
		writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
		return
	}

	var payload alertmanagerPayload
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	text := formatAlertmanager(payload)
	if text == "" {
		writeError(w, http.StatusBadRequest, "no alerts in payload")
		return
	}

	// Plain text (no parse_mode) so arbitrary label/annotation content can't
	// produce malformed markup that Telegram would reject.
	if err := s.sendMessage("", text, "", false); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
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

// redactToken keeps the bot token out of logs; Telegram URLs embed it,
// so transport errors can leak it via the request URL.
func redactToken(msg, token string) string {
	return strings.ReplaceAll(msg, token, "<redacted>")
}

func newServer(cfg config) *server {
	return &server{
		cfg:    cfg,
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

func (s *server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/send", s.handleSend)
	mux.HandleFunc("/webhook/alertmanager", s.handleAlertmanager)
	mux.HandleFunc("/healthz", s.handleHealthz)
	return mux
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("config error: %v", err)
	}
	s := newServer(cfg)
	addr := ":" + cfg.port
	log.Printf("telegram-alerter listening on %s", addr)
	if err := http.ListenAndServe(addr, s.routes()); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
