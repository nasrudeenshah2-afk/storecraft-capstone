package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"storecraft/internal/auth"
	"storecraft/internal/store"
)

const adminToken = "admin-secret"

// noAdmin, as Options.AdminToken in setupFull, starts a server with admin access disabled.
const noAdmin = "\x00no-admin"

type env struct {
	t      *testing.T
	url    string
	dbPath string
	st     *store.Store
	srv    *httptest.Server
	tokenA string
	tokenB string
}

func setup(t *testing.T) *env { return setupWith(t, 0) }

// setupWith starts a server with the given long-poll timeout (0 = default).
func setupWith(t *testing.T, pollTimeout time.Duration) *env {
	return setupFull(t, Options{PollTimeout: pollTimeout}, nil)
}

// setupFull starts a server with the given options; srvCfg, if set, may adjust
// the underlying http.Server (e.g. timeouts) before it starts.
func setupFull(t *testing.T, opts Options, srvCfg func(*http.Server)) *env {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "api.db")
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	e := &env{t: t, dbPath: dbPath, st: st}
	for _, tn := range []struct {
		name string
		tok  *string
	}{{"a", &e.tokenA}, {"b", &e.tokenB}} {
		tok, err := auth.NewToken()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.CreateTenant(ctx, tn.name, auth.HashToken(tok)); err != nil {
			t.Fatal(err)
		}
		*tn.tok = tok
	}
	switch opts.AdminToken {
	case "":
		opts.AdminToken = adminToken
	case noAdmin:
		opts.AdminToken = "" // admin access disabled
	}
	opts.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewUnstartedServer(New(st, opts))
	if srvCfg != nil {
		srvCfg(srv.Config)
	}
	srv.Start()
	t.Cleanup(srv.Close)
	e.url = srv.URL
	e.srv = srv
	return e
}

type resp struct {
	status int
	header http.Header
	body   string
}

// do sends a real HTTP request; and checks the error envelope on every non-2xx.
func (e *env) do(method, path, token, body string, hdr map[string]string) resp {
	e.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, e.url+path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	r := resp{status: res.StatusCode, header: res.Header, body: string(b)}
	if r.status >= 300 {
		e.checkEnvelope(r, "")
	}
	return r
}

func (e *env) checkEnvelope(r resp, wantCode string) {
	e.t.Helper()
	if ct := r.header.Get("Content-Type"); ct != "application/json" {
		e.t.Fatalf("status %d: Content-Type = %q, want application/json (body %q)", r.status, ct, r.body)
	}
	var env struct {
		Error struct{ Code, Message string }
	}
	if err := json.Unmarshal([]byte(r.body), &env); err != nil || env.Error.Code == "" || env.Error.Message == "" {
		e.t.Fatalf("status %d: bad envelope %q (%v)", r.status, r.body, err)
	}
	if wantCode != "" && env.Error.Code != wantCode {
		e.t.Fatalf("code = %q, want %q (body %q)", env.Error.Code, wantCode, r.body)
	}
}

func (e *env) expect(r resp, status int, code string) {
	e.t.Helper()
	if r.status != status {
		e.t.Fatalf("status = %d, want %d (body %q)", r.status, status, r.body)
	}
	if code != "" {
		e.checkEnvelope(r, code)
	}
}

func valueOf(t *testing.T, r resp) (string, int64) {
	t.Helper()
	var b struct {
		Value   json.RawMessage `json:"value"`
		Version int64           `json:"version"`
	}
	if err := json.Unmarshal([]byte(r.body), &b); err != nil {
		t.Fatalf("decode %q: %v", r.body, err)
	}
	return string(b.Value), b.Version
}

