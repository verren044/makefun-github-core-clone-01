// fastjson.go — hand-rolled JSON paths for the issue-create hot path.
//
// The write workload POSTs 200 issues; the generic path (json.Unmarshal
// into any + reflection marshal) costs ~15us per op. This file parses only
// what an issue create needs (title/body/labels) and serializes the
// response manually, byte-identical to encoding/json with
// SetEscapeHTML(false). Anything it cannot handle exactly falls back to
// the generic path — correctness first, speed second.
package main

import (
	"bytes"
	"encoding/json"
	"strconv"
	"unicode/utf8"
)

// appendJSONString appends s as a JSON string literal, exactly as
// encoding/json does with HTML escaping off: only '"', '\\' and bytes
// < 0x20 are escaped (\n \r \t short, the rest \u00XX); valid UTF-8
// passes through; each invalid byte becomes \ufffd.
func appendJSONString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c == '"' || c == '\\' {
			if start < i {
				dst = append(dst, s[start:i]...)
			}
			switch c {
			case '"':
				dst = append(dst, '\\', '"')
			case '\\':
				dst = append(dst, '\\', '\\')
			case '\n':
				dst = append(dst, '\\', 'n')
			case '\r':
				dst = append(dst, '\\', 'r')
			case '\t':
				dst = append(dst, '\\', 't')
			default:
				dst = append(dst, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xf])
			}
			start = i + 1
		} else if c >= 0x80 {
			// Validate the UTF-8 sequence starting here; encoding/json
			// replaces each invalid byte with \ufffd.
			_, size := utf8.DecodeRuneInString(s[i:])
			if size == 1 && c != utf8.RuneSelf {
				// DecodeRuneInString returns size 1 for an invalid byte.
				if start < i {
					dst = append(dst, s[start:i]...)
				}
				dst = append(dst, '\\', 'u', 'f', 'f', 'f', 'd')
				start = i + 1
			} else {
				i += size - 1
			}
		}
	}
	if start < len(s) {
		dst = append(dst, s[start:]...)
	}
	return append(dst, '"')
}

// jsonSkipWS returns the first index >= pos that is not JSON whitespace.
func jsonSkipWS(data []byte, pos int) int {
	for pos < len(data) {
		switch data[pos] {
		case ' ', '\t', '\n', '\r':
			pos++
		default:
			return pos
		}
	}
	return pos
}

// jsonSkipString returns the index just past the string token starting at
// data[pos] == '"', honouring backslash escapes. ok=false if unterminated.
func jsonSkipString(data []byte, pos int) (int, bool) {
	pos++ // opening quote
	for pos < len(data) {
		c := data[pos]
		if c == '\\' {
			pos += 2
			continue
		}
		if c == '"' {
			return pos + 1, true
		}
		pos++
	}
	return pos, false
}

// jsonSkipValue returns the index just past the JSON value starting at pos.
func jsonSkipValue(data []byte, pos int) (int, bool) {
	pos = jsonSkipWS(data, pos)
	if pos >= len(data) {
		return pos, false
	}
	switch data[pos] {
	case '"':
		return jsonSkipString(data, pos)
	case '{':
		pos++
		pos = jsonSkipWS(data, pos)
		if pos < len(data) && data[pos] == '}' {
			return pos + 1, true
		}
		for {
			pos = jsonSkipWS(data, pos)
			if pos >= len(data) || data[pos] != '"' {
				return pos, false
			}
			var ok bool
			pos, ok = jsonSkipString(data, pos)
			if !ok {
				return pos, false
			}
			pos = jsonSkipWS(data, pos)
			if pos >= len(data) || data[pos] != ':' {
				return pos, false
			}
			pos, ok = jsonSkipValue(data, pos+1)
			if !ok {
				return pos, false
			}
			pos = jsonSkipWS(data, pos)
			if pos < len(data) && data[pos] == ',' {
				pos++
				continue
			}
			if pos < len(data) && data[pos] == '}' {
				return pos + 1, true
			}
			return pos, false
		}
	case '[':
		pos++
		pos = jsonSkipWS(data, pos)
		if pos < len(data) && data[pos] == ']' {
			return pos + 1, true
		}
		for {
			var ok bool
			pos, ok = jsonSkipValue(data, pos)
			if !ok {
				return pos, false
			}
			pos = jsonSkipWS(data, pos)
			if pos < len(data) && data[pos] == ',' {
				pos++
				continue
			}
			if pos < len(data) && data[pos] == ']' {
				return pos + 1, true
			}
			return pos, false
		}
	default:
		// number, true, false, null: consume until a structural char.
		for pos < len(data) {
			switch data[pos] {
			case ',', '}', ']', ' ', '\t', '\n', '\r':
				return pos, true
			}
			pos++
		}
		return pos, true
	}
}

