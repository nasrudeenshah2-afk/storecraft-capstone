package main

import (
	"net/http"
	"testing"
)

// Whole-request timeouts would kill long-poll and SSE connections.
func TestHTTPServerHasNoWholeRequestTimeouts(t *testing.T) {
	srv := newHTTPServer(":0", http.NotFoundHandler())
	if srv.WriteTimeout != 0 || srv.ReadTimeout != 0 {
		t.Fatalf("WriteTimeout=%v ReadTimeout=%v, want both 0", srv.WriteTimeout, srv.ReadTimeout)
	}
	if srv.ReadHeaderTimeout <= 0 {
		t.Fatal("ReadHeaderTimeout should be set to protect against slow headers")
	}
}
