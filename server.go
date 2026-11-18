// HTTP server (raw TCP, hand-rolled HTTP/1.1), routing dispatch, the
// trial lifecycle (register + /done long-poll), and the scenario seed that
// re-derives the viewer login. Faithful port of the Node forge's server.

package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"fmt"
	"net"
	"net/url"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// --- scenario seed (wire.ts SCENARIO_SEED): viewer is users[0] ---

const scenarioSeed = uint64(0x6a09e667f3bcc909)
const u64Mask = uint64(0xffffffffffffffff)

func fnv1a64(text string) uint64 {
	hash := uint64(0xcbf29ce484222325)
	for i := 0; i < len(text); i++ {
		hash ^= uint64(text[i])
		hash = (hash * 0x100000001b3) & u64Mask
	}
	return hash
}

func mix64(value uint64) uint64 {
	z := (value + 0x9e3779b97f4a7c15) & u64Mask
	z = ((z ^ (z >> 30)) * 0xbf58476d1ce4e5b9) & u64Mask
	z = ((z ^ (z >> 27)) * 0x94d049bb133111eb) & u64Mask
	return (z ^ (z >> 31)) & u64Mask
}

type rng struct{ state uint64 }

func newRng(seed uint64) *rng {
	masked := seed & u64Mask
	if masked == 0 {
		masked = scenarioSeed
	}
	return &rng{state: masked}
}

func (r *rng) nextU64() uint64 {
	x := r.state
	x = (x ^ (x >> 12)) & u64Mask
	x = (x ^ (x << 25)) & u64Mask
	x = (x ^ (x >> 27)) & u64Mask
	r.state = x
	return (x * 0x2545f4914f6cdd1d) & u64Mask
}

func (r *rng) nextU32() uint32 { return uint32(r.nextU64() >> 32) }

func (r *rng) nextInt(maxExclusive int) int {
	if maxExclusive == 1 {
		return 0
	}
	const range32 = 4294967296
	limit := range32 - (range32 % uint64(maxExclusive))
	for {
		v := uint64(r.nextU32())
		if v < limit {
			return int(v % uint64(maxExclusive))
		}
	}
}

func (r *rng) pick(items []string) string { return items[r.nextInt(len(items))] }

var firstNames = []string{"ada", "bram", "cora", "dmitri", "elena", "felix", "greta", "hugo", "iris", "jonas", "kira", "lars", "mira", "nico", "olga", "pavel", "quinn", "rosa", "sven", "tess", "ulla", "vito", "wren", "yara", "zane"}
var lastNames = []string{"anders", "bauer", "costa", "dahl", "eriksen", "fischer", "garcia", "holm", "iversen", "jansen", "keller", "lind", "moreau", "novak", "olsen", "peters", "quist", "rossi", "sato", "turi", "ulrich", "vega", "weber", "xu", "yilmaz", "zimmer"}

type scenarioUser struct {
	id    int
	login string
	name  string
	email string
}

func scenarioUsers() []scenarioUser {
	r := newRng(mix64(scenarioSeed ^ fnv1a64("users")))
	used := make(map[string]bool)
	var users []scenarioUser
	for i := 0; i < 12; i++ {
		first := r.pick(firstNames)
		last := r.pick(lastNames)
		base := strings.Map(func(ch rune) rune {
			if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '-' {
				return ch
			}
			return -1
		}, first+last)
		login := base
		suffix := 2
		for used[login] {
			login = base + "-" + strconv.Itoa(suffix)
			suffix++
		}
		used[login] = true
		users = append(users, scenarioUser{id: 1000 + i, login: login, name: first + " " + last, email: login + "@example.invalid"})
	}
	return users
}

var scenarioUserList = scenarioUsers()
var viewerLogin = scenarioUserList[0].login
var viewerName = scenarioUserList[0].name
var viewerEmail = scenarioUserList[0].email
var viewerID = scenarioUserList[0].id

// --- small helpers ---

func b64URLEncode(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}

