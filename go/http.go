// Hand-rolled HTTP/1.1 server on net: the suite speaks plain HTTP/1.1 with
// Content-Length bodies over keep-alive connections, and a lean server (no
// net/http) keeps both the binary small and per-request allocation low.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type hdr struct{ k, v string } // k is lower-cased at parse time

type request struct {
	method    string
	target    string
	path      string
	rawQuery  string
	query     url.Values
	headers   []hdr // small slice + linear scan: no map alloc per request
	body      []byte
	proto11   bool
	keepAlive bool
}

func (r *request) header(k string) string {
	for _, h := range r.headers {
		if h.k == k {
			return h.v
		}
	}
	return ""
}

func (r *request) q(k string) string {
	if r.query == nil {
		q, _ := url.ParseQuery(r.rawQuery)
		r.query = q
	}
	return r.query.Get(k)
}

// response is what a handler returns; the connection loop serializes it.
type response struct {
	status  int
	ctype   string
	body    []byte
	headers map[string]string // extra headers, e.g. Link, X-OAuth-Scopes
	noBody  bool              // HEAD: headers only
}

func statusText(code int) string {
	switch code {
	case 200:
		return "OK"
	case 201:
		return "Created"
	case 202:
		return "Accepted"
	case 204:
		return "No Content"
	case 400:
		return "Bad Request"
	case 401:
		return "Unauthorized"
	case 404:
		return "Not Found"
	case 405:
		return "Method Not Allowed"
	case 409:
		return "Conflict"
	case 422:
		return "Unprocessable Entity"
	case 500:
		return "Internal Server Error"
	default:
		return "Status"
	}
}

func serveConn(c net.Conn) {
	defer c.Close()
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
	br := bufio.NewReaderSize(c, 65536)
	for {
		_ = c.SetReadDeadline(time.Now().Add(60 * time.Second))
		req, err := readRequest(br, c)
		if err != nil {
			return
		}
		// v4: dispatch takes mu itself for the general path; the fast
		// paths inside it run lock-free, so no lock is held here.
		resp := dispatch(req)
		if !writeResponse(c, req, resp) {
			return
		}
		if !req.keepAlive {
			return
		}
	}
}

// readLine reads one \n-terminated line without allocating (ReadSlice
// returns a slice of the buffer); only over-long lines fall back to a
// copying read. The returned slice is only valid until the next read.
func readLine(br *bufio.Reader) ([]byte, error) {
	line, err := br.ReadSlice('\n')
	if err == nil {
		return line, nil
	}
	if err != bufio.ErrBufferFull {
		return nil, err
	}
	// Over-long line: accumulate it (rare; correctness over speed).
	var out []byte
	out = append(out, line...)
	for {
		chunk, err := br.ReadSlice('\n')
		out = append(out, chunk...)
		if err == nil {
			return out, nil
		}
		if err != bufio.ErrBufferFull {
			return nil, err
		}
	}
}

func trimCRLF(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

func readRequest(br *bufio.Reader, c net.Conn) (*request, error) {
	// Skip stray empty lines before the request line.
	var line []byte
	var err error
	for {
		line, err = readLine(br)
		if err != nil {
			return nil, err
		}
		line = trimCRLF(line)
		if len(line) != 0 {
			break
		}
	}
	// Request line: METHOD SP TARGET SP VERSION (no allocation beyond the
	// two kept strings; path/query slice the target string).
	sp1 := bytes.IndexByte(line, ' ')
	sp2 := bytes.LastIndexByte(line, ' ')
	if sp1 < 0 || sp2 <= sp1 {
		return nil, fmt.Errorf("bad request line")
	}
	req := &request{
		method:  string(line[:sp1]),
		target:  string(line[sp1+1 : sp2]),
		proto11: string(line[sp2+1:]) == "HTTP/1.1",
	}
	path, query, _ := strings.Cut(req.target, "?")
	req.path = path
	req.rawQuery = query
	// Headers.
	for {
		h, err := readLine(br)
		if err != nil {
			return nil, err
		}
		h = trimCRLF(h)
		if len(h) == 0 {
			break
		}
		k, v, ok := bytes.Cut(h, []byte(":"))
		if !ok {
			continue
		}
		req.headers = append(req.headers, hdr{
			k: strings.ToLower(strings.TrimSpace(string(k))),
			v: strings.TrimSpace(string(v)),
		})
	}
	req.keepAlive = req.proto11
	if conn := req.header("connection"); conn != "" {
		lc := strings.ToLower(conn)
		if strings.Contains(lc, "close") {
			req.keepAlive = false
		} else if strings.Contains(lc, "keep-alive") {
			req.keepAlive = true
		}
	}
	// 100-continue: answer the interim response so the client sends its body.
	if strings.EqualFold(req.header("expect"), "100-continue") {
		if _, err := c.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n")); err != nil {
			return nil, err
		}
	}
	// Body.
	if te := req.header("transfer-encoding"); strings.Contains(strings.ToLower(te), "chunked") {
		body, err := readChunked(br)
		if err != nil {
			return nil, err
		}
		req.body = body
	} else if cl := req.header("content-length"); cl != "" {
		n, err := strconv.Atoi(strings.TrimSpace(cl))
		if err != nil || n < 0 || n > 64<<20 {
			return nil, fmt.Errorf("bad content-length")
		}
		if n > 0 {
			body := make([]byte, n)
			if _, err := io.ReadFull(br, body); err != nil {
				return nil, err
			}
			req.body = body
		}
	}
	return req, nil
}

