package main

import (
	"context"
	"os"
	"testing"

	appdb "equipment-monitoring-api/internal/db"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

// testPool is shared across the package's tests when TEST_DATABASE_URL is
// set. Tests that need it call requireTestDB(t), which skips otherwise —
// so `go test ./...` still passes with no DB configured (e.g. plain CI).
var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		os.Exit(m.Run())
	}
	ctx := context.Background()
	pool, err := appdb.Connect(ctx, url)
	if err != nil {
		panic(err)
	}
	if err := appdb.Migrate(ctx, pool); err != nil {
		panic(err)
	}
	testPool = pool
	code := m.Run()
	pool.Close()
	os.Exit(code)
}

// requireTestDBURL skips the test if no TEST_DATABASE_URL was configured,
// otherwise returns it — for tests that need their own throwaway
// connection (e.g. one they close mid-test) rather than the shared pool.
func requireTestDBURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping DB-backed test")
	}
	return url
}

// requireTestDB skips the test if no TEST_DATABASE_URL was configured,
// otherwise wipes and reseeds the three demo users for test isolation.
func requireTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if testPool == nil {
		t.Skip("TEST_DATABASE_URL not set; skipping DB-backed test")
	}
	ctx := context.Background()
	if _, err := testPool.Exec(ctx,
		`TRUNCATE users, machines, alerts, error_log, metric_history, downtimes, thresholds, sessions RESTART IDENTITY CASCADE`,
	); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	seedTestUsers(t, testPool)
	return testPool
}

// testUserPassword is the plaintext password behind every seeded test user
// (matches the "demo" convention used by the real seed script).
const testUserPassword = "demo"

func seedTestUsers(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	hash, err := bcrypt.GenerateFromPassword([]byte(testUserPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash test password: %v", err)
	}
	users := []struct{ id, name, email, role string }{
		{"u1", "Иван Петров", "operator@demo.com", "operator"},
		{"u2", "Мария Сидорова", "manager@demo.com", "manager"},
		{"u3", "Алексей Иванов", "admin@demo.com", "admin"},
	}
	for _, u := range users {
		if _, err := pool.Exec(ctx, `INSERT INTO users (id, name, email, role, password_hash) VALUES ($1,$2,$3,$4,$5)`,
			u.id, u.name, u.email, u.role, string(hash)); err != nil {
			t.Fatalf("seed user %s: %v", u.id, err)
		}
	}
}
