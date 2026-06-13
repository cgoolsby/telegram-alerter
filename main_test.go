package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testServer(t *testing.T, apiBase string) *server {
	t.Helper()
	return newServer(config{
		botToken:  "test-bot-token",
		chatID:    "12345",
		authToken: "test-auth-token",
		port:      "8080",
		apiBase:   apiBase,
	})
}

// fakeTelegram returns an httptest server that records the last sendMessage
// payload and responds with the given body and status.
func fakeTelegram(t *testing.T, status int, body string, captured *telegramSendMessage) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/bottest-bot-token/") {
			t.Errorf("unexpected telegram path: %s", r.URL.Path)
		}
		if captured != nil {
			if err := json.NewDecoder(r.Body).Decode(captured); err != nil {
				t.Errorf("failed to decode telegram payload: %v", err)
			}
		}
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
}

func doSend(s *server, authHeader, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/send", strings.NewReader(body))
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	return rec
}

func TestSendRejectsMissingAuth(t *testing.T) {
	s := testServer(t, "http://unused")
	rec := doSend(s, "", `{"message":"hi"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestSendRejectsWrongToken(t *testing.T) {
	s := testServer(t, "http://unused")
	rec := doSend(s, "Bearer wrong-token", `{"message":"hi"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestSendRejectsEmptyMessage(t *testing.T) {
	s := testServer(t, "http://unused")
	rec := doSend(s, "Bearer test-auth-token", `{"message":"  "}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestSendRejectsInvalidJSON(t *testing.T) {
	s := testServer(t, "http://unused")
	rec := doSend(s, "Bearer test-auth-token", `not json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestSendForwardsToTelegram(t *testing.T) {
	var captured telegramSendMessage
	tg := fakeTelegram(t, http.StatusOK, `{"ok":true}`, &captured)
	defer tg.Close()

	s := testServer(t, tg.URL)
	rec := doSend(s, "Bearer test-auth-token", `{"message":"disk full","parse_mode":"HTML","silent":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if captured.ChatID != "12345" {
		t.Errorf("expected default chat_id 12345, got %q", captured.ChatID)
	}
	if captured.Text != "disk full" {
		t.Errorf("expected text %q, got %q", "disk full", captured.Text)
	}
	if captured.ParseMode != "HTML" {
		t.Errorf("expected parse_mode HTML, got %q", captured.ParseMode)
	}
	if !captured.DisableNotification {
		t.Error("expected disable_notification to be true")
	}
}

func TestSendChatIDOverride(t *testing.T) {
	var captured telegramSendMessage
	tg := fakeTelegram(t, http.StatusOK, `{"ok":true}`, &captured)
	defer tg.Close()

	s := testServer(t, tg.URL)
	rec := doSend(s, "Bearer test-auth-token", `{"message":"hi","chat_id":"99999"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if captured.ChatID != "99999" {
		t.Errorf("expected chat_id override 99999, got %q", captured.ChatID)
	}
}

func TestSendTruncatesLongMessage(t *testing.T) {
	var captured telegramSendMessage
	tg := fakeTelegram(t, http.StatusOK, `{"ok":true}`, &captured)
	defer tg.Close()

	s := testServer(t, tg.URL)
	long := strings.Repeat("a", telegramMaxMessageLen+500)
	body, _ := json.Marshal(sendRequest{Message: long})
	rec := doSend(s, "Bearer test-auth-token", string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if len(captured.Text) != telegramMaxMessageLen {
		t.Errorf("expected text truncated to %d chars, got %d", telegramMaxMessageLen, len(captured.Text))
	}
}

func TestSendPassesThroughTelegramError(t *testing.T) {
	tg := fakeTelegram(t, http.StatusBadRequest, `{"ok":false,"description":"Bad Request: chat not found"}`, nil)
	defer tg.Close()

	s := testServer(t, tg.URL)
	rec := doSend(s, "Bearer test-auth-token", `{"message":"hi"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "chat not found") {
		t.Errorf("expected telegram error description in body, got %s", rec.Body.String())
	}
}

func TestHealthz(t *testing.T) {
	s := testServer(t, "http://unused")
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}