type objMember struct {
	key string
	val []byte // raw JSON of the value (string tokens include the quotes)
}

// scanObjectMembers parses a JSON object starting at data[pos] == '{'
// and returns its members in order. ok=false when the input is not a
// well-formed object.
func scanObjectMembers(data []byte, pos int) ([]objMember, bool) {
	pos = jsonSkipWS(data, pos)
	if pos >= len(data) || data[pos] != '{' {
		return nil, false
	}
	pos++
	pos = jsonSkipWS(data, pos)
	if pos < len(data) && data[pos] == '}' {
		return []objMember{}, true
	}
	var out []objMember
	for {
		pos = jsonSkipWS(data, pos)
		if pos >= len(data) || data[pos] != '"' {
			return nil, false
		}
		end, ok := jsonSkipString(data, pos)
		if !ok {
			return nil, false
		}
		var key string
		if err := json.Unmarshal(data[pos:end], &key); err != nil {
			return nil, false
		}
		pos = jsonSkipWS(data, end)
		if pos >= len(data) || data[pos] != ':' {
			return nil, false
		}
		vstart := jsonSkipWS(data, pos+1)
		vend, ok := jsonSkipValue(data, vstart)
		if !ok {
			return nil, false
		}
		out = append(out, objMember{key: key, val: data[vstart:vend]})
		pos = jsonSkipWS(data, vend)
		if pos < len(data) && data[pos] == ',' {
			pos++
			continue
		}
		if pos < len(data) && data[pos] == '}' {
			return out, true
		}
		return nil, false
	}
}

// memberString returns the string value of a member whose value is a JSON
// string; for any other JSON type it returns "" (matching strVal).
func memberString(m objMember) string {
	if len(m.val) == 0 || m.val[0] != '"' {
		return ""
	}
	var s string
	if err := json.Unmarshal(m.val, &s); err != nil {
		return ""
	}
	return s
}