func readChunked(br *bufio.Reader) ([]byte, error) {
	var out bytes.Buffer
	for {
		line, err := readLine(br)
		if err != nil {
			return nil, err
		}
		line = trimCRLF(line)
		if i := bytes.IndexByte(line, ';'); i >= 0 {
			line = line[:i]
		}
		n, err := strconv.ParseInt(strings.TrimSpace(string(line)), 16, 64)
		if err != nil || n < 0 || n > 64<<20 {
			return nil, fmt.Errorf("bad chunk size")
		}
		if n == 0 {
			// Trailers until an empty line.
			for {
				t, err := readLine(br)
				if err != nil {
					return nil, err
				}
				if len(trimCRLF(t)) == 0 {
					break
				}
			}
			return out.Bytes(), nil
		}
		chunk := make([]byte, n)
		if _, err := io.ReadFull(br, chunk); err != nil {
			return nil, err
		}
		out.Write(chunk)
		if _, err := io.ReadFull(br, make([]byte, 2)); err != nil {
			return nil, err
		}
	}
}

// hbPool reuses the header buffers writeResponse builds per response.
var hbPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

func writeResponse(c net.Conn, req *request, resp *response) bool {
	hb := hbPool.Get().(*bytes.Buffer)
	defer func() {
		hb.Reset()
		hbPool.Put(hb)
	}()
	bodyLen := len(resp.body)
	if resp.noBody {
		bodyLen = 0
	}
	_ = c.SetWriteDeadline(time.Now().Add(30 * time.Second))
	hb.WriteString("HTTP/1.1 " + itoa(resp.status) + " " + statusText(resp.status) + "\r\n")
	if resp.ctype != "" {
		hb.WriteString("Content-Type: " + resp.ctype + "\r\n")
	}
	hb.WriteString("Content-Length: " + itoa(bodyLen) + "\r\n")
	id := reqCounter.Add(1)
	hb.WriteString("X-GitHub-Request-Id: req-" + itoa(int(id)) + "\r\n")
	for k, v := range resp.headers {
		hb.WriteString(k + ": " + v + "\r\n")
	}
	if req.keepAlive {
		hb.WriteString("Connection: keep-alive\r\n")
	} else {
		hb.WriteString("Connection: close\r\n")
	}
	hb.WriteString("\r\n")
	// One write for headers+body: a split write can stall on Nagle/delayed
	// ACK between the two segments on sequential keep-alive traffic.
	if !resp.noBody && len(resp.body) > 0 {
		hb.Write(resp.body)
	}
	if _, err := c.Write(hb.Bytes()); err != nil {
		return false
	}
	return true
}

func jsonResp(status int, body []byte) *response {
	return &response{status: status, ctype: jsonType, body: body}
}

func jsonErr(status int, msg string) *response {
	return jsonResp(status, mustJSON(map[string]any{"message": msg}))
}

// mustJSON marshals without HTML escaping (matching JSON.stringify) and
// always appends the trailing newline sendJson uses.
func mustJSON(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return buf.Bytes() // Encoder.Encode appends '\n' already.
}

func marshalJSON(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	b := buf.Bytes()
	// Strip the trailing newline Encoder adds; callers add their own.
	return bytes.TrimRight(b, "\n")
}

func rawResp(status int, ctype string, body []byte) *response {
	return &response{status: status, ctype: ctype, body: body}
}
