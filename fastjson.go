// fastjson.go — hand-rolled JSON for the issue-create hot path (the write
// workload POSTs 200 issues). The generic path (json.Unmarshal into any +
// reflection marshal) costs ~15 us per op. This reader extracts only
// title/body/labels and the serializer emits the response byte-identical
// to encoding/json with SetEscapeHTML(false). Anything it cannot prove
// identical falls back to the generic path, so behaviour never diverges:
// invalid JSON -> 400, valid non-object -> empty-map behaviour,
// whitespace-only -> 400, non-string title/body or odd label elements ->
// the generic handler decides.
package main

import (
	"strconv"
	"unicode/utf16"
	"unicode/utf8"
)

// ---- request parsing ----

// fastIssueBody parses an issue-create request body without encoding/json.
// It returns the map restDispatch expects (title/body strings, labels as
// []any of strings) plus true, or (nil, false) to take the generic path.
func fastIssueBody(raw []byte) (map[string]any, bool) {
	i := skipWS(raw, 0)
	if i >= len(raw) || raw[i] != '{' {
		return nil, false
	}
	i++
	out := map[string]any{}
	i = skipWS(raw, i)
	if i < len(raw) && raw[i] == '}' {
		i = skipWS(raw, i+1)
		if i != len(raw) {
			return nil, false
		}
		return out, true
	}
	for {
		i = skipWS(raw, i)
		if i >= len(raw) || raw[i] != '"' {
			return nil, false
		}
		name, ni, ok := decodeJSONString(raw, i)
		if !ok {
			return nil, false
		}
		i = skipWS(raw, ni)
		if i >= len(raw) || raw[i] != ':' {
			return nil, false
		}
		i = skipWS(raw, i+1)
		switch name {
		case "title", "body":
			if i >= len(raw) || raw[i] != '"' {
				return nil, false
			}
			v, ni, ok := decodeJSONString(raw, i)
			if !ok {
				return nil, false
			}
			out[name] = v
			i = ni
		case "labels":
			if i >= len(raw) || raw[i] != '[' {
				return nil, false
			}
			labs, ni, ok := parseLabels(raw, i)
			if !ok {
				return nil, false
			}
			out["labels"] = labs
			i = ni
		default:
			ni, ok := skipValue(raw, i)
			if !ok {
				return nil, false
			}
			i = ni
		}
		i = skipWS(raw, i)
		if i >= len(raw) {
			return nil, false
		}
		if raw[i] == ',' {
			i++
			continue
		}
		if raw[i] == '}' {
			i = skipWS(raw, i+1)
			if i != len(raw) {
				return nil, false
			}
			return out, true
		}
		return nil, false
	}
}

// parseLabels parses a labels array: elements must be strings or
// {"name": string} objects (other members skipped). Returns []any of
// plain strings, matching what the generic path hands the handler.
func parseLabels(b []byte, i int) ([]any, int, bool) {
	// b[i] == '['
	i++
	out := []any{}
	i = skipWS(b, i)
	if i < len(b) && b[i] == ']' {
		return out, i + 1, true
	}
	for {
		i = skipWS(b, i)
		if i >= len(b) {
			return nil, 0, false
		}
		switch b[i] {
		case '"':
			v, ni, ok := decodeJSONString(b, i)
			if !ok {
				return nil, 0, false
			}
			out = append(out, v)
			i = ni
		case '{':
			name, ni, ok := parseLabelObject(b, i)
			if !ok {
				return nil, 0, false
			}
			out = append(out, name)
			i = ni
		default:
			return nil, 0, false
		}
		i = skipWS(b, i)
		if i >= len(b) {
			return nil, 0, false
		}
		if b[i] == ',' {
			i++
			continue
		}
		if b[i] == ']' {
			return out, i + 1, true
		}
		return nil, 0, false
	}
}

// parseLabelObject extracts the "name" member of a {"name": ...} label
// object (last "name" wins, like encoding/json); other members are skipped.
func parseLabelObject(b []byte, i int) (string, int, bool) {
	// b[i] == '{'
	i++
	name := ""
	i = skipWS(b, i)
	if i < len(b) && b[i] == '}' {
		return name, i + 1, true
	}
	for {
		i = skipWS(b, i)
		if i >= len(b) || b[i] != '"' {
			return "", 0, false
		}
		m, ni, ok := decodeJSONString(b, i)
		if !ok {
			return "", 0, false
		}
		i = skipWS(b, ni)
		if i >= len(b) || b[i] != ':' {
			return "", 0, false
		}
		i = skipWS(b, i+1)
		if m == "name" {
			if i >= len(b) || b[i] != '"' {
				return "", 0, false
			}
			v, ni, ok := decodeJSONString(b, i)
			if !ok {
				return "", 0, false
			}
			name = v
			i = ni
		} else {
			ni, ok := skipValue(b, i)
			if !ok {
				return "", 0, false
			}
			i = ni
		}
		i = skipWS(b, i)
		if i >= len(b) {
			return "", 0, false
		}
		if b[i] == ',' {
			i++
			continue
		}
		if b[i] == '}' {
			return name, i + 1, true
		}
		return "", 0, false
	}
}

