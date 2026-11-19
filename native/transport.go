package main

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/valyala/fasthttp"
)

var readBufPool = sync.Pool{
	New: func() any {
		return make([]byte, 8192)
	},
}

type connectionReply struct {
	target, host, credential, path, method, protocol, encoding string
	body                                                       []byte
	wireRequest                                                []byte
	revision                                                   uint64
	reply                                                      httpReply
}

func (c *connectionReply) matches(request *fasthttp.Request, revision uint64) bool {
	header := &request.Header
	return c.reply.status != 0 && c.revision == revision &&
		string(header.Method()) == c.method && string(header.RequestURI()) == c.target &&
		string(header.Host()) == c.host && string(header.Peek("Authorization")) == c.credential &&
		string(header.Peek("Git-Protocol")) == c.protocol && string(header.Peek("Content-Encoding")) == c.encoding &&
		bytes.Equal(request.Body(), c.body)
}

func (c *connectionReply) matchesBytes(method, target, host, credential, protocol, encoding, body []byte, revision uint64) bool {
	return c.reply.status != 0 && c.revision == revision &&
		len(method) == len(c.method) && string(method) == c.method &&
		len(target) == len(c.target) && string(target) == c.target &&
		len(host) == len(c.host) && string(host) == c.host &&
		len(credential) == len(c.credential) && string(credential) == c.credential &&
		len(protocol) == len(c.protocol) && string(protocol) == c.protocol &&
		len(encoding) == len(c.encoding) && string(encoding) == c.encoding &&
		bytes.Equal(body, c.body)
}

type forgeHTTPServer struct {
	store *store
}

func (s *forgeHTTPServer) dispatchAndCache(method, target, host, path, query, service, protocol, credential, encoding string, body, rawWire []byte, closeConn bool, revision uint64, cached *[3]connectionReply, nextUpload *int) (string, httpReply) {
	reply := s.store.httpResponse(method, path, host, query, service, protocol, credential, encoding, body)
	upload := method == "POST" && !strings.HasPrefix(path, "/api/") &&
		strings.HasSuffix(path, ".git/git-upload-pack")
	if (method == "GET" || upload) && reply.status == 200 &&
		len(body) <= 16384 && len(reply.body) <= 128<<10 {
		entry := connectionReply{
			target: target, host: host,
			credential: credential, path: path, method: method, protocol: protocol, encoding: encoding,
			body: bytes.Clone(body), revision: revision, reply: reply,
		}
		if !closeConn && len(rawWire) != 0 && len(rawWire) <= 4096 {
			entry.wireRequest = bytes.Clone(rawWire)
		}
		if upload {
			cached[*nextUpload] = entry
			*nextUpload = 3 - *nextUpload
		} else {
			cached[0] = entry
		}
	}
	return path, reply
}

func (s *forgeHTTPServer) Serve(listener net.Listener) error {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		go s.serveConn(conn)
	}
}

func asciiEqualFoldBytes(a []byte, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca := a[i]
		cb := b[i]
		if ca == cb {
			continue
		}
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

func trimOWS(b []byte) []byte {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t') {
		b = b[1:]
	}
	for len(b) > 0 && (b[len(b)-1] == ' ' || b[len(b)-1] == '\t') {
		b = b[:len(b)-1]
	}
	return b
}

func parseContentLengthBytes(b []byte) (int, bool) {
	if len(b) == 0 {
		return 0, false
	}
	n := 0
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, false
		}
		if n > (maxBody-int(c-'0'))/10 {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}

func isFastMethod(m []byte) bool {
	switch string(m) {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD":
		return true
	}
	return false
}

func isFastTarget(t []byte) bool {
	if len(t) == 0 || t[0] != '/' {
		return false
	}
	for i := 0; i < len(t); i++ {
		c := t[i]
		if c <= ' ' || c >= 0x7f || c == '%' || c == '#' {
			return false
		}
		if c == '/' && i+1 < len(t) && (t[i+1] == '/' || t[i+1] == '.') {
			return false
		}
	}
	return true
}

type fastParsedHead struct {
	headerLen     int
	contentLength int
	hasCL         bool
	closeConn     bool
	method        []byte
	target        []byte
	host          []byte
	credential    []byte
	protocol      []byte
	encoding      []byte
}

