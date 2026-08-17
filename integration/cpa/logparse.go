// Package logparse parses CLIProxyAPI request-log files (the === SECTION ===
// plain-text format) into structured form, for use as test fixtures.
//
// A log file records one client request round trip:
//
//	=== REQUEST INFO ===   metadata about the client request
//	=== HEADERS ===        headers sent by the client
//	=== REQUEST BODY ===   the client's original request body (single-line JSON)
//	=== API REQUEST N ===  attempt N forwarded upstream (URL, auth, headers, body)
//	=== API RESPONSE N === the upstream response for attempt N (raw SSE or JSON)
//	=== RESPONSE ===       what the proxy returned to the client
package cpa

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// Log is one parsed request-log file.
type Log struct {
	Info         map[string]string // REQUEST INFO key/value pairs
	Headers      http.Header       // client -> proxy headers
	RequestBody  []byte            // client's original request body
	APIRequests  []APIRequest      // upstream attempts, in order
	APIResponses []APIResponse     // upstream responses, in order
	Response     Response          // what the proxy returned to the client
}

// APIRequest is one forwarded upstream request attempt.
type APIRequest struct {
	N           int // attempt number, 1-based
	Timestamp   string
	UpstreamURL string
	Method      string
	Auth        string
	Headers     http.Header
	Body        []byte
}

// APIResponse is one upstream response.
type APIResponse struct {
	N       int // attempt number, 1-based
	Status  int
	Headers http.Header
	Body    []byte
}

// Response is the client-facing response.
type Response struct {
	Status  int
	Headers http.Header
	Body    []byte
}

// IsChatCompletions reports whether the first upstream attempt targeted an
// OpenAI chat-completions endpoint (as opposed to e.g. Claude passthrough).
func (l *Log) IsChatCompletions() bool {
	if len(l.APIRequests) == 0 {
		return false
	}
	return strings.Contains(l.APIRequests[0].UpstreamURL, "chat/completions")
}

// Parse reads a request-log file.
func Parse(r io.Reader) (*Log, error) {
	lg := &Log{Info: map[string]string{}}

	var (
		cur   string // current section name, e.g. "API REQUEST 1"
		curN  int    // attempt number for API REQUEST/RESPONSE sections
		lines []string
	)
	flush := func() error {
		if cur == "" {
			return nil
		}
		if err := lg.parseSection(cur, curN, lines); err != nil {
			return fmt.Errorf("section %s: %w", cur, err)
		}
		cur, curN, lines = "", 0, nil
		return nil
	}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1024*1024), 64*1024*1024) // bodies can be large single lines
	for sc.Scan() {
		line := sc.Text()
		if name, n, ok := sectionHeader(line); ok {
			if err := flush(); err != nil {
				return nil, err
			}
			cur, curN = name, n
			continue
		}
		// CLIProxyAPI sometimes glues the next section marker onto the last
		// body line without a newline. Split such lines here.
		if m := trailingSectionHeader(line); m != "" {
			name, n, _ := sectionHeader(m)
			if err := flush(); err != nil {
				return nil, err
			}
			body := strings.TrimRight(line[:len(line)-len(m)], "\n")
			if cur != "" && body != "" {
				lines = append(lines, body)
			}
			cur, curN = name, n
			continue
		}
		if cur != "" {
			lines = append(lines, line)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return lg, nil
}

// sectionHeader recognizes "=== NAME ===" and "=== API REQUEST 3 ===" style
// headers, returning the section name and attempt number.
func sectionHeader(line string) (string, int, bool) {
	if !strings.HasPrefix(line, "=== ") || !strings.HasSuffix(line, " ===") {
		return "", 0, false
	}
	name := strings.TrimSpace(line[4 : len(line)-4])
	if name == "" {
		return "", 0, false
	}
	for _, prefix := range []string{"API REQUEST ", "API RESPONSE "} {
		if strings.HasPrefix(name, prefix) {
			n, err := strconv.Atoi(strings.TrimPrefix(name, prefix))
			if err != nil {
				return "", 0, false
			}
			return strings.TrimSuffix(prefix, " "), n, true
		}
	}
	return name, 0, true
}

func (l *Log) parseSection(name string, n int, lines []string) error {
	switch name {
	case "REQUEST INFO":
		l.Info = parseKeyValues(lines)
	case "HEADERS":
		l.Headers = parseHeaderBlock(lines)
	case "REQUEST BODY":
		l.RequestBody = joinBody(lines)
	case "API REQUEST":
		req, err := parseAPIRequest(n, lines)
		if err != nil {
			return err
		}
		l.APIRequests = append(l.APIRequests, req)
	case "API RESPONSE":
		resp, err := parseAPIResponse(n, lines)
		if err != nil {
			return err
		}
		l.APIResponses = append(l.APIResponses, resp)
	case "RESPONSE":
		resp, err := parseResponse(lines)
		if err != nil {
			return err
		}
		l.Response = resp
	default:
		// Unknown section (future format additions) — ignore.
	}
	return nil
}

// parseKeyValues parses "Key: value" lines into a map.
func parseKeyValues(lines []string) map[string]string {
	m := map[string]string{}
	for _, ln := range lines {
		k, v, ok := splitKV(ln)
		if !ok {
			continue
		}
		m[k] = v
	}
	return m
}

// splitKV splits "Key: value" on the first colon.
func splitKV(line string) (string, string, bool) {
	i := strings.Index(line, ":")
	if i < 0 {
		return "", "", false
	}
	return strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+1:]), true
}