func TestUnauthorized(t *testing.T) {
	e := setup(t)
	for name, hdr := range map[string]map[string]string{
		"missing":   nil,
		"wrong":     {"Authorization": "Bearer nope"},
		"malformed": {"Authorization": "Token abc"},
		"empty":     {"Authorization": "Bearer "},
	} {
		for _, route := range []struct{ m, p string }{
			{"GET", "/v1/keys/k"}, {"PUT", "/v1/keys/k"}, {"DELETE", "/v1/keys/k"}, {"GET", "/v1/keys"},
		} {
			r := e.do(route.m, route.p, "", `{"value":1}`, hdr)
			if r.status != 401 {
				t.Fatalf("%s %s %s: status %d, want 401", name, route.m, route.p, r.status)
			}
			e.checkEnvelope(r, "unauthorized")
		}
	}
}

func TestAdminTokenForbiddenOnTenantData(t *testing.T) {
	e := setup(t)
	e.expect(e.do("GET", "/v1/keys", adminToken, "", nil), 403, "forbidden")
	e.expect(e.do("PUT", "/v1/keys/k", adminToken, `{"value":1}`, nil), 403, "forbidden")
}

func TestPutGetDeleteBasics(t *testing.T) {
	e := setup(t)
	r := e.do("PUT", "/v1/keys/greeting", e.tokenA, `{"value":{"hello":"world"}}`, nil)
	e.expect(r, 201, "")
	if r.header.Get("ETag") != `"1"` {
		t.Fatalf("PUT ETag = %q", r.header.Get("ETag"))
	}
	r = e.do("GET", "/v1/keys/greeting", e.tokenA, "", nil)
	e.expect(r, 200, "")
	if v, ver := valueOf(t, r); v != `{"hello":"world"}` || ver != 1 {
		t.Fatalf("GET value=%s version=%d", v, ver)
	}
	if r.header.Get("ETag") != `"1"` {
		t.Fatalf("GET ETag = %q", r.header.Get("ETag"))
	}
	r = e.do("PUT", "/v1/keys/greeting", e.tokenA, `{"value":"again"}`, nil)
	e.expect(r, 200, "")
	if r.header.Get("ETag") != `"2"` {
		t.Fatalf("second PUT ETag = %q", r.header.Get("ETag"))
	}
	e.expect(e.do("DELETE", "/v1/keys/greeting", e.tokenA, "", nil), 204, "")
	e.expect(e.do("GET", "/v1/keys/greeting", e.tokenA, "", nil), 404, "not_found")
	e.expect(e.do("DELETE", "/v1/keys/greeting", e.tokenA, "", nil), 404, "not_found")
}

func TestKeysWithSlashes(t *testing.T) {
	e := setup(t)
	e.expect(e.do("PUT", "/v1/keys/app/config/theme", e.tokenA, `{"value":"dark"}`, nil), 201, "")
	r := e.do("GET", "/v1/keys?prefix=app/", e.tokenA, "", nil)
	if !strings.Contains(r.body, `"key":"app/config/theme"`) {
		t.Fatalf("list = %s", r.body)
	}
}

func TestTenantIsolation(t *testing.T) {
	e := setup(t)
	e.expect(e.do("PUT", "/v1/keys/shared", e.tokenA, `{"value":"A"}`, nil), 201, "")
	// B cannot see A's key.
	e.expect(e.do("GET", "/v1/keys/shared", e.tokenB, "", nil), 404, "not_found")
	if r := e.do("GET", "/v1/keys", e.tokenB, "", nil); !strings.Contains(r.body, `"keys":[]`) {
		t.Fatalf("B list leaked: %s", r.body)
	}
	// B writes the same key name: independent, starts at version 1.
	e.expect(e.do("PUT", "/v1/keys/shared", e.tokenB, `{"value":"B"}`, nil), 201, "")
	ra := e.do("GET", "/v1/keys/shared", e.tokenA, "", nil)
	if v, _ := valueOf(t, ra); v != `"A"` {
		t.Fatalf("A's value = %s", v)
	}
	// B deleting must not remove A's key; B's CAS against A's version must not touch A.
	e.expect(e.do("DELETE", "/v1/keys/shared", e.tokenB, "", nil), 204, "")
	e.expect(e.do("GET", "/v1/keys/shared", e.tokenA, "", nil), 200, "")
}

