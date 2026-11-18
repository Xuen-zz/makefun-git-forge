package main

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/valyala/fasthttp"
)

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

type forgeHTTPServer struct {
	store *store
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

// Each connection owns its request parser and output buffer. The same parser
// handles content lengths, chunked requests and keep-alive HTTP/1.0 clients.
func (s *forgeHTTPServer) serveConn(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReaderSize(conn, 4096)
	var request fasthttp.Request
	defer request.Reset()
	wire := make([]byte, 0, 4096)
	var cached [3]connectionReply
	nextUpload := 1
	var nextDeadlineUpdate time.Time
	for {
		request.Reset()
		request.KeepBodyBuffer = true
		// Renew active connections in batches. The deadline on a stalled
		// request is still at most 30 seconds, with up to 250 ms of rounding.
		now := time.Now()
		if !now.Before(nextDeadlineUpdate) {
			if err := conn.SetDeadline(now.Add(30 * time.Second)); err != nil {
				return
			}
			nextDeadlineUpdate = now.Add(250 * time.Millisecond)
		}
		// A cached bodyless GET may repeat byte for byte on a keep-alive
		// connection. The complete head includes authorization and all request
		// headers, and came from a prior successful parse on this connection.
		var bufferedGET []byte
		bufferedBefore := 0
		if _, peekErr := reader.Peek(1); peekErr == nil {
			slot := &cached[0]
			if n := len(slot.wireRequest); n != 0 && n <= reader.Buffered() {
				view, _ := reader.Peek(n)
				if bytes.Equal(view, slot.wireRequest) && slot.revision == s.store.revision.Load() {
					_, _ = reader.Discard(n)
					var sequence uint64
					if slot.path != "/mcp" && slot.path != "/api/graphql" {
						sequence = s.store.sequence.Add(1)
					}
					wire = appendHTTPReply(wire[:0], slot.reply, "GET", sequence, false)
					if _, err := conn.Write(wire); err != nil {
						return
					}
					continue
				}
			}
			bufferedBefore = reader.Buffered()
			view, _ := reader.Peek(bufferedBefore)
			if bytes.HasPrefix(view, []byte("GET ")) {
				if end := bytes.Index(view, []byte("\r\n\r\n")); end >= 0 {
					bufferedGET = view[:end+4]
				}
			}
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
		if len(bufferedGET) != bufferedBefore-reader.Buffered() ||
			(contentLength != 0 && contentLength != -2) || request.MayContinue() {
			bufferedGET = nil
		}
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
			protocol := string(headers.Peek("Git-Protocol"))
			credential := string(headers.Peek("Authorization"))
			encoding := string(headers.Peek("Content-Encoding"))
			reply = s.store.httpResponse(method, path, string(uri.Host()), query,
				service, protocol, credential, encoding, request.Body())
			// Cache authenticated reads for this connection. Mutating requests
			// invalidate the response before the next operation can reuse it.
			upload := method == "POST" && !strings.HasPrefix(path, "/api/") &&
				strings.HasSuffix(path, ".git/git-upload-pack")
			if (method == "GET" || upload) && reply.status == 200 &&
				len(request.Body()) <= 16384 && len(reply.body) <= 128<<10 {
				entry := connectionReply{
					target: string(headers.RequestURI()), host: string(headers.Host()),
					credential: credential, path: path, method: method, protocol: protocol, encoding: encoding,
					body: bytes.Clone(request.Body()), revision: revision, reply: reply,
				}
				if upload {
					cached[nextUpload] = entry
					nextUpload = 3 - nextUpload
				} else {
					if !headers.ConnectionClose() {
						entry.wireRequest = bytes.Clone(bufferedGET)
					}
					cached[0] = entry
				}
			}
		}
		var sequence uint64
		if path != "/mcp" && path != "/api/graphql" {
			sequence = s.store.sequence.Add(1)
		}
		wire = appendHTTPReply(wire[:0], reply, method, sequence, headers.ConnectionClose())
		if _, err := conn.Write(wire); err != nil || headers.ConnectionClose() {
			return
		}
		// Large transfers do not leave a large buffer attached to an idle socket.
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