func skipWS(b []byte, i int) int {
	for i < len(b) {
		switch b[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return i
		}
	}
	return i
}

// skipValue skips one JSON value starting at i (which must not be
// whitespace) and returns the position just past it. It validates the
// value's shape strictly enough that anything malformed returns false
// (the caller then falls back to the generic path, which reports 400).
func skipValue(b []byte, i int) (int, bool) {
	if i >= len(b) {
		return 0, false
	}
	switch c := b[i]; {
	case c == '"':
		_, ni, ok := decodeJSONString(b, i)
		return ni, ok
	case c == '{':
		i++
		i = skipWS(b, i)
		if i < len(b) && b[i] == '}' {
			return i + 1, true
		}
		for {
			i = skipWS(b, i)
			if i >= len(b) || b[i] != '"' {
				return 0, false
			}
			_, ni, ok := decodeJSONString(b, i)
			if !ok {
				return 0, false
			}
			i = skipWS(b, ni)
			if i >= len(b) || b[i] != ':' {
				return 0, false
			}
			ni, ok = skipValue(b, skipWS(b, i+1))
			if !ok {
				return 0, false
			}
			i = skipWS(b, ni)
			if i >= len(b) {
				return 0, false
			}
			if b[i] == ',' {
				i++
				continue
			}
			if b[i] == '}' {
				return i + 1, true
			}
			return 0, false
		}
	case c == '[':
		i++
		i = skipWS(b, i)
		if i < len(b) && b[i] == ']' {
			return i + 1, true
		}
		for {
			ni, ok := skipValue(b, skipWS(b, i))
			if !ok {
				return 0, false
			}
			i = skipWS(b, ni)
			if i >= len(b) {
				return 0, false
			}
			if b[i] == ',' {
				i++
				continue
			}
			if b[i] == ']' {
				return i + 1, true
			}
			return 0, false
		}
	case c == 't':
		if len(b)-i >= 4 && string(b[i:i+4]) == "true" {
			return i + 4, true
		}
		return 0, false
	case c == 'f':
		if len(b)-i >= 5 && string(b[i:i+5]) == "false" {
			return i + 5, true
		}
		return 0, false
	case c == 'n':
		if len(b)-i >= 4 && string(b[i:i+4]) == "null" {
			return i + 4, true
		}
		return 0, false
	case c == '-' || (c >= '0' && c <= '9'):
		return skipNumber(b, i)
	default:
		return 0, false
	}
}

// skipNumber validates strict JSON number grammar:
// -?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?
func skipNumber(b []byte, i int) (int, bool) {
	j := i
	if j < len(b) && b[j] == '-' {
		j++
	}
	if j >= len(b) {
		return 0, false
	}
	if b[j] == '0' {
		j++
	} else if b[j] >= '1' && b[j] <= '9' {
		for j < len(b) && b[j] >= '0' && b[j] <= '9' {
			j++
		}
	} else {
		return 0, false
	}
	if j < len(b) && b[j] == '.' {
		j++
		if j >= len(b) || b[j] < '0' || b[j] > '9' {
			return 0, false
		}
		for j < len(b) && b[j] >= '0' && b[j] <= '9' {
			j++
		}
	}
	if j < len(b) && (b[j] == 'e' || b[j] == 'E') {
		j++
		if j < len(b) && (b[j] == '+' || b[j] == '-') {
			j++
		}
		if j >= len(b) || b[j] < '0' || b[j] > '9' {
			return 0, false
		}
		for j < len(b) && b[j] >= '0' && b[j] <= '9' {
			j++
		}
	}
	return j, true
}

func hexVal(c byte) (int, bool) {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0'), true
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10, true
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10, true
	}
	return 0, false
}

