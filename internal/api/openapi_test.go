package api

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The OpenAPI document is checked as plain text (no YAML dependency). It relies
// on the document's layout: path keys at 2 spaces, methods at 4, the
// "responses:" block at 6 and status codes at 8.
const openAPIPath = "../../api/openapi.yaml"

var (
	pathLineRE   = regexp.MustCompile(`^  (/\S*):\s*$`)
	methodLineRE = regexp.MustCompile(`^    (get|put|post|delete|patch|head|options|trace):\s*$`)
	codeLineRE   = regexp.MustCompile(`^        '(\d{3})':\s*$`)
)

type openAPIDoc struct {
	text string
	// ops maps "METHOD /path" to its documented response codes.
	ops map[string]map[int]bool
}

func loadOpenAPI(t *testing.T) *openAPIDoc {
	t.Helper()
	b, err := os.ReadFile(openAPIPath)
	if err != nil {
		t.Fatalf("read OpenAPI document: %v", err)
	}
	doc := &openAPIDoc{text: string(b), ops: map[string]map[int]bool{}}

	inPaths, inResponses := false, false
	var path, op string
	for _, line := range strings.Split(strings.ReplaceAll(doc.text, "\r\n", "\n"), "\n") {
		if line != "" && line[0] != ' ' && line[0] != '#' { // a top-level key
			inPaths = line == "paths:"
			path, op, inResponses = "", "", false
			continue
		}
		if !inPaths {
			continue
		}
		if m := pathLineRE.FindStringSubmatch(line); m != nil {
			path, op, inResponses = m[1], "", false
			continue
		}
		if m := methodLineRE.FindStringSubmatch(line); m != nil && path != "" {
			op = strings.ToUpper(m[1]) + " " + path
			doc.ops[op] = map[int]bool{}
			inResponses = false
			continue
		}
		if op == "" {
			continue
		}
		if strings.HasPrefix(line, "      ") && !strings.HasPrefix(line, "       ") { // 6-space key
			inResponses = strings.TrimSpace(line) == "responses:"
			continue
		}
		if m := codeLineRE.FindStringSubmatch(line); m != nil && inResponses {
			code, _ := strconv.Atoi(m[1])
			doc.ops[op][code] = true
		}
	}
	return doc
}