func b64URLDecode(s string) (string, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func b64Std(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}

func b64DecodeLenient(s string) []byte {
	// Node's Buffer.from(s, 'base64') skips characters outside the alphabet.
	var clean []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '+' || c == '/' || c == '=' {
			clean = append(clean, c)
		}
	}
	out, _ := base64.StdEncoding.DecodeString(string(clean))
	return out
}

// --- config ---

var shopToken = os.Getenv("MAKEFUN_SHOP_TOKEN")
var envURL = os.Getenv("MAKEFUN_ENV_URL")

var requestCounter atomic.Int64

func nextRequestID() string {
	return "forge-" + strconv.FormatInt(requestCounter.Add(1), 10)
}

func authed(headers map[string]string) bool {
	h, ok := headers["authorization"]
	if !ok || !strings.HasPrefix(h, "Bearer ") {
		return false
	}
	token := h[len("Bearer "):]
	if token == "" {
		return false
	}
	if shopToken == "" {
		return true
	}
	return token == shopToken
}

// --- HTTP server ---

type httpResponse struct {
	status      int
	contentType string
	body        []byte
	headers     [][2]string
	noBody      bool
}

func jsonResp(status int, body []byte, headers ...[2]string) httpResponse {
	return httpResponse{status: status, contentType: "application/json; charset=utf-8", body: body, headers: headers}
}

func jsonErr(status int, msg string) httpResponse {
	return httpResponse{status: status, contentType: "application/json; charset=utf-8", body: []byte("{\"message\":" + jsonQuote(msg) + "}\n")}
}

var bufReaderPool = sync.Pool{New: func() any { return bufio.NewReaderSize(nil, 32768) }}
var bufWriterPool = sync.Pool{New: func() any { return bufio.NewWriterSize(nil, 32768) }}

type httpConn struct {
	nc net.Conn
	br *bufio.Reader
	bw *bufio.Writer
}

var statusText = map[int]string{
	200: "OK", 201: "Created", 202: "Accepted", 204: "No Content",
	400: "Bad Request", 401: "Unauthorized", 404: "Not Found",
	405: "Method Not Allowed", 409: "Conflict", 422: "Unprocessable Entity",
	500: "Internal Server Error",
}

func (c *httpConn) writeResp(resp httpResponse, keepAlive bool) {
	text, ok := statusText[resp.status]
	if !ok {
		text = "OK"
	}
	var b []byte
	b = append(b, "HTTP/1.1 "...)
	b = strconv.AppendInt(b, int64(resp.status), 10)
	b = append(b, ' ')
	b = append(b, text...)
	b = append(b, "\r\n"...)
	if resp.contentType != "" {
		b = append(b, "content-type: "...)
		b = append(b, resp.contentType...)
		b = append(b, "\r\n"...)
	}
	bodyLen := len(resp.body)
	if resp.noBody {
		bodyLen = 0
	}
	b = append(b, "content-length: "...)
	b = strconv.AppendInt(b, int64(bodyLen), 10)
	b = append(b, "\r\n"...)
	b = append(b, "x-github-request-id: "...)
	b = append(b, nextRequestID()...)
	b = append(b, "\r\n"...)
	for _, h := range resp.headers {
		b = append(b, strings.ToLower(h[0])...)
		b = append(b, ": "...)
		b = append(b, h[1]...)
		b = append(b, "\r\n"...)
	}
	if !keepAlive {
		b = append(b, "connection: close\r\n"...)
	}
	b = append(b, "\r\n"...)
	if !resp.noBody {
		b = append(b, resp.body...)
	}
	_, _ = c.bw.Write(b)
	_ = c.bw.Flush()
}

// readLine reads one CRLF-terminated line (without the CRLF).
func (c *httpConn) readLine() (string, error) {
	line, err := c.br.ReadSlice('\n')
	if err != nil {
		return "", err
	}
	s := string(line)
	s = strings.TrimRight(s, "\r\n")
	return s, nil
}

