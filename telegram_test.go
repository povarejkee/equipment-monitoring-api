package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testBotToken = "123456:SECRET-test-token"

func newTestTelegram(baseURL string, min AlertSeverity) *TelegramNotifier {
	return &TelegramNotifier{
		token: testBotToken, chatID: "-100500", baseURL: baseURL, minSeverity: min,
		client: &http.Client{Timeout: 2 * time.Second}, retryDelay: time.Millisecond,
		queue: make(chan Alert, telegramQueueSize),
	}
}

type tgRecorder struct {
	mu       sync.Mutex
	paths    []string
	messages []map[string]string
}

func (r *tgRecorder) handler(statuses ...int) http.HandlerFunc {
	var call int32
	return func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		var m map[string]string
		_ = json.Unmarshal(body, &m)
		r.mu.Lock()
		r.paths = append(r.paths, req.URL.Path)
		r.messages = append(r.messages, m)
		r.mu.Unlock()
		i := int(atomic.AddInt32(&call, 1)) - 1
		status := http.StatusOK
		if i < len(statuses) {
			status = statuses[i]
		}
		w.WriteHeader(status)
		if status != http.StatusOK {
			_, _ = w.Write([]byte(`{"ok":false,"description":"nope"}`))
		}
	}
}

func (r *tgRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.messages)
}

func criticalAlert() Alert {
	return Alert{ID: "a1", MachineName: "ЧПУ-01", Severity: SeverityCritical,
		Message: "ЧПУ-01: Температура превысила критический порог (96.0 °C)", Timestamp: time.Date(2026, 10, 5, 12, 30, 0, 0, time.UTC)}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

func TestTelegram_DisabledWithoutConfigIsANoOp(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "")
	t.Setenv("TELEGRAM_CHAT_ID", "")
	n := NewTelegramNotifierFromEnv()

	if n.Enabled() {
		t.Fatal("must be disabled with no token/chat")
	}
	n.Notify(criticalAlert())
	if len(n.queue) != 0 {
		t.Error("a disabled notifier must not queue anything")
	}
	if err := n.SendTest(context.Background()); err != errTelegramDisabled {
		t.Errorf("SendTest = %v, want errTelegramDisabled", err)
	}
}

func TestTelegram_EnvConfigAndSeverityParsing(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "tok")
	t.Setenv("TELEGRAM_CHAT_ID", "42")
	if n := NewTelegramNotifierFromEnv(); !n.Enabled() || n.minSeverity != SeverityCritical {
		t.Errorf("default = enabled:%v min:%v, want enabled critical-only", n.Enabled(), n.minSeverity)
	}
	t.Setenv("TELEGRAM_MIN_SEVERITY", "Warning")
	if n := NewTelegramNotifierFromEnv(); n.minSeverity != SeverityWarning {
		t.Errorf("min severity = %v, want warning (case-insensitive)", n.minSeverity)
	}
	t.Setenv("TELEGRAM_MIN_SEVERITY", "garbage")
	if n := NewTelegramNotifierFromEnv(); n.minSeverity != SeverityCritical {
		t.Errorf("an unknown value must fall back to critical, got %v", n.minSeverity)
	}
}

func TestTelegram_DeliversCriticalAlertToTheBotEndpoint(t *testing.T) {
	rec := &tgRecorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()
	n := newTestTelegram(srv.URL, SeverityCritical)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go n.Run(ctx)

	n.Notify(criticalAlert())
	waitFor(t, func() bool { return rec.count() == 1 })

	if rec.paths[0] != "/bot"+testBotToken+"/sendMessage" {
		t.Errorf("path = %q", rec.paths[0])
	}
	m := rec.messages[0]
	if m["chat_id"] != "-100500" {
		t.Errorf("chat_id = %q", m["chat_id"])
	}
	for _, want := range []string{"КРИТИЧНО", "ЧПУ-01", "96.0", "05.10.2026 12:30"} {
		if !strings.Contains(m["text"], want) {
			t.Errorf("message %q is missing %q", m["text"], want)
		}
	}
	waitFor(t, func() bool { return n.Status().LastSentAt != nil })
}

func TestTelegram_SeverityFilter(t *testing.T) {
	rec := &tgRecorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()
	warning := criticalAlert()
	warning.Severity = SeverityWarning

	critOnly := newTestTelegram(srv.URL, SeverityCritical)
	critOnly.Notify(warning)
	if len(critOnly.queue) != 0 {
		t.Error("a warning must not be queued when only criticals are wanted")
	}
	critOnly.Notify(criticalAlert())
	if len(critOnly.queue) != 1 {
		t.Error("a critical must be queued")
	}

	withWarnings := newTestTelegram(srv.URL, SeverityWarning)
	withWarnings.Notify(warning)
	if len(withWarnings.queue) != 1 {
		t.Error("a warning must be queued when the minimum is warning")
	}
}

