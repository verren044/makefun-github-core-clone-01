// main.go — HTTP server, request dispatch, auth, and the entry lifecycle
// (bind ephemeral port, register with the env, poll /done, exit 0).
// Faithful port of v1's Handler + main().
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

var token = os.Getenv("MAKEFUN_SHOP_TOKEN")
var envURL = strings.TrimSuffix(os.Getenv("MAKEFUN_ENV_URL"), "/")

var gitRouteRe = regexp.MustCompile(`^/([^/]+)/([^/]+)\.git(/.*)$`)
var mcpIDRe = regexp.MustCompile(`"id"\s*:\s*("([^"\\]|\\.)*"|-?\d+)`)

func main() {
	// Short-lived trial process with a small heap: halving GC frequency
	// removes pauses from the timed rounds.
	debug.SetGCPercent(200)
	port := 0
	if v := os.Getenv("FORGE_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			port = n
		}
	}
	ln, err := net.Listen("tcp", "0.0.0.0:"+strconv.Itoa(port))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[entry] listen: %v\n", err)
		os.Exit(1)
	}
	bound := ln.Addr().(*net.TCPAddr).Port
	fmt.Printf("[entry] listening on port %d\n", bound)
	srv := &http.Server{Handler: http.HandlerFunc(dispatch)}
	if envURL == "" {
		fmt.Println("[entry] no MAKEFUN_ENV_URL; serving locally")
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "[entry] serve: %v\n", err)
			os.Exit(1)
		}
		return
	}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "[entry] serve: %v\n", err)
			os.Exit(1)
		}
	}()
	if err := register(bound); err != nil {
		fmt.Fprintf(os.Stderr, "[entry] register: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("[entry] registered; waiting for the env to say done")
	waitForDone()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	fmt.Println("[entry] done")
}

// requestID returns a fresh "req-%06d" (one per response, like v1).
var reqCounter atomic.Int64

// requestID formats "req-%06d" into a stack buffer instead of pad6's
// loop-concat; the fallback keeps exact behaviour past 999999.
func requestID() string {
	n := reqCounter.Add(1)
	if n > 999999 {
		return "req-" + pad6(int(n))
	}
	var buf [10]byte
	buf[0], buf[1], buf[2], buf[3] = 'r', 'e', 'q', '-'
	v := int(n)
	for i := 9; i >= 4; i-- {
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[:])
}

func writeJSON(w http.ResponseWriter, r *http.Request, status int, body []byte, extra map[string]string) {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Server", "Forge/1.0")
	h.Set("X-GitHub-Request-Id", requestID())
	for k, v := range extra {
		h.Set(k, v)
	}
	if body == nil {
		body = []byte{}
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if len(body) > 0 && r.Method != "HEAD" {
		_, _ = w.Write(body)
	}
}

func writeBytes(w http.ResponseWriter, r *http.Request, status int, body []byte, ctype string) {
	h := w.Header()
	h.Set("Content-Type", ctype)
	h.Set("Server", "Forge/1.0")
	h.Set("X-GitHub-Request-Id", requestID())
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if len(body) > 0 && r.Method != "HEAD" {
		_, _ = w.Write(body)
	}
}

func writeErr(w http.ResponseWriter, r *http.Request, status int, msg string) {
	writeJSON(w, r, status, jsonBytes(map[string]any{"message": msg}), nil)
}

func checkAuth(r *http.Request) error {
	if token == "" {
		return nil // local smoke-test mode: no token configured
	}
	if r.Header.Get("Authorization") != "Bearer "+token {
		return &apiError{401, "Bad credentials"}
	}
	return nil
}

func parseJSONBody(raw []byte) (any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, &apiError{400, "Invalid JSON"}
	}
	return v, nil
}

// dispatch is the single entry point. It mirrors v1's Handler.dispatch:
// read the body first, then route; git smart HTTP needs no auth, the API
// surfaces need the bearer token. Locking: read-only git paths (advertise,
// v2 upload-pack) take RLock so the 50 concurrent clones parallelize;
// everything that mutates (v0 upload-pack, receive-pack, REST, GraphQL,
// MCP) takes the write lock.
func dispatch(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if rec := recover(); rec != nil {
			// never leak a stack trace or HTML
			writeErr(w, r, 500, "Internal Server Error")
		}
	}()
	// Requests without a body (every GET in the read workload, the
	// advertise GETs) skip the ReadAll buffer allocation entirely;
	// chunked bodies (ContentLength -1) still read.
	var raw []byte
	if r.ContentLength != 0 {
		raw, _ = io.ReadAll(r.Body)
	}
	_ = r.Body.Close()
	method := r.Method
	path := r.URL.Path
	if method == "GET" && path == "/healthz" {
		writeJSON(w, r, 200, jsonBytes(map[string]any{"ok": true}), nil)
		return
	}
	// The git-route regexp only runs when the path contains ".git/";
	// every /api/v3 request skips it.
	var gm []string
	if strings.Contains(path, ".git/") {
		gm = gitRouteRe.FindStringSubmatch(path)
	}
	if gm != nil {
		dispatchGit(w, r, gm[1], gm[2], gm[3], raw)
		return
	}
	if err := checkAuth(r); err != nil {
		writeErr(w, r, err.(*apiError).status, err.(*apiError).msg)
		return
	}
	if path == "/api/graphql" || path == "/api/graphql/" {
		if method != "POST" {
			writeErr(w, r, 404, "Not Found")
			return
		}
		doc, err := parseJSONBody(raw)
		if err != nil {
			writeErr(w, r, 400, "Invalid JSON")
			return
		}
		store.mu.Lock()
		status, payload, gqlErr := handleGraphQL(doc)
		store.mu.Unlock()
		if gqlErr != nil {
			if ae, ok := gqlErr.(*apiError); ok {
				writeErr(w, r, ae.status, ae.msg)
			} else {
				writeErr(w, r, 500, "Internal Server Error")
			}
			return
		}
		writeJSON(w, r, status, jsonBytes(payload), nil)
		return
	}
	if path == "/mcp" {
		if method == "DELETE" {
			writeJSON(w, r, 200, jsonBytes(map[string]any{}), nil)
			return
		}
		if method != "POST" {
			writeErr(w, r, 404, "Not Found")
			return
		}
		if len(raw) == 0 {
			writeJSON(w, r, 400, jsonBytes(map[string]any{
				"jsonrpc": "2.0", "id": nil,
				"error": map[string]any{"code": -32700, "message": "Parse error"},
			}), nil)
			return
		}
		var doc any
		if err := json.Unmarshal(raw, &doc); err != nil {
			// A truncated body can still carry a readable id; echo it.
			var reqID any
			if mm := mcpIDRe.FindSubmatch(raw); mm != nil {
				_ = json.Unmarshal(mm[1], &reqID)
			}
			writeJSON(w, r, 400, jsonBytes(map[string]any{
				"jsonrpc": "2.0", "id": reqID,
				"error": map[string]any{"code": -32700, "message": "Parse error"},
			}), nil)
			return
		}
		store.mu.Lock()
		status, payload, isNotif := handleMCP(doc)
		store.mu.Unlock()
		if isNotif {
			h := w.Header()
			h.Set("Server", "Forge/1.0")
			h.Set("X-GitHub-Request-Id", requestID())
			h.Set("Content-Length", "0")
			w.WriteHeader(status)
			return
		}
		writeJSON(w, r, status, jsonBytes(payload), nil)
		return
	}
	if strings.HasPrefix(path, "/api/v3") {
		var body any
		// Issue creates (the write workload's 200 POSTs) take the
		// hand-rolled fast path when the body proves identical;
		// anything else falls back to the generic Unmarshal below.
		if method == "POST" && strings.HasSuffix(path, "/issues") {
			if fb, ok := fastIssueBody(raw); ok {
				body = fb
			}
		}
		if body == nil {
			var err error
			body, err = parseJSONBody(raw)
			if err != nil {
				writeErr(w, r, 400, "Invalid JSON")
				return
			}
		}
		headers := map[string]string{}
		// Go's server keeps Host in r.Host, never in r.Header: r.Header.Get("Host")
		// is always empty, which left the /user/repos Link relative and failed
		// repos.pagination (sameOrigin does new URL(a) — a relative URL throws).
		if h := r.Host; h != "" {
			headers["Host"] = h
		}
		store.mu.Lock()
		// r.URL.Query() parses (and allocates) even with no query
		// string; nil Values.Get returns "" exactly like an empty map.
		var query url.Values
		if r.URL.RawQuery != "" {
			query = r.URL.Query()
		}
		res, derr := restDispatch(method, path, query, body, headers)
		store.mu.Unlock()
		if derr != nil {
			if ae, ok := derr.(*apiError); ok {
				writeErr(w, r, ae.status, ae.msg)
			} else {
				writeErr(w, r, 500, "Internal Server Error")
			}
			return
		}
		writeJSON(w, r, res.status, res.body, res.extra)
		return
	}
	writeErr(w, r, 404, "Not Found")
}

