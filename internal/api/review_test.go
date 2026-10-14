package api

// Regression tests for findings of the independent review (B13 triage).

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"testing"
	"time"
)

// R8: a client that disconnects while its write waits for the write lock is
// not a server error: no ERROR log, no 500 in the request metrics.
func TestReviewR8ClientDisconnectIsNotA500(t *testing.T) {
	e := setupWith(t, 300*time.Millisecond)
	e.put(e.tokenA, "warm", `{"value":1}`)

	// Hold SQLite's write lock from another connection, as another process could.
	db, err := sql.Open("sqlite", "file:"+e.dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "PUT", e.url+"/v1/keys/k", strings.NewReader(`{"value":1}`))
	req.Header.Set("Authorization", "Bearer "+e.tokenA)
	if res, err := http.DefaultClient.Do(req); err == nil {
		res.Body.Close()
		t.Fatalf("the request finished (%d) while the write lock was held", res.StatusCode)
	}
	time.Sleep(700 * time.Millisecond) // let the server notice the disconnect
	conn.ExecContext(context.Background(), `ROLLBACK`)

	_, sc := e.scrape()
	if v, ok := sc.Value(reqTotal, reqLabels("PUT", "/v1/keys/{key}", "500")); ok && v > 0 {
		t.Fatalf("a client disconnect was counted as %v server error(s)", v)
	}
}
