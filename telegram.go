package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// AlertNotifier receives alerts that were just persisted. Implementations
// must not block: Tick() calls it on the hot path.
type AlertNotifier interface {
	Notify(a Alert)
}

const (
	defaultTelegramBaseURL = "https://api.telegram.org"
	telegramQueueSize      = 64
	telegramMaxAttempts    = 3
	telegramMessageLimit   = 4000 // Telegram caps a message at 4096 characters
)

// TelegramStatus is what the settings page shows an admin. It never
// contains the bot token or chat id.
type TelegramStatus struct {
	Enabled     bool       `json:"enabled"`
	MinSeverity string     `json:"minSeverity"`
	LastSentAt  *time.Time `json:"lastSentAt,omitempty"`
	LastError   string     `json:"lastError,omitempty"`
}

// TelegramNotifier pushes alerts to a Telegram chat. Configured entirely
// by environment (TELEGRAM_BOT_TOKEN, TELEGRAM_CHAT_ID, optional
// TELEGRAM_MIN_SEVERITY = critical|warning|info, default critical;
// TELEGRAM_API_BASE overrides https://api.telegram.org, e.g. for a
// self-hosted Bot API server); with
// no token/chat it is disabled and Notify is a no-op.
type TelegramNotifier struct {
	token       string
	chatID      string
	baseURL     string
	minSeverity AlertSeverity
	client      *http.Client
	// retryDelay is the base backoff between attempts (doubles each time).
	retryDelay time.Duration

	queue chan Alert

	mu       sync.Mutex
	lastSent *time.Time
	lastErr  string
}

func NewTelegramNotifierFromEnv() *TelegramNotifier {
	n := &TelegramNotifier{
		token:       strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN")),
		chatID:      strings.TrimSpace(os.Getenv("TELEGRAM_CHAT_ID")),
		baseURL:     defaultTelegramBaseURL,
		minSeverity: SeverityCritical,
		client:      &http.Client{Timeout: 10 * time.Second},
		retryDelay:  time.Second,
		queue:       make(chan Alert, telegramQueueSize),
	}
	if v := strings.TrimSpace(os.Getenv("TELEGRAM_API_BASE")); v != "" {
		n.baseURL = strings.TrimRight(v, "/")
	}
	switch AlertSeverity(strings.ToLower(strings.TrimSpace(os.Getenv("TELEGRAM_MIN_SEVERITY")))) {
	case SeverityWarning:
		n.minSeverity = SeverityWarning
	case SeverityInfo:
		n.minSeverity = SeverityInfo
	}
	return n
}

func (n *TelegramNotifier) Enabled() bool {
	return n != nil && n.token != "" && n.chatID != ""
}

func severityRank(s AlertSeverity) int {
	switch s {
	case SeverityCritical:
		return 3
	case SeverityWarning:
		return 2
	default:
		return 1
	}
}

// Notify queues an alert at or above the configured severity. It never
// blocks: if the queue is full (Telegram unreachable for a long time) the
// alert is dropped with a log line — the alert itself is still in the DB
// and the web UI.
func (n *TelegramNotifier) Notify(a Alert) {
	if !n.Enabled() || severityRank(a.Severity) < severityRank(n.minSeverity) {
		return
	}
	select {
	case n.queue <- a:
	default:
		logger.Warn("telegram queue full, dropping notification", "alertId", a.ID)
	}
}

// Run delivers queued alerts until ctx is cancelled.
func (n *TelegramNotifier) Run(ctx context.Context) {
	if !n.Enabled() {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case a := <-n.queue:
			if err := n.sendWithRetry(ctx, formatTelegramAlert(a)); err != nil {
				logger.Error("telegram notification failed", "alertId", a.ID, "error", err)
			}
		}
	}
}

// SendTest sends a one-off message so an admin can verify the setup.
func (n *TelegramNotifier) SendTest(ctx context.Context) error {
	if !n.Enabled() {
		return errTelegramDisabled
	}
	return n.sendWithRetry(ctx, "Проверка связи: уведомления Equipment Monitoring подключены.")
}

var errTelegramDisabled = errors.New("telegram notifications are not configured")

func (n *TelegramNotifier) Status() TelegramStatus {
	n.mu.Lock()
	defer n.mu.Unlock()
	st := TelegramStatus{Enabled: n.Enabled(), MinSeverity: string(n.minSeverity), LastError: n.lastErr}
	if n.lastSent != nil {
		t := *n.lastSent
		st.LastSentAt = &t
	}
	return st
}

func (n *TelegramNotifier) record(err error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if err == nil {
		now := time.Now()
		n.lastSent = &now
		n.lastErr = ""
		return
	}
	n.lastErr = err.Error()
}

// permanentError marks a failure that retrying cannot fix (bad token,
// wrong chat id…).
type permanentError struct{ error }

func (n *TelegramNotifier) sendWithRetry(ctx context.Context, text string) error {
	var err error
	delay := n.retryDelay
	for attempt := 1; attempt <= telegramMaxAttempts; attempt++ {
		err = n.send(ctx, text)
		if err == nil {
			n.record(nil)
			return nil
		}
		var perm permanentError
		if errors.As(err, &perm) || attempt == telegramMaxAttempts {
			break
		}
		select {
		case <-ctx.Done():
			n.record(ctx.Err())
			return ctx.Err()
		case <-time.After(delay):
			delay *= 2
		}
	}
	n.record(err)
	return err
}

// send makes one delivery attempt. Errors are scrubbed of the bot token —
// net/http embeds the full request URL (which contains it) in its errors.
func (n *TelegramNotifier) send(ctx context.Context, text string) error {
	if len(text) > telegramMessageLimit {
		text = text[:telegramMessageLimit]
	}
	body, _ := json.Marshal(map[string]string{"chat_id": n.chatID, "text": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.baseURL+"/bot"+n.token+"/sendMessage", bytes.NewReader(body))
	if err != nil {
		return n.scrub(err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := n.client.Do(req)
	if err != nil {
		return n.scrub(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}

	var apiErr struct {
		Description string `json:"description"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	_ = json.Unmarshal(raw, &apiErr)
	err = fmt.Errorf("telegram: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(apiErr.Description))
	// 429 and 5xx are worth retrying; other 4xx (401 bad token, 400 bad
	// chat) will fail identically every time.
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return err
	}
	return permanentError{err}
}

func (n *TelegramNotifier) scrub(err error) error {
	if n.token == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), n.token, "***"))
}

func formatTelegramAlert(a Alert) string {
	label := map[AlertSeverity]string{SeverityCritical: "КРИТИЧНО", SeverityWarning: "Предупреждение", SeverityInfo: "Информация"}[a.Severity]
	if label == "" {
		label = string(a.Severity)
	}
	return fmt.Sprintf("%s: %s\n%s", label, a.Message, a.Timestamp.Format("02.01.2006 15:04"))
}

// ── HTTP handlers (admin only) ───────────────────────────────────────

func (s *Server) handleTelegramStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.telegram.Status())
}

func (s *Server) handleTelegramTest(w http.ResponseWriter, r *http.Request) {
	if !s.telegram.Enabled() {
		writeError(w, http.StatusConflict, "Telegram не настроен: задайте TELEGRAM_BOT_TOKEN и TELEGRAM_CHAT_ID на сервере")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := s.telegram.SendTest(ctx); err != nil {
		writeError(w, http.StatusBadGateway, "Не удалось отправить сообщение: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.telegram.Status())
}