func TestCompareAndSwap(t *testing.T) {
	e := setup(t)
	e.expect(e.do("PUT", "/v1/keys/doc", e.tokenA, `{"value":"v1"}`, nil), 201, "")

	// Success, with both quoted and bare If-Match forms.
	r := e.do("PUT", "/v1/keys/doc", e.tokenA, `{"value":"v2"}`, map[string]string{"If-Match": `"1"`})
	e.expect(r, 200, "")
	if r.header.Get("ETag") != `"2"` {
		t.Fatalf("CAS ETag = %q", r.header.Get("ETag"))
	}
	e.expect(e.do("PUT", "/v1/keys/doc", e.tokenA, `{"value":"v3"}`, map[string]string{"If-Match": "2"}), 200, "")

	// Conflict: stale version; value unchanged.
	e.expect(e.do("PUT", "/v1/keys/doc", e.tokenA, `{"value":"lost"}`, map[string]string{"If-Match": "1"}), 412, "precondition_failed")
	r = e.do("GET", "/v1/keys/doc", e.tokenA, "", nil)
	if v, ver := valueOf(t, r); v != `"v3"` || ver != 3 {
		t.Fatalf("after 412: value=%s version=%d", v, ver)
	}
	// If-Match on a missing key is also a failed precondition.
	e.expect(e.do("PUT", "/v1/keys/ghost", e.tokenA, `{"value":1}`, map[string]string{"If-Match": "1"}), 412, "precondition_failed")
	// Garbage If-Match.
	e.expect(e.do("PUT", "/v1/keys/doc", e.tokenA, `{"value":1}`, map[string]string{"If-Match": "abc"}), 400, "invalid_request")
}

func TestIfNoneMatch(t *testing.T) {
	e := setup(t)
	star := map[string]string{"If-None-Match": "*"}
	e.expect(e.do("PUT", "/v1/keys/once", e.tokenA, `{"value":"first"}`, star), 201, "")
	e.expect(e.do("PUT", "/v1/keys/once", e.tokenA, `{"value":"second"}`, star), 412, "precondition_failed")
	r := e.do("GET", "/v1/keys/once", e.tokenA, "", nil)
	if v, ver := valueOf(t, r); v != `"first"` || ver != 1 {
		t.Fatalf("value=%s version=%d", v, ver)
	}
	// Another tenant may still create the same key name.
	e.expect(e.do("PUT", "/v1/keys/once", e.tokenB, `{"value":"b"}`, star), 201, "")
	e.expect(e.do("PUT", "/v1/keys/x", e.tokenA, `{"value":1}`, map[string]string{"If-None-Match": "1"}), 400, "invalid_request")
	e.expect(e.do("PUT", "/v1/keys/x", e.tokenA, `{"value":1}`, map[string]string{"If-None-Match": "*", "If-Match": "1"}), 400, "invalid_request")
}

func TestConcurrentIfNoneMatchOneWinner(t *testing.T) {
	e := setup(t)
	const n = 20
	statuses := make([]int, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			req, _ := http.NewRequest("PUT", e.url+"/v1/keys/race", strings.NewReader(fmt.Sprintf(`{"value":%d}`, i)))
			req.Header.Set("Authorization", "Bearer "+e.tokenA)
			req.Header.Set("If-None-Match", "*")
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				statuses[i] = -1
				return
			}
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			statuses[i] = res.StatusCode
		}(i)
	}
	close(start)
	wg.Wait()
	created, failed := 0, 0
	for _, s := range statuses {
		switch s {
		case 201:
			created++
		case 412:
			failed++
		default:
			t.Fatalf("unexpected status %d in %v", s, statuses)
		}
	}
	if created != 1 || failed != n-1 {
		t.Fatalf("created=%d failed=%d, want 1/%d", created, failed, n-1)
	}
}