func TestTelegram_RetriesTransientFailuresThenSucceeds(t *testing.T) {
	rec := &tgRecorder{}
	srv := httptest.NewServer(rec.handler(http.StatusInternalServerError, http.StatusTooManyRequests, http.StatusOK))
	defer srv.Close()
	n := newTestTelegram(srv.URL, SeverityCritical)

	if err := n.sendWithRetry(context.Background(), "x"); err != nil {
		t.Fatalf("sendWithRetry: %v", err)
	}
	if rec.count() != 3 {
		t.Errorf("attempts = %d, want 3 (500, 429, then 200)", rec.count())
	}
	if st := n.Status(); st.LastError != "" || st.LastSentAt == nil {
		t.Errorf("status after recovery = %+v, want no error and a send time", st)
	}
}

func TestTelegram_GivesUpAfterMaxAttempts(t *testing.T) {
	rec := &tgRecorder{}
	srv := httptest.NewServer(rec.handler(500, 500, 500, 500, 500))
	defer srv.Close()
	n := newTestTelegram(srv.URL, SeverityCritical)

	if err := n.sendWithRetry(context.Background(), "x"); err == nil {
		t.Fatal("want an error after exhausting retries")
	}
	if rec.count() != telegramMaxAttempts {
		t.Errorf("attempts = %d, want %d", rec.count(), telegramMaxAttempts)
	}
	if !strings.Contains(n.Status().LastError, "HTTP 500") {
		t.Errorf("lastError = %q", n.Status().LastError)
	}
}

func TestTelegram_DoesNotRetryAPermanentFailure(t *testing.T) {
	rec := &tgRecorder{}
	srv := httptest.NewServer(rec.handler(http.StatusUnauthorized))
	defer srv.Close()
	n := newTestTelegram(srv.URL, SeverityCritical)

	err := n.sendWithRetry(context.Background(), "x")

	if err == nil || rec.count() != 1 {
		t.Fatalf("err=%v attempts=%d, want one failed attempt (a bad token never recovers)", err, rec.count())
	}
}

func TestTelegram_TokenNeverLeaksIntoErrorsOrStatus(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // connection refused: net/http's error text embeds the URL, and so the token
	n := newTestTelegram(url, SeverityCritical)

	err := n.sendWithRetry(context.Background(), "x")
	if err == nil {
		t.Fatal("want a network error")
	}
	if strings.Contains(err.Error(), testBotToken) {
		t.Errorf("error leaks the bot token: %v", err)
	}
	blob, _ := json.Marshal(n.Status())
	if strings.Contains(string(blob), testBotToken) || strings.Contains(string(blob), "-100500") {
		t.Errorf("status JSON leaks credentials: %s", blob)
	}
}

func TestTelegram_NotifyNeverBlocksWhenTheQueueIsFull(t *testing.T) {
	n := newTestTelegram("http://unused", SeverityCritical)
	n.queue = make(chan Alert, 2)

	done := make(chan struct{})
	go func() {
		for i := 0; i < 50; i++ {
			n.Notify(criticalAlert())
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Notify blocked on a full queue — it would stall Tick()")
	}
	if len(n.queue) != 2 {
		t.Errorf("queue = %d, want it capped at 2", len(n.queue))
	}
}

// ── Tick integration ─────────────────────────────────────────────────

type fakeNotifier struct {
	mu     sync.Mutex
	alerts []Alert
}

func (f *fakeNotifier) Notify(a Alert) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.alerts = append(f.alerts, a)
}

func (f *fakeNotifier) all() []Alert {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Alert(nil), f.alerts...)
}

func seedHotMachine(t *testing.T, temp float64) {
	t.Helper()
	mustExecTest(t, testPool, `INSERT INTO thresholds (metric, label, unit, warning_value, critical_value) VALUES
		('temperature', 'Температура', '°C', 80, 95)`)
	mustExecTest(t, testPool, `INSERT INTO machines
		(id, name, type, location, status, temperature, load, output, uptime, power_consumption, vibration, last_updated)
		VALUES ('m1', 'ЧПУ-01', 'cnc', 'Цех 1', 'running', $1, 50, 20, 10, 20, 1, now())`, temp)
}

