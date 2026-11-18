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
	"time"
)

type request struct {
	method    string
	target    string
	path      string
	rawQuery  string
	query     url.Values
	headers   map[string]string
	body      []byte
	proto11   bool
	keepAlive bool
}

func (r *request) header(k string) string { return r.headers[k] }

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
		mu.Lock()
		resp := dispatch(req)
		mu.Unlock()
		if !writeResponse(c, req, resp) {
			return
		}
		if !req.keepAlive {
			return
		}
	}
}

func readRequest(br *bufio.Reader, c net.Conn) (*request, error) {
	// Skip stray empty lines before the request line.
	var line string
	var err error
	for {
		line, err = br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line != "" {
			break
		}
	}
	parts := strings.SplitN(line, " ", 3)
	if len(parts) != 3 {
		return nil, fmt.Errorf("bad request line")
	}
	req := &request{
		method:  parts[0],
		target:  parts[1],
		headers: map[string]string{},
		proto11: parts[2] == "HTTP/1.1",
	}
	path, query, _ := strings.Cut(req.target, "?")
	req.path = path
	req.rawQuery = query
	// Headers.
	for {
		h, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		h = strings.TrimRight(h, "\r\n")
		if h == "" {
			break
		}
		k, v, ok := strings.Cut(h, ":")
		if !ok {
			continue
		}
		req.headers[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
	}
	req.keepAlive = req.proto11
	if conn := req.headers["connection"]; conn != "" {
		lc := strings.ToLower(conn)
		if strings.Contains(lc, "close") {
			req.keepAlive = false
		} else if strings.Contains(lc, "keep-alive") {
			req.keepAlive = true
		}
	}
	// 100-continue: answer the interim response so the client sends its body.
	if strings.EqualFold(req.headers["expect"], "100-continue") {
		if _, err := c.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n")); err != nil {
			return nil, err
		}
	}
	// Body.
	if te := req.headers["transfer-encoding"]; strings.Contains(strings.ToLower(te), "chunked") {
		body, err := readChunked(br)
		if err != nil {
			return nil, err
		}
		req.body = body
	} else if cl := req.headers["content-length"]; cl != "" {
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
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if i := strings.IndexByte(line, ';'); i >= 0 {
			line = line[:i]
		}
		n, err := strconv.ParseInt(strings.TrimSpace(line), 16, 64)
		if err != nil || n < 0 || n > 64<<20 {
			return nil, fmt.Errorf("bad chunk size")
		}
		if n == 0 {
			// Trailers until an empty line.
			for {
				t, err := br.ReadString('\n')
				if err != nil {
					return nil, err
				}
				if strings.TrimRight(t, "\r\n") == "" {
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

func writeResponse(c net.Conn, req *request, resp *response) bool {
	var hb bytes.Buffer
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
	mu.Lock()
	reqCounter++
	id := reqCounter
	mu.Unlock()
	hb.WriteString("X-GitHub-Request-Id: req-" + itoa(id) + "\r\n")
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
