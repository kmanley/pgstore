package pgstore

import (
	"net/http"
	"os"
	"testing"

	"github.com/gorilla/sessions"
)

func TestCleanup(t *testing.T) {
	dsn := os.Getenv("PGSTORE_TEST_CONN")
	if dsn == "" {
		t.Skip("This test requires a real database.")
	}

	opts := &sessions.Options{
		Path:   "/",
		MaxAge: 86400 * 30,
	}

	p, err := NewPGStore(dsn, opts, []byte(secret))
	if err != nil {
		t.Fatal("Failed to get store", err)
	}

	defer p.Close()
	// Start the cleanup goroutine.
	//defer p.StopCleanup(p.Cleanup(time.Millisecond * 500))

	req, err := http.NewRequest("GET", "http://www.example.com", nil)
	if err != nil {
		t.Fatal("Failed to create request", err)
	}

	session, err := p.Get(req, "newsess")
	if err != nil {
		t.Fatal("Failed to create session", err)
	}

	// Expire the session.
	session.Options.MaxAge = 1

	m := make(http.Header)
	if err = p.Save(req, headerOnlyResponseWriter(m), session); err != nil {
		t.Fatal("failed to save session:", err.Error())
	}

	// Give the ticker a moment to run.
	//time.Sleep(time.Millisecond * 1500)

	p.DeleteExpired()

	// SELECT expired sessions. We should get a count of zero back.
	var count int
	err = p.db.QueryRow("SELECT count(*) FROM http_sessions WHERE expires_on < now()").Scan(&count)
	if err != nil {
		t.Fatalf("failed to select expired sessions from DB: %v", err)
	}

	if count > 0 {
		t.Fatalf("ticker did not delete expired sessions: want 0 got %v", count)
	}
}