// parseHeaderBlock parses consecutive "Key: value" lines into an http.Header,
// preserving repeated headers (Set-Cookie, Vary, ...) via Add.
func parseHeaderBlock(lines []string) http.Header {
	h := http.Header{}
	for _, ln := range lines {
		k, v, ok := splitKV(ln)
		if !ok || k == "" {
			continue
		}
		h.Add(k, v)
	}
	return h
}

// joinBody joins body lines (multi-line for SSE) with newlines, trimming
// leading/trailing blank lines. It also strips a trailing section-header
// fragment: CLIProxyAPI sometimes writes the next "=== NAME ===" marker on the
// same line as the last body line without a newline separator. Bodies may
// legitimately contain "===" (source code, diff markers), so only a trailing
// header-shaped fragment is stripped.
func joinBody(lines []string) []byte {
	if n := len(lines); n > 0 {
		lines[n-1] = stripTrailingSectionHeader(lines[n-1])
	}
	s := strings.Join(lines, "\n")
	return []byte(strings.Trim(s, "\n"))
}

// sectionHeaderRe matches CLIProxyAPI section markers like "=== RESPONSE ==="
// or "=== API REQUEST 2 ===" glued to the end of a line.
var sectionHeaderRe = regexp.MustCompile(`=== (?:REQUEST INFO|HEADERS|REQUEST BODY|API REQUEST|API RESPONSE|RESPONSE)(?: [0-9]+)? ===$`)

// stripTrailingSectionHeader removes a section header glued to the end of a
// body line (no newline before it).
func stripTrailingSectionHeader(line string) string {
	if m := trailingSectionHeader(line); m != "" {
		return strings.TrimRight(line[:len(line)-len(m)], "\n")
	}
	return line
}

// trailingSectionHeader returns the trailing glued section marker on a line,
// or "" when the line doesn't end with one.
func trailingSectionHeader(line string) string {
	for _, m := range sectionHeaderRe.FindAllStringSubmatchIndex(line, -1) {
		if m[1] == len(line) && m[0] > 0 {
			return line[m[0]:]
		}
	}
	return ""
}