func TestListSortedWithPrefix(t *testing.T) {
	e := setup(t)
	for _, k := range []string{"b", "app-2", "app-1", "a", "app-3"} {
		e.expect(e.do("PUT", "/v1/keys/"+k, e.tokenA, `{"value":"`+k+`"}`, nil), 201, "")
	}
	keys := func(path string) []string {
		r := e.do("GET", path, e.tokenA, "", nil)
		e.expect(r, 200, "")
		var b struct{ Keys []struct{ Key string } }
		if err := json.Unmarshal([]byte(r.body), &b); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, k := range b.Keys {
			out = append(out, k.Key)
		}
		return out
	}
	if got := fmt.Sprint(keys("/v1/keys")); got != "[a app-1 app-2 app-3 b]" {
		t.Fatalf("all = %s", got)
	}
	if got := fmt.Sprint(keys("/v1/keys?prefix=app-")); got != "[app-1 app-2 app-3]" {
		t.Fatalf("prefix = %s", got)
	}
	if got := keys("/v1/keys?prefix=zzz"); len(got) != 0 {
		t.Fatalf("none = %v", got)
	}
}

func TestErrorEnvelopeUnknownPathsAndMethods(t *testing.T) {
	e := setup(t)
	// Unknown paths: 404 envelope, with or without a token.
	for _, p := range []string{"/", "/nope", "/v1", "/v1/other", "/v1/keys/", "/v2/keys/k"} {
		e.expect(e.do("GET", p, "", "", nil), 404, "not_found")
		e.expect(e.do("GET", p, e.tokenA, "", nil), 404, "not_found")
	}
	// Wrong methods: 405 + Allow + envelope.
	for _, c := range []struct{ method, path, allow string }{
		{"POST", "/v1/keys/k", "GET, PUT, DELETE"},
		{"PATCH", "/v1/keys/k", "GET, PUT, DELETE"},
		{"POST", "/v1/keys", "GET"},
		{"PUT", "/v1/keys", "GET"},
		{"DELETE", "/v1/keys", "GET"},
	} {
		r := e.do(c.method, c.path, e.tokenA, `{"value":1}`, nil)
		e.expect(r, 405, "method_not_allowed")
		if got := r.header.Get("Allow"); got != c.allow {
			t.Fatalf("%s %s: Allow = %q, want %q", c.method, c.path, got, c.allow)
		}
	}
}

func TestInvalidBodiesChangeNothing(t *testing.T) {
	e := setup(t)
	e.expect(e.do("PUT", "/v1/keys/k", e.tokenA, `{"value":"orig"}`, nil), 201, "")

	big := `{"value":"` + strings.Repeat("a", maxBodyBytes) + `"}`
	for name, body := range map[string]string{
		"not json":      `{nope`,
		"empty":         ``,
		"missing value": `{"other":1}`,
		"empty object":  `{}`,
		"array":         `[1,2]`,
		"trailing data": `{"value":1} {"value":2}`,
		"too large":     big,
	} {
		r := e.do("PUT", "/v1/keys/k", e.tokenA, body, nil)
		if r.status != 400 {
			t.Fatalf("%s: status %d, want 400 (%q)", name, r.status, truncate(r.body))
		}
		e.checkEnvelope(r, "invalid_request")
	}
	// Invalid body on a new key must not create it either.
	e.expect(e.do("PUT", "/v1/keys/fresh", e.tokenA, `{"nope":1}`, nil), 400, "invalid_request")
	e.expect(e.do("GET", "/v1/keys/fresh", e.tokenA, "", nil), 404, "not_found")

	r := e.do("GET", "/v1/keys/k", e.tokenA, "", nil)
	if v, ver := valueOf(t, r); v != `"orig"` || ver != 1 {
		t.Fatalf("value changed by invalid bodies: %s v%d", v, ver)
	}
}

func TestBodyJustUnderLimitAccepted(t *testing.T) {
	e := setup(t)
	body := `{"value":"` + strings.Repeat("a", maxBodyBytes-100) + `"}`
	e.expect(e.do("PUT", "/v1/keys/big", e.tokenA, body, nil), 201, "")
}

func truncate(s string) string {
	if len(s) > 120 {
		return s[:120] + "..."
	}
	return s
}