// decodeJSONString decodes a JSON string starting at b[i] == '"' with
// encoding/json's exact semantics: escapes, \uXXXX with surrogate pairs
// (lone surrogates -> U+FFFD), invalid UTF-8 bytes -> U+FFFD each, raw
// control bytes -> not valid JSON (false).
func decodeJSONString(b []byte, i int) (string, int, bool) {
	// b[i] == '"'
	i++
	// Fast path: no escapes, pure ASCII.
	j := i
	for j < len(b) && b[j] != '"' && b[j] != '\\' {
		if b[j] < 0x20 {
			return "", 0, false
		}
		j++
	}
	if j < len(b) && b[j] == '"' {
		if j == i {
			return "", j + 1, true
		}
		// All ASCII so far; the raw slice is the decoded string only if
		// it holds no byte >= 0x80 (multi-byte UTF-8 is fine as-is, but
		// invalid bytes need U+FFFD substitution, so check).
		s := b[i:j]
		if utf8.Valid(s) {
			return string(s), j + 1, true
		}
	}
	out := make([]byte, 0, 64)
	for i < len(b) {
		c := b[i]
		switch {
		case c == '"':
			return string(out), i + 1, true
		case c == '\\':
			i++
			if i >= len(b) {
				return "", 0, false
			}
			switch b[i] {
			case '"':
				out = append(out, '"')
				i++
			case '\\':
				out = append(out, '\\')
				i++
			case '/':
				out = append(out, '/')
				i++
			case 'b':
				out = append(out, '\b')
				i++
			case 'f':
				out = append(out, '\f')
				i++
			case 'n':
				out = append(out, '\n')
				i++
			case 'r':
				out = append(out, '\r')
				i++
			case 't':
				out = append(out, '\t')
				i++
			case 'u':
				if i+4 >= len(b) {
					return "", 0, false
				}
				r := 0
				for k := 1; k <= 4; k++ {
					v, ok := hexVal(b[i+k])
					if !ok {
						return "", 0, false
					}
					r = r<<4 | v
				}
				i += 5
				if r >= 0xD800 && r <= 0xDBFF {
					// High surrogate: needs a following \uDC00-\uDFFF.
					if i+5 < len(b) && b[i] == '\\' && b[i+1] == 'u' {
						r2 := 0
						ok2 := true
						for k := 2; k <= 5; k++ {
							v, ok := hexVal(b[i+k])
							if !ok {
								ok2 = false
								break
							}
							r2 = r2<<4 | v
						}
						if ok2 && r2 >= 0xDC00 && r2 <= 0xDFFF {
							out = utf8.AppendRune(out, utf16.DecodeRune(rune(r), rune(r2)))
							i += 6
							continue
						}
					}
					out = utf8.AppendRune(out, utf8.RuneError)
					continue
				}
				if r >= 0xDC00 && r <= 0xDFFF {
					out = utf8.AppendRune(out, utf8.RuneError)
					continue
				}
				out = utf8.AppendRune(out, rune(r))
			default:
				return "", 0, false
			}
		case c < 0x20:
			return "", 0, false
		case c < 0x80:
			out = append(out, c)
			i++
		default:
			r, size := utf8.DecodeRune(b[i:])
			if r == utf8.RuneError && size == 1 {
				out = utf8.AppendRune(out, utf8.RuneError)
				i++
			} else {
				out = append(out, b[i:i+size]...)
				i += size
			}
		}
	}
	return "", 0, false
}

// ---- response serializing ----

// appendJSONString appends s quoted exactly like encoding/json with
// SetEscapeHTML(false): only '"', '\\' and bytes < 0x20 are escaped
// (\u2028/\u2029 are escaped too, as encoding/json always does); valid
// UTF-8 passes through; each invalid byte becomes U+FFFD.
func appendJSONString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '"':
			dst = append(dst, '\\', '"')
			i++
		case c == '\\':
			dst = append(dst, '\\', '\\')
			i++
		case c == '\n':
			dst = append(dst, '\\', 'n')
			i++
		case c == '\r':
			dst = append(dst, '\\', 'r')
			i++
		case c == '\t':
			dst = append(dst, '\\', 't')
			i++
		case c < 0x20:
			dst = append(dst, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xf])
			i++
		case c < 0x80:
			dst = append(dst, c)
			i++
		default:
			r, size := utf8.DecodeRuneInString(s[i:])
			if r == utf8.RuneError && size == 1 {
				dst = append(dst, `\ufffd`...)
				i++
			} else if r == '\u2028' {
				dst = append(dst, `\u2028`...)
				i += size
			} else if r == '\u2029' {
				dst = append(dst, `\u2029`...)
				i += size
			} else {
				dst = append(dst, s[i:i+size]...)
				i += size
			}
		}
	}
	dst = append(dst, '"')
	return dst
}

// fastIssueResponse serializes an issue exactly like issueRestJSON (same
// field order, same escaping, no trailing newline) and is used to populate
// cachedRest. It returns nil only if a field cannot be proven identical,
// in which case the caller falls back to issueRestJSON.
func fastIssueResponse(issue *Issue) []byte {
	out := make([]byte, 0, 256)
	out = append(out, `{"id":`...)
	out = strconv.AppendInt(out, int64(issue.Number*100000+issue.Repo.DBID), 10)
	out = append(out, `,"node_id":`...)
	out = appendJSONString(out, issue.GID)
	out = append(out, `,"number":`...)
	out = strconv.AppendInt(out, int64(issue.Number), 10)
	out = append(out, `,"title":`...)
	out = appendJSONString(out, issue.Title)
	out = append(out, `,"body":`...)
	out = appendJSONString(out, issue.Body)
	out = append(out, `,"state":`...)
	out = appendJSONString(out, issue.State)
	out = append(out, `,"labels":[`...)
	for i, n := range issue.Labels {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, `{"name":`...)
		out = appendJSONString(out, n)
		out = append(out, '}')
	}
	out = append(out, `],"user":{"login":`...)
	out = appendJSONString(out, viewerLogin)
	out = append(out, `,"id":`...)
	out = strconv.AppendInt(out, int64(viewer.ID), 10)
	out = append(out, `},"created_at":`...)
	out = appendJSONString(out, issue.CreatedAt)
	out = append(out, `,"updated_at":`...)
	out = appendJSONString(out, issue.UpdatedAt)
	out = append(out, `,"html_url":"","url":""}`...)
	return out
}