// parseAPIRequest parses an "=== API REQUEST N ===" section.
// Layout: field lines, blank, "Headers:" block, blank, "Body:", body lines.
func parseAPIRequest(n int, lines []string) (APIRequest, error) {
	req := APIRequest{N: n, Headers: http.Header{}}
	i := 0
	// Leading "Key: value" fields until blank line or Headers:/Body: marker.
	for ; i < len(lines); i++ {
		ln := lines[i]
		if ln == "" {
			continue // tolerate blank lines between fields
		}
		if ln == "Headers:" || ln == "Body:" {
			break
		}
		k, v, ok := splitKV(ln)
		if !ok {
			continue
		}
		switch k {
		case "Timestamp":
			req.Timestamp = v
		case "Upstream URL":
			req.UpstreamURL = v
		case "HTTP Method":
			req.Method = v
		case "Auth":
			req.Auth = v
		}
	}
	req.Headers, req.Body, _ = parseHeadersAndBody(lines[i:])
	return req, nil
}

// parseAPIResponse parses an "=== API RESPONSE N ===" section.
// Layout: Timestamp, blank, "Status: NNN", "Headers:" block, blank, "Body:", body.
func parseAPIResponse(n int, lines []string) (APIResponse, error) {
	resp := APIResponse{N: n, Headers: http.Header{}}
	i := 0
	for ; i < len(lines); i++ {
		if k, v, ok := splitKV(lines[i]); ok && k == "Status" {
			status, err := strconv.Atoi(v)
			if err != nil {
				return resp, fmt.Errorf("parse status %q: %w", v, err)
			}
			resp.Status = status
			i++
			break
		}
	}
	rest, body, hasBody := parseHeadersAndBody(lines[i:])
	resp.Headers = rest
	if !hasBody {
		resp.Body = nil
	} else {
		resp.Body = body
	}
	return resp, nil
}

// parseResponse parses the "=== RESPONSE ===" section.
// Unlike API RESPONSE, headers follow "Status:" directly without a
// "Headers:" marker, until the first blank line; the body follows.
func parseResponse(lines []string) (Response, error) {
	resp := Response{Headers: http.Header{}}
	i := 0
	for ; i < len(lines); i++ {
		if k, v, ok := splitKV(lines[i]); ok && k == "Status" {
			status, err := strconv.Atoi(v)
			if err != nil {
				return resp, fmt.Errorf("parse status %q: %w", v, err)
			}
			resp.Status = status
			i++
			break
		}
	}
	// Header lines until blank line.
	for ; i < len(lines) && lines[i] != ""; i++ {
		if lines[i] == "Body:" {
			break
		}
		if k, v, ok := splitKV(lines[i]); ok {
			resp.Headers.Add(k, v)
		}
	}
	// Skip blank separator; optional "Body:" marker.
	for ; i < len(lines); i++ {
		if lines[i] == "Body:" {
			i++
			break
		}
		if lines[i] != "" {
			break // body starts directly
		}
	}
	resp.Body = joinBody(lines[i:])
	return resp, nil
}

// parseHeadersAndBody handles the shared tail layout: an optional "Headers:"
// marker followed by header lines, then an optional "Body:" marker followed by
// the body. Returns headers, body, and whether a body was present at all.
func parseHeadersAndBody(lines []string) (http.Header, []byte, bool) {
	h := http.Header{}
	i := 0
	if i < len(lines) && lines[i] == "Headers:" {
		i++
		for ; i < len(lines) && lines[i] != ""; i++ {
			if k, v, ok := splitKV(lines[i]); ok && k != "Body" {
				h.Add(k, v)
			}
		}
	}
	// Skip blank separators up to "Body:".
	for ; i < len(lines); i++ {
		if lines[i] == "Body:" {
			i++
			return h, joinBody(lines[i:]), true
		}
		if lines[i] != "" {
			break // content without a Body: marker
		}
	}
	if i < len(lines) {
		return h, joinBody(lines[i:]), true
	}
	return h, nil, false
}
