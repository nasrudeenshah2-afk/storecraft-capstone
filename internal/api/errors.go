package api

import (
	"bytes"
	"encoding/json"
	"net/http"
)

// Error codes used in the error envelope.
const (
	codeUnauthorized        = "unauthorized"
	codeForbidden           = "forbidden"
	codeNotFound            = "not_found"
	codeInvalidRequest      = "invalid_request"
	codePreconditionFailed  = "precondition_failed"
	codeMethodNotAllowed    = "method_not_allowed"
	codeInternal            = "internal"
	codeIdempotencyConflict = "idempotency_conflict"
	codeReadOnlyReplica     = "read_only_replica"

	// STORE-210.
	codeQuotaExceeded        = "quota_exceeded"
	codePreconditionRequired = "precondition_required"
)

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"` // quota_exceeded only
}

// marshal encodes v as compact JSON WITHOUT Go's default HTML escaping, so a
// value containing <, > or & leaves the server exactly as it is stored (the
// canonical form, STORE-210; this is also what fixed intake finding F1).
func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := marshal(v)
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "internal error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(append(b, '\n'))
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, errorBody{Error: errorDetail{Code: code, Message: msg}})
}

// writeErrorDetails is writeError with a details object.
func writeErrorDetails(w http.ResponseWriter, status int, code, msg string, details any) {
	writeJSON(w, status, errorBody{Error: errorDetail{Code: code, Message: msg, Details: details}})
}