func handleConn(nc net.Conn) {
	defer nc.Close()
	br := bufReaderPool.Get().(*bufio.Reader)
	br.Reset(nc)
	defer bufReaderPool.Put(br)
	bw := bufWriterPool.Get().(*bufio.Writer)
	bw.Reset(nc)
	defer bufWriterPool.Put(bw)
	c := &httpConn{nc: nc, br: br, bw: bw}
	for {
		keepAlive, done := c.serveOne()
		if done {
			return
		}
		if !keepAlive {
			return
		}
	}
}

// serveOne handles a single request on the connection.
// Returns (keepAlive, done).
func (c *httpConn) serveOne() (bool, bool) {
	reqLine, err := c.readLine()
	if err != nil {
		return false, true
	}
	if reqLine == "" {
		// tolerate a stray blank line before the request line
		reqLine, err = c.readLine()
		if err != nil {
			return false, true
		}
	}
	parts := strings.SplitN(reqLine, " ", 3)
	if len(parts) != 3 {
		return false, true
	}
	method, target, version := parts[0], parts[1], parts[2]
	headers := make(map[string]string)
	contentLength := -1
	chunked := false
	for {
		line, err := c.readLine()
		if err != nil {
			return false, true
		}
		if line == "" {
			break
		}
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(line[:colon]))
		val := strings.TrimSpace(line[colon+1:])
		headers[key] = val
		switch key {
		case "content-length":
			if n, err := strconv.Atoi(val); err == nil {
				contentLength = n
			}
		case "transfer-encoding":
			if strings.Contains(strings.ToLower(val), "chunked") {
				chunked = true
			}
		case "expect":
			if strings.EqualFold(val, "100-continue") {
				_, _ = c.bw.WriteString("HTTP/1.1 100 Continue\r\n\r\n")
				_ = c.bw.Flush()
			}
		}
	}
	keepAlive := true
	if version == "HTTP/1.0" {
		keepAlive = strings.EqualFold(headers["connection"], "keep-alive")
	} else if strings.EqualFold(headers["connection"], "close") {
		keepAlive = false
	}
	// Health + root answer without reading a body (there is none on GET).
	u, err := url.Parse(target)
	if err != nil {
		c.writeResp(jsonErr(400, "bad request target"), false)
		return false, true
	}
	pathname := u.Path
	if pathname == "" {
		pathname = "/"
	}
	if pathname == "/healthz" {
		c.writeResp(jsonResp(200, []byte("{\"ok\":true}\n")), keepAlive)
		return keepAlive, false
	}
	if pathname == "/" {
		c.writeResp(jsonResp(200, []byte("{\"ok\":true,\"service\":\"github-forge\"}\n")), keepAlive)
		return keepAlive, false
	}
	// Read the body.
	var rawBody []byte
	if chunked {
		rawBody, err = c.readChunked()
		if err != nil {
			return false, true
		}
	} else if contentLength > 0 {
		if contentLength > 64*1024*1024 {
			c.writeResp(jsonErr(400, "body too large"), false)
			return false, true
		}
		rawBody = make([]byte, contentLength)
		if _, err := readFull(c.br, rawBody); err != nil {
			return false, true
		}
	}
	resp := dispatch(method, pathname, u, headers, rawBody)
	c.writeResp(resp, keepAlive)
	return keepAlive, false
}

func readFull(r *bufio.Reader, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := r.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func (c *httpConn) readChunked() ([]byte, error) {
	var out []byte
	for {
		line, err := c.readLine()
		if err != nil {
			return nil, err
		}
		sizeStr := line
		if i := strings.IndexByte(sizeStr, ';'); i >= 0 {
			sizeStr = sizeStr[:i]
		}
		n, err := strconv.ParseInt(strings.TrimSpace(sizeStr), 16, 64)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			// trailing headers until blank line
			for {
				l, err := c.readLine()
				if err != nil {
					return nil, err
				}
				if l == "" {
					break
				}
			}
			return out, nil
		}
		if int64(len(out))+n > 64*1024*1024 {
			return nil, errChunkTooLarge
		}
		chunk := make([]byte, n)
		if _, err := readFull(c.br, chunk); err != nil {
			return nil, err
		}
		out = append(out, chunk...)
		// trailing CRLF
		if _, err := c.readLine(); err != nil {
			return nil, err
		}
	}
}

