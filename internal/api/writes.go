package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"storecraft/internal/store"
)

const maxIdempotencyKeyLen = 255

// rejectIfReplica answers writes on a replica with 409 read_only_replica. It
// runs after authentication, so unauthenticated callers still get 401.
func (s *server) rejectIfReplica(w http.ResponseWriter) bool {
	if !s.readOnly {
		return false
	}
	writeError(w, http.StatusConflict, codeReadOnlyReplica, "this node is a read-only replica; send writes to the primary")
	return true
}

func (s *server) delete(w http.ResponseWriter, r *http.Request, t store.Tenant, key string) {
	if s.rejectIfReplica(w) {
		return
	}
	idem, ok := idempotency(w, r, key, nil)
	if !ok {
		return
	}
	s.apply(w, r, t, idem, store.Op{Kind: store.KindDelete, Key: key}, func(int64) store.Response {
		return store.Response{Status: http.StatusNoContent}
	})
}

func (s *server) put(w http.ResponseWriter, r *http.Request, t store.Tenant, key string) {
	if s.rejectIfReplica(w) {
		return
	}
	ifMatch := strings.TrimSpace(r.Header.Get("If-Match"))
	ifNoneMatch := strings.TrimSpace(r.Header.Get("If-None-Match"))
	if ifMatch != "" && ifNoneMatch != "" {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "If-Match and If-None-Match cannot be combined")
		return
	}
	op := store.Op{Kind: store.KindPut, Key: key}
	switch {
	case ifMatch != "":
		v, err := strconv.ParseInt(strings.Trim(ifMatch, `"`), 10, 64)
		if err != nil || v < 1 {
			writeError(w, http.StatusBadRequest, codeInvalidRequest, "If-Match must be a version number")
			return
		}
		op.Kind, op.Expected = store.KindPutIfVersion, v
	case ifNoneMatch == "*":
		op.Kind = store.KindPutIfAbsent
	case ifNoneMatch != "":
		writeError(w, http.StatusBadRequest, codeInvalidRequest, `If-None-Match must be "*"`)
		return
	}

	body, ok := readBody(w, r)
	if !ok {
		return
	}
	value, ok := parseValue(w, body)
	if !ok {
		return
	}
	op.Value = value
	idem, ok := idempotency(w, r, key, body)
	if !ok {
		return
	}
	s.apply(w, r, t, idem, op, func(version int64) store.Response {
		status := http.StatusOK
		if version == 1 {
			status = http.StatusCreated
		}
		b, _ := json.Marshal(map[string]any{"key": key, "version": version})
		return store.Response{Status: status, Body: append(b, '\n')}
	})
}

// apply runs one write through the store and writes the (fresh or replayed)
// response, mapping store errors to the error envelope.
func (s *server) apply(w http.ResponseWriter, r *http.Request, t store.Tenant, idem *store.Idem, op store.Op, render func(int64) store.Response) {
	resp, replayed, err := s.st.Apply(r.Context(), t.ID, idem, op, render)
	var quota *store.QuotaError
	switch {
	case errors.As(err, &quota):
		s.m.QuotaRejections.WithLabelValues(quota.Dimension).Inc()
		writeErrorDetails(w, http.StatusForbidden, codeQuotaExceeded, quota.Error(), map[string]any{
			"dimension": quota.Dimension, "used": quota.Used, "limit": quota.Limit, "requested": quota.Requested,
		})
		return
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusPreconditionFailed, codePreconditionFailed, "version precondition failed")
		return
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, codeNotFound, "key not found")
		return
	case errors.Is(err, store.ErrIdempotencyConflict):
		writeError(w, http.StatusUnprocessableEntity, codeIdempotencyConflict,
			"Idempotency-Key was already used with a different method, path or body")
		return
	case err != nil:
		s.internal(w, r, err)
		return
	}
	if replayed {
		s.m.IdempotentReplays.Inc()
		w.Header().Set("Idempotent-Replayed", "true")
	} else if op.Kind == store.KindDelete {
		s.m.ChangesWritten.WithLabelValues(store.OpDelete).Inc()
	} else {
		s.m.ChangesWritten.WithLabelValues(store.OpPut).Inc()
	}
	writeStored(w, resp)
}

// writeStored sends a store.Response. PUT responses carry their version, from
// which the ETag is (re)built, so replays are identical to the original.
func writeStored(w http.ResponseWriter, resp store.Response) {
	if len(resp.Body) > 0 {
		w.Header().Set("Content-Type", "application/json")
		if resp.Status == http.StatusOK || resp.Status == http.StatusCreated {
			var v struct {
				Version int64 `json:"version"`
			}
			if json.Unmarshal(resp.Body, &v) == nil && v.Version > 0 {
				w.Header().Set("ETag", etag(v.Version))
			}
		}
	}
	w.WriteHeader(resp.Status)
	w.Write(resp.Body)
}

// idempotency builds the store.Idem for the request's Idempotency-Key header.
// It returns (nil, true) when the header is absent and (nil, false) after
// writing a 400 when it is invalid.
func idempotency(w http.ResponseWriter, r *http.Request, key string, body []byte) (*store.Idem, bool) {
	k := r.Header.Get("Idempotency-Key")
	if k == "" {
		return nil, true
	}
	if len(k) > maxIdempotencyKeyLen {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "Idempotency-Key must be at most 255 characters")
		return nil, false
	}
	for i := 0; i < len(k); i++ {
		if k[i] < 0x21 || k[i] > 0x7e {
			writeError(w, http.StatusBadRequest, codeInvalidRequest, "Idempotency-Key must be printable ASCII without spaces")
			return nil, false
		}
	}
	h := sha256.New()
	h.Write([]byte(r.Method))
	h.Write([]byte{0})
	h.Write([]byte("/v1/keys/" + key))
	h.Write([]byte{0})
	h.Write(body)
	return &store.Idem{Key: k, RequestHash: hex.EncodeToString(h.Sum(nil))}, true
}

// readBody reads the request body, enforcing the size limit. On failure it
// writes a 400 and returns ok=false.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusBadRequest, codeInvalidRequest, "request body exceeds 1 MiB")
		} else {
			writeError(w, http.StatusBadRequest, codeInvalidRequest, "could not read request body")
		}
		return nil, false
	}
	return body, true
}

// parseValue extracts {"value": <any JSON>} from body. On failure it writes a
// 400 and returns ok=false.
func parseValue(w http.ResponseWriter, body []byte) (json.RawMessage, bool) {
	bad := func(msg string) (json.RawMessage, bool) {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, msg)
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	var req struct {
		Value json.RawMessage `json:"value"`
	}
	if err := dec.Decode(&req); err != nil {
		return bad("body must be valid JSON")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return bad("body must be a single JSON object")
	}
	if req.Value == nil {
		return bad(`missing "value" field`)
	}
	return req.Value, true
}