func dispatchGit(w http.ResponseWriter, r *http.Request, owner, name, rest string, raw []byte) {
	method := r.Method
	// Lock mode is chosen by route before the lookup: advertise and v2
	// upload-pack are read-only (RLock, so the 50 concurrent clones
	// parallelize); v0 upload-pack and receive-pack mutate (Lock).
	readOnly := false
	if method == "GET" && rest == "/info/refs" {
		readOnly = true
	}
	if method == "POST" && rest == "/git-upload-pack" && strings.Contains(r.Header.Get("Git-Protocol"), "version=2") {
		readOnly = true
	}
	if readOnly {
		store.mu.RLock()
		defer store.mu.RUnlock()
	} else {
		store.mu.Lock()
		defer store.mu.Unlock()
	}
	repo, ok := store.repos[[2]string{owner, name}]
	if !ok {
		writeErr(w, r, 404, "Not Found")
		return
	}
	if method == "GET" && rest == "/info/refs" {
		service := r.URL.Query().Get("service")
		if service == "git-upload-pack" {
			proto := r.Header.Get("Git-Protocol")
			var out []byte
			if strings.Contains(proto, "version=2") {
				out = gitAdvertiseV2()
			} else {
				out = gitAdvertiseV0(repo, "git-upload-pack")
			}
			writeBytes(w, r, 200, out, "application/x-git-upload-pack-advertisement")
			return
		}
		if service == "git-receive-pack" {
			out := gitAdvertiseV0(repo, "git-receive-pack")
			writeBytes(w, r, 200, out, "application/x-git-receive-pack-advertisement")
			return
		}
		writeErr(w, r, 404, "Not Found")
		return
	}
	if method == "POST" && rest == "/git-upload-pack" {
		proto := r.Header.Get("Git-Protocol")
		var out []byte
		if strings.Contains(proto, "version=2") {
			out = handleUploadPackV2(repo, raw)
		} else {
			out = handleUploadPackV0(repo, raw)
		}
		writeBytes(w, r, 200, out, "application/x-git-upload-pack-result")
		return
	}
	if method == "POST" && rest == "/git-receive-pack" {
		out := handleReceivePackV0(repo, raw)
		writeBytes(w, r, 200, out, "application/x-git-receive-pack-result")
		return
	}
	writeErr(w, r, 404, "Not Found")
}

// ---- lifecycle (register / wait-for-done)

var httpClient = &http.Client{Timeout: 30 * time.Second}

func httpRequest(method, url string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	return res.StatusCode, data, err
}

func retryDelay(attempt int) time.Duration {
	d := 250 * time.Millisecond
	for i := 0; i < attempt && i < 8; i++ {
		d *= 2
	}
	if d > 2000*time.Millisecond {
		d = 2000 * time.Millisecond
	}
	return d
}

func register(port int) error {
	for attempt := 0; ; attempt++ {
		status, _, err := httpRequest("POST", envURL+"/register", map[string]any{"port": port})
		if err == nil {
			if status == 200 || status == 409 {
				return nil
			}
			if status == 400 || status == 405 {
				return fmt.Errorf("register answered HTTP %d", status)
			}
		}
		time.Sleep(retryDelay(attempt))
	}
}

func waitForDone() {
	for attempt := 0; ; attempt++ {
		status, raw, err := httpRequest("GET", envURL+"/done", nil)
		if err == nil && status == 200 {
			var doc map[string]any
			if json.Unmarshal(raw, &doc) == nil {
				if done, _ := doc["done"].(bool); done {
					return
				}
			}
		}
		time.Sleep(retryDelay(attempt))
	}
}