var errChunkTooLarge = &simpleError{"body too large"}

type simpleError struct{ s string }

func (e *simpleError) Error() string { return e.s }

// --- routing ---

func dispatch(method, pathname string, u *url.URL, headers map[string]string, rawBody []byte) (resp httpResponse) {
	defer func() {
		if r := recover(); r != nil {
			os.Stderr.WriteString("forge panic: " + fmt.Sprintf("%v", r) + "\n")
			debug.PrintStack()
			resp = jsonErr(500, "internal error")
		}
	}()
	host := headers["host"]
	if host == "" {
		host = "127.0.0.1"
	}
	q := u.Query()
	// MCP
	if pathname == "/mcp" {
		if method == "DELETE" {
			return httpResponse{status: 202, noBody: true}
		}
		if method != "POST" {
			return jsonErr(404, "Not Found")
		}
		if !authed(headers) {
			return httpResponse{status: 401, contentType: "application/json", body: []byte("{\"jsonrpc\":\"2.0\",\"id\":null,\"error\":{\"code\":-32000,\"message\":\"Requires authentication\"}}")}
		}
		status, body := handleMcp(rawBody)
		if status == 202 {
			return httpResponse{status: 202, noBody: true}
		}
		return httpResponse{status: status, contentType: "application/json", body: body}
	}
	// GraphQL
	if pathname == "/api/graphql" {
		if method != "POST" {
			return jsonErr(404, "Not Found")
		}
		if !authed(headers) {
			return jsonErr(401, "Requires authentication")
		}
		return handleGraphql(rawBody)
	}
	// REST v3
	if pathname == "/api/v3" || strings.HasPrefix(pathname, "/api/v3/") {
		var body JVal = map[string]JVal{}
		if len(rawBody) > 0 {
			parsed, err := parseJSON(rawBody)
			if err != nil {
				return jsonErr(400, "Request body is not valid JSON")
			}
			body = parsed
		}
		if !authed(headers) {
			return jsonErr(401, "Requires authentication")
		}
		segments := splitSegments(pathname)
		// segments after /api/v3
		rest := segments[2:]
		r := handleRest(method, rest, q, host, pathname, body)
		return restResultToHTTP(r)
	}
	// Git smart HTTP
	if owner, name, tail, ok := matchGitPath(pathname); ok {
		if tail == "info/refs" {
			service := q.Get("service")
			if service != "git-upload-pack" && service != "git-receive-pack" {
				return jsonErr(400, "service is required")
			}
			status, ct, body, msg := handleGit(method, owner, name, service, rawBody, headers["git-protocol"])
			if msg != "" {
				return jsonErr(status, msg)
			}
			return httpResponse{status: status, contentType: ct, body: body}
		}
		status, ct, body, msg := handleGit(method, owner, name, tail, rawBody, headers["git-protocol"])
		if msg != "" {
			return jsonErr(status, msg)
		}
		return httpResponse{status: status, contentType: ct, body: body}
	}
	return jsonErr(404, "Not Found")
}

func restResultToHTTP(r restResult) httpResponse {
	if r.noBody {
		return httpResponse{status: r.status, headers: r.headers, noBody: true}
	}
	body := r.body
	// REST JSON replies end with a newline (Node's jsonBody appends one).
	if len(body) > 0 && body[len(body)-1] != '\n' {
		body = append(body, '\n')
	}
	return httpResponse{status: r.status, contentType: "application/json; charset=utf-8", body: body, headers: r.headers}
}