// parseFastHead parses a standard HTTP/1.1 request header block.
// Returns (head, needMore, ok): if !needMore && !ok, the caller falls back to fasthttp.
func parseFastHead(buf []byte) (fastParsedHead, bool, bool) {
	var h fastParsedHead
	lineEnd := bytes.Index(buf, []byte("\r\n"))
	if lineEnd < 0 {
		return h, true, false
	}
	reqLine := buf[:lineEnd]
	sp1 := bytes.IndexByte(reqLine, ' ')
	if sp1 <= 0 {
		return h, false, false
	}
	h.method = reqLine[:sp1]
	if !isFastMethod(h.method) {
		return h, false, false
	}
	rest := reqLine[sp1+1:]
	sp2 := bytes.IndexByte(rest, ' ')
	if sp2 <= 0 {
		return h, false, false
	}
	h.target = rest[:sp2]
	if !isFastTarget(h.target) {
		return h, false, false
	}
	if !bytes.Equal(rest[sp2+1:], []byte("HTTP/1.1")) {
		return h, false, false
	}
	pos := lineEnd + 2
	hasHost := false
	for {
		if pos+2 <= len(buf) && buf[pos] == '\r' && buf[pos+1] == '\n' {
			if !hasHost {
				return h, false, false
			}
			h.headerLen = pos + 2
			return h, false, true
		}
		rel := bytes.Index(buf[pos:], []byte("\r\n"))
		if rel < 0 {
			return h, true, false
		}
		line := buf[pos : pos+rel]
		colon := bytes.IndexByte(line, ':')
		if colon <= 0 {
			return h, false, false
		}
		name := line[:colon]
		for _, c := range name {
			if c <= ' ' || c >= 0x7f {
				return h, false, false
			}
		}
		val := trimOWS(line[colon+1:])
		switch name[0] {
		case 'H', 'h':
			if asciiEqualFoldBytes(name, "Host") {
				if hasHost {
					return h, false, false
				}
				hasHost = true
				h.host = val
			}
		case 'A', 'a':
			if asciiEqualFoldBytes(name, "Authorization") {
				h.credential = val
			}
		case 'C', 'c':
			if asciiEqualFoldBytes(name, "Content-Length") {
				if h.hasCL {
					return h, false, false
				}
				n, ok := parseContentLengthBytes(val)
				if !ok {
					return h, false, false
				}
				h.hasCL = true
				h.contentLength = n
			} else if asciiEqualFoldBytes(name, "Content-Encoding") {
				if len(h.encoding) != 0 {
					return h, false, false
				}
				h.encoding = val
			} else if asciiEqualFoldBytes(name, "Connection") {
				if asciiEqualFoldBytes(val, "close") {
					h.closeConn = true
				} else if !asciiEqualFoldBytes(val, "keep-alive") {
					return h, false, false
				}
			}
		case 'G', 'g':
			if asciiEqualFoldBytes(name, "Git-Protocol") {
				h.protocol = val
			}
		case 'T', 't':
			if asciiEqualFoldBytes(name, "Transfer-Encoding") {
				return h, false, false
			}
		case 'E', 'e':
			if asciiEqualFoldBytes(name, "Expect") {
				return h, false, false
			}
		}
		pos += rel + 2
	}
}