func TestTick_NotifiesOnceForANewCriticalAlert(t *testing.T) {
	requireTestDB(t)
	seedHotMachine(t, 96)
	store := NewStore(testPool)
	fake := &fakeNotifier{}
	store.SetNotifier(fake)

	if _, err := store.Tick(); err != nil {
		t.Fatal(err)
	}
	got := fake.all()
	if len(got) != 1 || got[0].Severity != SeverityCritical || got[0].MachineID != "m1" {
		t.Fatalf("notified = %+v, want exactly one critical alert for m1", got)
	}

	if _, err := store.Tick(); err != nil {
		t.Fatal(err)
	}
	if n := len(fake.all()); n != 1 {
		t.Errorf("a still-open alert was announced again: %d notifications", n)
	}
}

func TestTick_AWarningDoesNotHideALaterCritical(t *testing.T) {
	requireTestDB(t)
	seedHotMachine(t, 96)
	// An unacknowledged *warning* for the same metric is already open.
	mustExecTest(t, testPool, `INSERT INTO alerts
		(id, machine_id, machine_name, type, severity, message, metric_name, current_value, threshold_value, timestamp)
		VALUES ('old-warning', 'm1', 'ЧПУ-01', 'threshold_exceeded', 'warning', 'x', 'temperature', 85, 80, now() - interval '1 hour')`)
	store := NewStore(testPool)
	fake := &fakeNotifier{}
	store.SetNotifier(fake)

	if _, err := store.Tick(); err != nil {
		t.Fatal(err)
	}

	got := fake.all()
	if len(got) != 1 || got[0].Severity != SeverityCritical {
		t.Fatalf("notified = %+v, want the escalation to critical to be raised and announced", got)
	}
}

func TestTick_AnOpenWarningStillSuppressesAnotherWarning(t *testing.T) {
	requireTestDB(t)
	seedHotMachine(t, 85) // warning range only
	mustExecTest(t, testPool, `INSERT INTO alerts
		(id, machine_id, machine_name, type, severity, message, metric_name, current_value, threshold_value, timestamp)
		VALUES ('old-warning', 'm1', 'ЧПУ-01', 'threshold_exceeded', 'warning', 'x', 'temperature', 85, 80, now() - interval '1 hour')`)
	store := NewStore(testPool)
	fake := &fakeNotifier{}
	store.SetNotifier(fake)

	if _, err := store.Tick(); err != nil {
		t.Fatal(err)
	}
	if n := len(fake.all()); n != 0 {
		t.Errorf("duplicate warning created: %d new alerts", n)
	}
}

// ── handlers ─────────────────────────────────────────────────────────

func TestTelegramHandlers_StatusAndTest(t *testing.T) {
	requireTestDB(t)
	rec := &tgRecorder{}
	tg := httptest.NewServer(rec.handler())
	defer tg.Close()
	srv := NewServer(NewStore(testPool), NewAuthManager(testPool), NewHub())

	// Not configured.
	srv.telegram = &TelegramNotifier{queue: make(chan Alert, 1)}
	w := httptest.NewRecorder()
	srv.handleTelegramTest(w, httptest.NewRequest(http.MethodPost, "/api/notifications/telegram/test", nil))
	if w.Code != http.StatusConflict {
		t.Errorf("unconfigured test = %d, want 409", w.Code)
	}

	// Configured.
	srv.telegram = newTestTelegram(tg.URL, SeverityCritical)
	w = httptest.NewRecorder()
	srv.handleTelegramTest(w, httptest.NewRequest(http.MethodPost, "/api/notifications/telegram/test", nil))
	if w.Code != http.StatusOK || rec.count() != 1 {
		t.Fatalf("test = %d with %d messages, want 200 and 1 message", w.Code, rec.count())
	}

	w = httptest.NewRecorder()
	srv.handleTelegramStatus(w, httptest.NewRequest(http.MethodGet, "/api/notifications/telegram", nil))
	var st TelegramStatus
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil || !st.Enabled || st.LastSentAt == nil {
		t.Errorf("status = %+v (%v), want enabled with a last-sent time", st, err)
	}
	if strings.Contains(w.Body.String(), testBotToken) {
		t.Error("status response leaks the bot token")
	}
}

func TestTelegramHandlers_AreAdminOnly(t *testing.T) {
	requireTestDB(t)
	srv := NewServer(NewStore(testPool), NewAuthManager(testPool), NewHub())
	guarded := requireRole(RoleAdmin, srv.handleTelegramStatus)
	for _, role := range []UserRole{RoleOperator, RoleManager} {
		w := httptest.NewRecorder()
		guarded(w, withUser(httptest.NewRequest(http.MethodGet, "/", nil), &User{ID: "x", Role: role}))
		if w.Code != http.StatusForbidden {
			t.Errorf("%s status = %d, want 403", role, w.Code)
		}
	}
}
