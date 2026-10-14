package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// CanonicalJSON returns the canonical form of a JSON value (STORE-210, spec
// section 2): no insignificant whitespace, object members in their original
// order, numbers exactly as written, and strings re-encoded minimally:
// escapes such as backslash-u003c become the character itself and nothing is
// HTML-escaped. Two spellings of the same value therefore have the same
// canonical bytes, which is what is stored, replicated, counted and hashed.
//
// Input that is not valid JSON is returned unchanged (the HTTP API only ever
// stores valid JSON; the store itself accepts any bytes). As in Go's own
// decoder, invalid UTF-8 and lone surrogate escapes become U+FFFD.
func CanonicalJSON(v []byte) []byte {
	out, err := canonical(v)
	if err != nil {
		return v
	}
	return out
}

func canonical(v []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(v))
	dec.UseNumber()
	var out bytes.Buffer
	out.Grow(len(v))
	if err := writeCanonical(dec, &out); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("canonical json: trailing data")
	}
	return out.Bytes(), nil
}

func writeCanonical(dec *json.Decoder, out *bytes.Buffer) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch t := tok.(type) {
	case json.Delim:
		closer, open := json.Delim('}'), byte('{')
		if t == '[' {
			closer, open = ']', '['
		} else if t != '{' {
			return fmt.Errorf("canonical json: unexpected %q", t)
		}
		out.WriteByte(open)
		for first := true; dec.More(); first = false {
			if !first {
				out.WriteByte(',')
			}
			if t == '{' {
				k, err := dec.Token()
				if err != nil {
					return err
				}
				name, ok := k.(string)
				if !ok {
					return errors.New("canonical json: object key is not a string")
				}
				writeString(out, name)
				out.WriteByte(':')
			}
			if err := writeCanonical(dec, out); err != nil {
				return err
			}
		}
		end, err := dec.Token()
		if err != nil {
			return err
		}
		if end != closer {
			return fmt.Errorf("canonical json: unexpected %v", end)
		}
		out.WriteByte(byte(closer))
	case string:
		writeString(out, t)
	case json.Number:
		out.WriteString(string(t))
	case bool:
		if t {
			out.WriteString("true")
		} else {
			out.WriteString("false")
		}
	case nil:
		out.WriteString("null")
	default:
		return fmt.Errorf("canonical json: unexpected token %T", tok)
	}
	return nil
}

// writeString writes s as a JSON string with the minimal escapes (quote,
// backslash, control characters, U+2028/U+2029) and no HTML escaping.
func writeString(out *bytes.Buffer, s string) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(s) // a string always encodes
	out.Write(bytes.TrimSuffix(b.Bytes(), []byte("\n")))
}