func (d *openAPIDoc) opKeys() []string {
	var keys []string
	for k := range d.ops {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Every registered route+method is documented, and nothing documented is missing from the router.
func TestOpenAPIDocumentsExactlyTheRouterRoutes(t *testing.T) {
	doc := loadOpenAPI(t)
	if !strings.Contains(doc.text, "\nopenapi: 3.0.3\n") && !strings.HasPrefix(doc.text, "openapi: 3.0.3\n") {
		t.Fatal(`document must declare "openapi: 3.0.3"`)
	}
	if len(doc.ops) == 0 {
		t.Fatal("no operations found under paths: - did the document layout change?")
	}

	routes := map[string]bool{}
	for _, r := range Routes() {
		routes[r.Method+" "+r.Path] = true
	}
	for key := range routes {
		if _, ok := doc.ops[key]; !ok {
			t.Errorf("route %q is registered but not documented in api/openapi.yaml", key)
		}
	}
	for _, key := range doc.opKeys() {
		if !routes[key] {
			t.Errorf("operation %q is documented in api/openapi.yaml but not served by the router", key)
		}
	}
}

// The documented status codes of each operation are exactly the ones it can return.
func TestOpenAPIStatusCodes(t *testing.T) {
	doc := loadOpenAPI(t)
	changes := []int{200, 400, 401, 403, 405, 500}
	want := map[string][]int{
		"PUT /v1/keys/{key}":         {200, 201, 400, 401, 403, 405, 409, 412, 422, 500},
		"GET /v1/keys/{key}":         {200, 401, 403, 404, 405, 500},
		"DELETE /v1/keys/{key}":      {204, 400, 401, 403, 404, 405, 409, 422, 500},
		"GET /v1/keys":               {200, 401, 403, 405, 500},
		"GET /v1/changes":            changes,
		"GET /v1/changes/stream":     changes,
		"GET /metrics":               {200, 401, 405},
		"GET /v1/admin/changes":      changes,
		"GET /v1/replication/status": {200, 401, 403, 405, 500},
		"GET /v1/admin/digest":       {200, 400, 401, 403, 404, 405, 500},

		// STORE-210 (spec docs/capstone/spec.md, contract added in B3).
		"GET /v1/usage":                         {200, 401, 403, 405, 500},
		"GET /v1/admin/tenants":                 {200, 401, 403, 405, 500},
		"GET /v1/admin/tenants/{id}":            {200, 400, 401, 403, 404, 405, 500},
		"GET /v1/admin/tenants/{id}/quota":      {200, 400, 401, 403, 404, 405, 500},
		"PUT /v1/admin/tenants/{id}/quota":      {200, 400, 401, 403, 404, 405, 409, 412, 428, 500},
		"GET /v1/flags":                         {200, 401, 405, 500},
		"PUT /v1/admin/flags/quota_enforcement": {200, 400, 401, 403, 405, 409, 412, 428, 500},
	}
	for op, codes := range want {
		got, ok := doc.ops[op]
		if !ok {
			t.Errorf("%s: not documented", op)
			continue
		}
		wantSet := map[int]bool{}
		for _, c := range codes {
			wantSet[c] = true
		}
		for c := range wantSet {
			if !got[c] {
				t.Errorf("%s: status %d is not documented", op, c)
			}
		}
		for c := range got {
			if !wantSet[c] {
				t.Errorf("%s: documents status %d which the route cannot return", op, c)
			}
		}
	}
}

func TestOpenAPIDescribesSecurityHeadersAndErrors(t *testing.T) {
	doc := loadOpenAPI(t).text
	for _, s := range []string{
		"securitySchemes:", "bearerAuth:", "type: http", "scheme: bearer",
		"security:\n  - bearerAuth: []",
		"name: If-Match", "name: If-None-Match", "name: Idempotency-Key", "name: Last-Event-ID",
		"ETag:", "Idempotent-Replayed:", "Allow:",
		"text/event-stream", "text/plain:", "application/json:",
	} {
		if !strings.Contains(doc, s) {
			t.Errorf("document does not mention %q", s)
		}
	}

	// The Error schema must list exactly the codes the server uses.
	errSchema := schemaBlock(t, doc, "Error")
	if !strings.Contains(errSchema, "code:") || !strings.Contains(errSchema, "message:") {
		t.Error("Error schema must have error.code and error.message")
	}
	wantCodes := []string{
		codeUnauthorized, codeForbidden, codeNotFound, codeInvalidRequest,
		codePreconditionFailed, codeMethodNotAllowed, codeIdempotencyConflict, codeReadOnlyReplica, codeInternal,
		codeQuotaExceeded, codePreconditionRequired,
	}
	listed := map[string]bool{}
	for _, line := range strings.Split(errSchema, "\n") {
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "- ") {
			listed[strings.TrimPrefix(t, "- ")] = true
		}
	}
	for _, c := range wantCodes {
		if !listed[c] {
			t.Errorf("Error schema does not list code %q", c)
		}
		delete(listed, c)
	}
	for c := range listed {
		t.Errorf("Error schema lists code %q which the server does not define", c)
	}
}

// schemaBlock returns the text of components.schemas.<name> (4-space key) up to the next schema.
func schemaBlock(t *testing.T, doc, name string) string {
	t.Helper()
	lines := strings.Split(strings.ReplaceAll(doc, "\r\n", "\n"), "\n")
	var out []string
	in, inSchemas := false, false
	for _, line := range lines {
		if line != "" && line[0] != ' ' {
			inSchemas = false
		}
		if line == "  schemas:" {
			inSchemas = true
			continue
		}
		if !inSchemas {
			continue
		}
		if strings.HasPrefix(line, "    ") && !strings.HasPrefix(line, "     ") { // 4-space key
			in = strings.TrimSpace(line) == name+":"
			continue
		}
		if in {
			out = append(out, line)
		}
	}
	if len(out) == 0 {
		t.Fatalf("schema %q not found in the document", name)
	}
	return strings.Join(out, "\n")
}

// Real responses must be documented: a battery of real HTTP requests across
// every route, each status checked against the document.
func TestOpenAPIMatchesRealResponses(t *testing.T) {
	doc := loadOpenAPI(t)
	e := setupFull(t, Options{SSEHeartbeat: 100 * time.Millisecond}, nil)

	documented := func(method, template string, status int) {
		t.Helper()
		codes, ok := doc.ops[method+" "+template]
		if !ok {
			// A method the path does not support: only 405 is allowed, and
			// every operation on the path documents it.
			for key, c := range doc.ops {
				if strings.HasSuffix(key, " "+template) && !c[405] {
					t.Errorf("%s documents no 405", key)
				}
			}
			if status != 405 {
				t.Errorf("%s %s: unsupported method returned %d, want 405", method, template, status)
			}
			return
		}
		if !codes[status] {
			t.Errorf("%s %s returned %d, which the document does not list (%v)", method, template, status, keysOf(codes))
		}
	}
	do := func(method, template, path, token, body string, hdr map[string]string) resp {
		t.Helper()
		r := e.do(method, path, token, body, hdr)
		documented(method, template, r.status)
		return r
	}
	const key = "/v1/keys/{key}"

	// keys
	do("PUT", key, "/v1/keys/k", e.tokenA, `{"value":1}`, nil)                                // 201
	do("PUT", key, "/v1/keys/k", e.tokenA, `{"value":2}`, nil)                                // 200
	do("PUT", key, "/v1/keys/k", e.tokenA, `{"value":3}`, map[string]string{"If-Match": "9"}) // 412
	do("PUT", key, "/v1/keys/k", e.tokenA, `{bad`, nil)                                       // 400
	do("PUT", key, "/v1/keys/k", "", `{"value":1}`, nil)                                      // 401
	do("PUT", key, "/v1/keys/k", adminToken, `{"value":1}`, nil)                              // 403
	do("PUT", key, "/v1/keys/i", e.tokenA, `{"value":1}`, idem("x"))                          // 201
	do("PUT", key, "/v1/keys/i", e.tokenA, `{"value":2}`, idem("x"))                          // 422
	do("GET", key, "/v1/keys/k", e.tokenA, "", nil)                                           // 200
	do("GET", key, "/v1/keys/missing", e.tokenA, "", nil)                                     // 404
	do("GET", key, "/v1/keys/k", "bad", "", nil)                                              // 401
	do("GET", key, "/v1/keys/k", adminToken, "", nil)                                         // 403
	do("DELETE", key, "/v1/keys/k", e.tokenA, "", nil)                                        // 204
	do("DELETE", key, "/v1/keys/k", e.tokenA, "", nil)                                        // 404
	do("DELETE", key, "/v1/keys/k", e.tokenA, "", idem(strings.Repeat("x", 300)))             // 400
	do("DELETE", key, "/v1/keys/i", e.tokenA, "", idem("x"))                                  // 422 (key used by a PUT)
	do("POST", key, "/v1/keys/k", e.tokenA, `{"value":1}`, nil)                               // 405
	do("PATCH", key, "/v1/keys/k", e.tokenA, `{"value":1}`, nil)                              // 405
	do("GET", "/v1/keys", "/v1/keys", e.tokenA, "", nil)                                      // 200
	do("GET", "/v1/keys", "/v1/keys", "", "", nil)                                            // 401
	do("GET", "/v1/keys", "/v1/keys", adminToken, "", nil)                                    // 403
	do("POST", "/v1/keys", "/v1/keys", e.tokenA, `{}`, nil)                                   // 405

	// changes (data exists, so the long-poll returns immediately)
	do("GET", "/v1/changes", "/v1/changes?since=0", e.tokenA, "", nil)                 // 200
	do("GET", "/v1/changes", "/v1/changes?since=abc", e.tokenA, "", nil)               // 400
	do("GET", "/v1/changes", "/v1/changes", "", "", nil)                               // 401
	do("GET", "/v1/changes", "/v1/changes", adminToken, "", nil)                       // 403
	do("PUT", "/v1/changes", "/v1/changes", e.tokenA, `{}`, nil)                       // 405
	do("GET", "/v1/changes/stream", "/v1/changes/stream?since=abc", e.tokenA, "", nil) // 400
	do("GET", "/v1/changes/stream", "/v1/changes/stream", "", "", nil)                 // 401
	do("GET", "/v1/changes/stream", "/v1/changes/stream", adminToken, "", nil)         // 403
	do("POST", "/v1/changes/stream", "/v1/changes/stream", e.tokenA, `{}`, nil)        // 405
	c := e.stream(e.tokenA, "", nil)                                                   // 200 text/event-stream (checked by the helper)
	documented("GET", "/v1/changes/stream", 200)
	c.close()

	// metrics
	do("GET", "/metrics", "/metrics", adminToken, "", nil)  // 200
	do("GET", "/metrics", "/metrics", "", "", nil)          // 401
	do("GET", "/metrics", "/metrics", e.tokenA, "", nil)    // 401 (tenant token)
	do("POST", "/metrics", "/metrics", adminToken, "", nil) // 405

	// replication
	do("GET", "/v1/admin/changes", "/v1/admin/changes?since=0", adminToken, "", nil)      // 200
	do("GET", "/v1/admin/changes", "/v1/admin/changes?since=abc", adminToken, "", nil)    // 400
	do("GET", "/v1/admin/changes", "/v1/admin/changes", "", "", nil)                      // 401
	do("GET", "/v1/admin/changes", "/v1/admin/changes", e.tokenA, "", nil)                // 403
	do("POST", "/v1/admin/changes", "/v1/admin/changes", adminToken, `{}`, nil)           // 405
	do("GET", "/v1/replication/status", "/v1/replication/status", adminToken, "", nil)    // 200
	do("GET", "/v1/replication/status", "/v1/replication/status", "", "", nil)            // 401
	do("GET", "/v1/replication/status", "/v1/replication/status", e.tokenA, "", nil)      // 403
	do("DELETE", "/v1/replication/status", "/v1/replication/status", adminToken, "", nil) // 405
	do("GET", "/v1/admin/digest", "/v1/admin/digest", adminToken, "", nil)                // 200
	do("GET", "/v1/admin/digest", "/v1/admin/digest?tenant=1", adminToken, "", nil)       // 200
	do("GET", "/v1/admin/digest", "/v1/admin/digest?tenant=abc", adminToken, "", nil)     // 400
	do("GET", "/v1/admin/digest", "/v1/admin/digest?tenant=9999", adminToken, "", nil)    // 404
	do("GET", "/v1/admin/digest", "/v1/admin/digest", "", "", nil)                        // 401
	do("GET", "/v1/admin/digest", "/v1/admin/digest", e.tokenA, "", nil)                  // 403
	do("POST", "/v1/admin/digest", "/v1/admin/digest", adminToken, `{}`, nil)             // 405

	if t.Failed() {
		t.Logf("documented operations: %s", fmt.Sprint(doc.opKeys()))
	}
}

func keysOf(m map[int]bool) []int {
	var out []int
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}