func (s *forgeHTTPServer) serveConn(conn net.Conn) {
	defer conn.Close()
	buf := readBufPool.Get().([]byte)
	defer readBufPool.Put(buf)
	off, have := 0, 0
	wire := make([]byte, 0, 4096)
	var cached [3]connectionReply
	nextUpload := 1
	var nextDeadlineUpdate time.Time

	for {
		if off == have {
			off, have = 0, 0
		}
		if have-off == 0 {
			now := time.Now()
			if !now.Before(nextDeadlineUpdate) {
				if err := conn.SetDeadline(now.Add(30 * time.Second)); err != nil {
					return
				}
				nextDeadlineUpdate = now.Add(250 * time.Millisecond)
			}
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			have = n
		}

		// Fast path: match complete wire bytes of cached GET or small POST requests.
		avail := have - off
		rev := s.store.revision.Load()
		matchedWire := false
		for i := range cached {
			slot := &cached[i]
			if nw := len(slot.wireRequest); nw != 0 && nw <= avail && slot.revision == rev {
				if bytes.Equal(buf[off:off+nw], slot.wireRequest) {
					off += nw
					var sequence uint64
					if slot.path != "/mcp" && slot.path != "/api/graphql" {
						sequence = s.store.sequence.Add(1)
					}
					wire = appendHTTPReply(wire[:0], slot.reply, slot.method, sequence, false)
					if _, err := conn.Write(wire); err != nil {
						return
					}
					matchedWire = true
					break
				}
			}
		}
		if matchedWire {
			continue
		}

		// Parse HTTP/1.1 head from buffer, reading more if headers span reads.
		var head fastParsedHead
		for {
			var needMore, ok bool
			head, needMore, ok = parseFastHead(buf[off:have])
			if ok {
				break
			}
			if !needMore {
				s.serveConnFallback(conn, buf[off:have], wire, &cached, nextUpload, nextDeadlineUpdate)
				return
			}
			if off > 0 {
				copy(buf, buf[off:have])
				have -= off
				off = 0
			}
			if have == len(buf) {
				s.serveConnFallback(conn, buf[:have], wire, &cached, nextUpload, nextDeadlineUpdate)
				return
			}
			now := time.Now()
			if !now.Before(nextDeadlineUpdate) {
				if err := conn.SetDeadline(now.Add(30 * time.Second)); err != nil {
					return
				}
				nextDeadlineUpdate = now.Add(250 * time.Millisecond)
			}
			n, err := conn.Read(buf[have:])
			if err != nil {
				return
			}
			have += n
		}

		totalLen := head.headerLen + head.contentLength
		var body []byte
		var rawWire []byte
		if head.contentLength == 0 {
			rawWire = buf[off : off+head.headerLen]
			off += head.headerLen
		} else if totalLen <= len(buf) {
			if off+totalLen > have {
				if off > 0 {
					copy(buf, buf[off:have])
					have -= off
					off = 0
				}
				for have < totalLen {
					now := time.Now()
					if !now.Before(nextDeadlineUpdate) {
						if err := conn.SetDeadline(now.Add(30 * time.Second)); err != nil {
							return
						}
						nextDeadlineUpdate = now.Add(250 * time.Millisecond)
					}
					n, err := conn.Read(buf[have:])
					if err != nil {
						return
					}
					have += n
				}
			}
			body = buf[off+head.headerLen : off+totalLen]
			rawWire = buf[off : off+totalLen]
			off += totalLen
		} else {
			body = make([]byte, head.contentLength)
			already := have - (off + head.headerLen)
			copy(body, buf[off+head.headerLen:have])
			off, have = 0, 0
			if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
				return
			}
			nextDeadlineUpdate = time.Now().Add(250 * time.Millisecond)
			if _, err := io.ReadFull(conn, body[already:]); err != nil {
				return
			}
		}

		var path string
		var reply httpReply
		for i := range cached {
			if cached[i].matchesBytes(head.method, head.target, head.host, head.credential, head.protocol, head.encoding, body, rev) {
				path, reply = cached[i].path, cached[i].reply
				break
			}
		}
		methodStr := string(head.method)
		if reply.status == 0 {
			targetBytes := head.target
			var pathBytes, queryBytes []byte
			if qm := bytes.IndexByte(targetBytes, '?'); qm >= 0 {
				pathBytes = targetBytes[:qm]
				queryBytes = targetBytes[qm+1:]
			} else {
				pathBytes = targetBytes
			}
			path = string(pathBytes)
			query := string(queryBytes)
			service := ""
			if strings.HasSuffix(path, ".git/info/refs") && query != "" {
				if svc, ok := strings.CutPrefix(query, "service="); ok && !strings.ContainsAny(svc, "&%") {
					service = svc
				} else {
					var args fasthttp.Args
					args.ParseBytes(queryBytes)
					service = string(args.Peek("service"))
				}
			}
			path, reply = s.dispatchAndCache(
				methodStr, string(head.target), string(head.host), path, query,
				service, string(head.protocol), string(head.credential), string(head.encoding),
				body, rawWire, head.closeConn, rev, &cached, &nextUpload,
			)
		}

		var sequence uint64
		if path != "/mcp" && path != "/api/graphql" {
			sequence = s.store.sequence.Add(1)
		}
		wire = appendHTTPReply(wire[:0], reply, methodStr, sequence, head.closeConn)
		if _, err := conn.Write(wire); err != nil || head.closeConn {
			return
		}
		if cap(wire) > 256<<10 {
			wire = make([]byte, 0, 4096)
		}
	}
}