// fastIssueCreate handles POST /repos/{owner}/{name}/issues without the
// generic json.Unmarshal-into-any path. It returns handled=false when the
// body needs the generic path (invalid JSON, or a shape the fast reader
// cannot prove identical). On handled=true, status is the HTTP status and
// body the response bytes (with trailing newline, as the generic path).
func fastIssueCreate(owner, name string, raw []byte) (handled bool, status int, body []byte) {
	// Empty body: the generic path creates an issue with empty fields
	// (parseJSONBody returns nil, bodyMap(nil) is an empty map), so the
	// fast path does the same. Whitespace-only is NOT empty: the generic
	// path rejects it as invalid JSON, so it must fall through below.
	var members []objMember
	if len(raw) == 0 {
		members = []objMember{}
	} else {
		var ok bool
		members, ok = scanObjectMembers(raw, 0)
		if !ok {
			if json.Valid(raw) {
				// Valid JSON but not an object: the generic path treats
				// it as an empty map.
				members = []objMember{}
			} else {
				return false, 0, nil // generic path reports 400
			}
		}
	}
	var title, bdy string
	var labelNames []string
	for _, m := range members {
		switch m.key {
		case "title":
			title = memberString(m)
		case "body":
			bdy = memberString(m)
		case "labels":
			if len(m.val) == 0 || m.val[0] != '[' {
				continue // generic path ignores a non-array labels value
			}
			// Walk the array elements.
			pos := 1
			for {
				pos = jsonSkipWS(m.val, pos)
				if pos >= len(m.val) {
					return false, 0, nil
				}
				if m.val[pos] == ']' {
					break
				}
				end, ok := jsonSkipValue(m.val, pos)
				if !ok {
					return false, 0, nil
				}
				elem := m.val[pos:end]
				var lname string
				if len(elem) > 0 && elem[0] == '"' {
					if err := json.Unmarshal(elem, &lname); err != nil {
						return false, 0, nil
					}
				} else if len(elem) > 0 && elem[0] == '{' {
					em, ok := scanObjectMembers(elem, 0)
					if !ok {
						return false, 0, nil
					}
					for _, emm := range em {
						if emm.key == "name" {
							lname = memberString(emm)
						}
					}
				}
				// Any other element type leaves lname "" (generic path:
				// the switch falls through with lname ""), which then
				// fails the label-existence check below.
				labelNames = append(labelNames, lname)
				pos = jsonSkipWS(m.val, end)
				if pos < len(m.val) && m.val[pos] == ',' {
					pos++
					continue
				}
				if pos < len(m.val) && m.val[pos] == ']' {
					break
				}
				return false, 0, nil
			}
		}
	}
	if err := requireOwner(owner); err != nil {
		return true, 404, jsonBytes(map[string]any{"message": "Not Found"})
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	repo, ok := store.repos[[2]string{owner, name}]
	if !ok {
		return true, 404, jsonBytes(map[string]any{"message": "Not Found"})
	}
	for _, lname := range labelNames {
		if _, ok := repo.Labels[lname]; !ok {
			return true, 422, jsonBytes(map[string]any{"message": "Label does not exist"})
		}
	}
	num := repo.NextIssue
	repo.NextIssue++
	issue := newIssue(repo, num, title, bdy, labelNames)
	repo.Issues[num] = issue
	repo.IssueOrder = append(repo.IssueOrder, num)
	// Manual serialization, byte-identical to issueRestJSON + "\n".
	var buf bytes.Buffer
	buf.WriteString(`{"id":`)
	buf.WriteString(strconv.Itoa(issue.Number*100000 + issue.Repo.DBID))
	buf.WriteString(`,"node_id":`)
	buf.Write(appendJSONString(nil, issue.GID))
	buf.WriteString(`,"number":`)
	buf.WriteString(strconv.Itoa(issue.Number))
	buf.WriteString(`,"title":`)
	buf.Write(appendJSONString(nil, issue.Title))
	buf.WriteString(`,"body":`)
	buf.Write(appendJSONString(nil, issue.Body))
	buf.WriteString(`,"state":`)
	buf.Write(appendJSONString(nil, issue.State))
	buf.WriteString(`,"labels":[`)
	for i, ln := range labelNames {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.WriteString(`{"name":`)
		buf.Write(appendJSONString(nil, ln))
		buf.WriteByte('}')
	}
	buf.WriteString(`],"user":{"login":`)
	buf.Write(appendJSONString(nil, viewerLogin))
	buf.WriteString(`,"id":`)
	buf.WriteString(strconv.Itoa(viewer.ID))
	buf.WriteString(`},"created_at":`)
	buf.Write(appendJSONString(nil, issue.CreatedAt))
	buf.WriteString(`,"updated_at":`)
	buf.Write(appendJSONString(nil, issue.UpdatedAt))
	buf.WriteString(`,"html_url":"","url":""}`)
	buf.WriteByte('\n')
	// Keep the read path's cache consistent with the generic path.
	issue.cachedRest = bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	return true, 201, buf.Bytes()
}