func splitSegments(pathname string) []string {
	parts := strings.Split(pathname, "/")
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// matchGitPath matches /{owner}/{repo}.git/{info/refs|git-upload-pack|git-receive-pack}
func matchGitPath(pathname string) (owner, name, tail string, ok bool) {
	if !strings.HasPrefix(pathname, "/") {
		return "", "", "", false
	}
	rest := pathname[1:]
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		return "", "", "", false
	}
	owner = rest[:slash]
	rest = rest[slash+1:]
	slash = strings.IndexByte(rest, '/')
	if slash < 0 {
		return "", "", "", false
	}
	repoSeg := rest[:slash]
	tail = rest[slash+1:]
	if !strings.HasSuffix(repoSeg, ".git") {
		return "", "", "", false
	}
	name = repoSeg[:len(repoSeg)-4]
	if owner == "" || name == "" {
		return "", "", "", false
	}
	if tail != "info/refs" && tail != "git-upload-pack" && tail != "git-receive-pack" {
		return "", "", "", false
	}
	if u, err := url.PathUnescape(owner); err == nil {
		owner = u
	}
	if u, err := url.PathUnescape(name); err == nil {
		name = u
	}
	return owner, name, tail, true
}

// --- GraphQL handler ---

func handleGraphql(rawBody []byte) httpResponse {
	var body JVal
	if err := func() error {
		v, err := parseJSON(rawBody)
		if err != nil {
			return err
		}
		body = v
		return nil
	}(); err != nil {
		return jsonErr(400, "Request body is not valid JSON")
	}
	ops, isArray := body.([]JVal)
	if !isArray {
		ops = []JVal{body}
	}
	// Mutations take the write lock, queries the read lock.
	mutating := bytes.Contains(bytes.ToLower(rawBody), []byte("mutation"))
	if mutating {
		db.mu.Lock()
		defer db.mu.Unlock()
	} else {
		db.mu.RLock()
		defer db.mu.RUnlock()
	}
	var out []byte
	out = append(out, '[')
	for i, op := range ops {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, gqlOneOp(op)...)
	}
	out = append(out, ']')
	if !isArray {
		// single result object, not an array
		out = out[1 : len(out)-1]
	}
	return jsonResp(200, out)
}

func gqlOneOp(op JVal) []byte {
	m, ok := op.(map[string]JVal)
	if !ok {
		return []byte(`{"data":null,"errors":[{"message":"query is required"}]}`)
	}
	query, ok := m["query"].(string)
	if !ok {
		return []byte(`{"data":null,"errors":[{"message":"query is required"}]}`)
	}
	variables, _ := m["variables"].(map[string]JVal)
	if variables == nil {
		variables = map[string]JVal{}
	}
	results, allErrors, err := gqlExecute(query, variables)
	if err != nil {
		return []byte(`{"data":null,"errors":[{"message":` + jsonQuote(err.Error()) + `}]}`)
	}
	data := results[0].bytes()
	var b []byte
	b = append(b, `{"data":`...)
	b = append(b, data...)
	if len(allErrors[0]) > 0 {
		b = append(b, `,"errors":[`...)
		for i, e := range allErrors[0] {
			if i > 0 {
				b = append(b, ',')
			}
			b = append(b, `{"message":`...)
			b = appendJSONString(b, e)
			b = append(b, '}')
		}
		b = append(b, ']')
	}
	b = append(b, '}')
	return b
}

// --- trial lifecycle (env register + /done poll) ---