// serveConnFallback handles chunked requests, Expect: 100-continue, HTTP/1.0,
// percent-encoded paths, and malformed inputs using fasthttp's full parser.
func (s *forgeHTTPServer) serveConnFallback(conn net.Conn, pending, wire []byte, cached *[3]connectionReply, nextUpload int, nextDeadlineUpdate time.Time) {
	var stream io.Reader = conn
	if len(pending) > 0 {
		stream = io.MultiReader(bytes.NewReader(bytes.Clone(pending)), conn)
	}
	reader := bufio.NewReaderSize(stream, 4096)
	var request fasthttp.Request
	defer request.Reset()
	for {
		request.Reset()
		request.KeepBodyBuffer = true
		now := time.Now()
		if !now.Before(nextDeadlineUpdate) {
			if err := conn.SetDeadline(now.Add(30 * time.Second)); err != nil {
				return
			}
			nextDeadlineUpdate = now.Add(250 * time.Millisecond)
		}
		if err := request.Header.Read(reader); err != nil {
			var networkErr net.Error
			if !errors.Is(err, io.EOF) && !(errors.As(err, &networkErr) && networkErr.Timeout()) {
				status := 400
				if err == fasthttp.ErrBodyTooLarge {
					status = 413
				}
				wire = appendHTTPReply(wire[:0], httpReply{status: status, typ: contentType, body: missing}, "", 0, true)
				conn.Write(wire)
			}
			return
		}
		contentLength := request.Header.ContentLength()
		if request.MayContinue() {
			if request.Header.ContentLength() > maxBody {
				wire = appendHTTPReply(wire[:0], httpReply{status: 413, typ: contentType, body: missing}, "", 0, true)
				conn.Write(wire)
				return
			}
			if _, err := conn.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n")); err != nil {
				return
			}
		}
		var bodyErr error
		if contentLength != 0 && contentLength != -2 {
			bodyErr = request.ContinueReadBody(reader, maxBody, false)
		}
		if bodyErr != nil {
			status := 400
			if bodyErr == fasthttp.ErrBodyTooLarge {
				status = 413
			}
			wire = appendHTTPReply(wire[:0], httpReply{status: status, typ: contentType, body: missing}, "", 0, true)
			conn.Write(wire)
			return
		}
		headers := &request.Header
		method := string(headers.Method())
		revision := s.store.revision.Load()
		var path string
		var reply httpReply
		for i := range cached {
			if cached[i].matches(&request, revision) {
				path, reply = cached[i].path, cached[i].reply
				break
			}
		}
		if reply.status == 0 {
			uri := request.URI()
			path = string(uri.Path())
			query := string(uri.QueryString())
			service := ""
			if strings.HasSuffix(path, ".git/info/refs") {
				service = string(uri.QueryArgs().Peek("service"))
			}
			path, reply = s.dispatchAndCache(
				method, string(headers.RequestURI()), string(headers.Host()), path, query,
				service, string(headers.Peek("Git-Protocol")), string(headers.Peek("Authorization")),
				string(headers.Peek("Content-Encoding")), request.Body(), nil,
				headers.ConnectionClose(), revision, cached, &nextUpload,
			)
		}
		var sequence uint64
		if path != "/mcp" && path != "/api/graphql" {
			sequence = s.store.sequence.Add(1)
		}
		wire = appendHTTPReply(wire[:0], reply, method, sequence, headers.ConnectionClose())
		if _, err := conn.Write(wire); err != nil || headers.ConnectionClose() {
			return
		}
		if cap(wire) > 256<<10 {
			wire = make([]byte, 0, 4096)
		}
	}
}

func appendHTTPReply(wire []byte, reply httpReply, method string, sequence uint64, close bool) []byte {
	wire = append(wire, "HTTP/1.1 "...)
	wire = strconv.AppendInt(wire, int64(reply.status), 10)
	wire = append(wire, ' ')
	wire = append(wire, fasthttp.StatusMessage(reply.status)...)
	wire = append(wire, "\r\nContent-Type: "...)
	wire = append(wire, reply.typ...)
	if reply.status != 204 && reply.status != 304 {
		wire = append(wire, "\r\nContent-Length: "...)
		wire = strconv.AppendInt(wire, int64(len(reply.body)), 10)
	}
	if sequence != 0 {
		wire = append(wire, "\r\nX-GitHub-Request-Id: "...)
		wire = strconv.AppendUint(wire, sequence, 16)
	}
	if reply.challenge {
		wire = append(wire, "\r\nWWW-Authenticate: Basic realm=\"Git forge\""...)
	}
	if strings.HasPrefix(reply.typ, "application/x-git-") {
		wire = append(wire, "\r\nCache-Control: no-cache"...)
	}
	if reply.link != "" && !strings.ContainsAny(reply.link, "\r\n") {
		wire = append(wire, "\r\nLink: "...)
		wire = append(wire, reply.link...)
	}
	if reply.scopes != "" && !strings.ContainsAny(reply.scopes, "\r\n") {
		wire = append(wire, "\r\nX-OAuth-Scopes: "...)
		wire = append(wire, reply.scopes...)
	}
	if reply.allow != "" && !strings.ContainsAny(reply.allow, "\r\n") {
		wire = append(wire, "\r\nAllow: "...)
		wire = append(wire, reply.allow...)
	}
	if close {
		wire = append(wire, "\r\nConnection: close"...)
	}
	wire = append(wire, "\r\n\r\n"...)
	if method != "HEAD" && reply.status != 204 && reply.status != 304 {
		wire = append(wire, reply.body...)
	}
	return wire
}
