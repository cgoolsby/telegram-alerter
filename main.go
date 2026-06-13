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

const telegramMaxMessageLen = 4096

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

	text := req.Message
	if len(text) > telegramMaxMessageLen {
		text = text[:telegramMaxMessageLen]
	}
	chatID := req.ChatID
	if chatID == "" {
		chatID = s.cfg.chatID
	}

	payload, err := json.Marshal(telegramSendMessage{
		ChatID:              chatID,
		Text:                text,
		ParseMode:           req.ParseMode,
		DisableNotification: req.Silent,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to encode request")
		return
	}

	url := fmt.Sprintf("%s/bot%s/sendMessage", s.cfg.apiBase, s.cfg.botToken)
	resp, err := s.client.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		log.Printf("telegram request failed: %v", redactToken(err.Error(), s.cfg.botToken))
		writeError(w, http.StatusBadGateway, "failed to reach telegram")
		return
	}
	defer resp.Body.Close()

	var tgResp telegramResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tgResp); err != nil {
		log.Printf("failed to decode telegram response (status %d): %v", resp.StatusCode, err)
		writeError(w, http.StatusBadGateway, "invalid response from telegram")
		return
	}
	if !tgResp.OK {
		log.Printf("telegram rejected message (status %d): %s", resp.StatusCode, tgResp.Description)
		writeError(w, http.StatusBadGateway, fmt.Sprintf("telegram error: %s", tgResp.Description))
		return
	}

	log.Printf("message sent to chat %s (%d chars)", chatID, len(text))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
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