func envRequest(method, urlStr string, payload []byte) (int, string, error) {
	u, err := url.Parse(urlStr)
	if err != nil {
		return 0, "", err
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		if u.Scheme == "https" {
			host += ":443"
		} else {
			host += ":80"
		}
	}
	conn, err := net.DialTimeout("tcp", host, 5*time.Second)
	if err != nil {
		return 0, "", err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	var req bytes.Buffer
	req.WriteString(method + " " + u.RequestURI() + " HTTP/1.1\r\n")
	req.WriteString("host: " + u.Host + "\r\n")
	req.WriteString("content-type: application/json\r\n")
	if payload != nil {
		req.WriteString("content-length: " + strconv.Itoa(len(payload)) + "\r\n")
	} else {
		req.WriteString("content-length: 0\r\n")
	}
	req.WriteString("connection: close\r\n\r\n")
	if payload != nil {
		req.Write(payload)
	}
	if _, err := conn.Write(req.Bytes()); err != nil {
		return 0, "", err
	}
	br := bufio.NewReader(conn)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		return 0, "", err
	}
	var status int
	_, _ = parseStatusLine(statusLine, &status)
	contentLength := -1
	chunked := false
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return 0, "", err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if i := strings.IndexByte(line, ':'); i >= 0 {
			k := strings.ToLower(strings.TrimSpace(line[:i]))
			v := strings.TrimSpace(line[i+1:])
			if k == "content-length" {
				contentLength, _ = strconv.Atoi(v)
			} else if k == "transfer-encoding" && strings.Contains(strings.ToLower(v), "chunked") {
				chunked = true
			}
		}
	}
	var body []byte
	if chunked {
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return 0, "", err
			}
			n, err := strconv.ParseInt(strings.TrimSpace(strings.SplitN(line, ";", 2)[0]), 16, 64)
			if err != nil {
				return 0, "", err
			}
			if n == 0 {
				break
			}
			chunk := make([]byte, n)
			if _, err := readFull(br, chunk); err != nil {
				return 0, "", err
			}
			body = append(body, chunk...)
			_, _ = br.ReadString('\n')
		}
	} else if contentLength > 0 {
		body = make([]byte, contentLength)
		if _, err := readFull(br, body); err != nil {
			return 0, "", err
		}
	}
	return status, string(body), nil
}

func parseStatusLine(line string, status *int) (bool, error) {
	parts := strings.SplitN(strings.TrimRight(line, "\r\n"), " ", 3)
	if len(parts) < 2 {
		return false, errChunkTooLarge
	}
	n, err := strconv.Atoi(parts[1])
	if err != nil {
		return false, err
	}
	*status = n
	return true, nil
}

func main() {
	mcpToolsListJSON = buildToolsListJSON()
	port := 0
	if v := os.Getenv("FORGE_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			port = n
		}
	}
	// Listen on all interfaces (0.0.0.0), not loopback only: the grader
	// runs the entry in its own sandbox/pod and reaches it at the
	// sandbox's address (the env forms the entry URL from the register
	// request's peer address). A loopback-only listener is unreachable
	// from outside the sandbox, and the trial fails with "fetch failed"
	// on every workload. Loopback clients (practice runner, sealed
	// trials) still connect fine.
	ln, err := net.Listen("tcp", ":"+strconv.Itoa(port))
	if err != nil {
		os.Stderr.WriteString("fatal: listen: " + err.Error() + "\n")
		os.Exit(1)
	}
	actualPort := ln.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handleConn(conn)
		}
	}()
	if envURL == "" {
		os.Stdout.WriteString("github-forge listening on 127.0.0.1:" + strconv.Itoa(actualPort) + "\n")
		select {}
	}
	// Suite mode: register, then long-poll /done.
	registered := false
	delay := 250 * time.Millisecond
	for attempt := 0; attempt < 40 && !registered; attempt++ {
		status, _, err := envRequest("POST", strings.TrimRight(envURL, "/")+"/register", []byte("{\"port\":"+strconv.Itoa(actualPort)+"}"))
		if err == nil {
			if status == 200 || status == 409 {
				registered = true
			} else if status == 400 || status == 405 {
				os.Stderr.WriteString("fatal: register failed: " + strconv.Itoa(status) + "\n")
				os.Exit(1)
			}
		}
		if !registered {
			time.Sleep(delay)
			delay *= 2
			if delay > 2*time.Second {
				delay = 2 * time.Second
			}
		}
	}
	if !registered {
		os.Stderr.WriteString("fatal: could not register with suite env\n")
		os.Exit(1)
	}
	for {
		time.Sleep(500 * time.Millisecond)
		status, text, err := envRequest("GET", strings.TrimRight(envURL, "/")+"/done", nil)
		if err == nil && status == 200 {
			if v, err := parseJSON([]byte(text)); err == nil {
				if done, _ := jGet(v, "done"); done == true {
					break
				}
			}
		}
	}
	_ = ln.Close()
	os.Exit(0)
}
