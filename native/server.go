package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/valyala/fasthttp"
)

var missing = []byte("{\"message\":\"Not Found\"}")
var healthy = httpReply{status: 200, typ: contentType, body: []byte("{\"ok\":true}")}

func (s *store) httpResponse(method, path, origin, rawQuery, service, protocol, credential, encoding string, body []byte) (reply httpReply) {
	defer func() {
		if cause := recover(); cause != nil {
			if e, ok := cause.(apiError); ok {
				reply = jsonReply(e.status, mapJSON{"message": e.message})
				return
			}
			log.Printf("forge request failed: %v", cause)
			reply = jsonReply(500, mapJSON{"message": "Internal Server Error"})
		}
	}()
	if method == "GET" && path == "/healthz" {
		return healthy
	}
	if !strings.HasPrefix(path, "/api/") && path != "/mcp" {
		owner, rest, _ := strings.Cut(strings.TrimPrefix(path, "/"), "/")
		if name, route, ok := strings.Cut(rest, ".git/"); ok && name != "" && !strings.Contains(name, "/") {
			if route == "info/refs" || route == receiveService || route == uploadService {
				return s.gitHTTP(method, owner, name, route, service, protocol, credential, encoding, body)
			}
		}
	}
	isRest := path == "/api/v3" || strings.HasPrefix(path, "/api/v3/")
	if !isRest && path != "/api/graphql" && path != "/mcp" {
		reject(404, "Not Found")
	}
	if !s.authorized(credential, false) {
		reject(401, "Bad credentials")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if isRest {
		return s.rest(method, path, origin, rawQuery, body)
	}
	if path == "/api/graphql" {
		return s.serveGraphQL(method, body)
	}
	return s.mcp(method, origin, body)
}

func (s *store) handler(ctx *fasthttp.RequestCtx) {
	path := string(ctx.Path())
	uri := ctx.URI()
	query := ""
	service := ""
	if raw := uri.QueryString(); len(raw) > 0 {
		query = string(raw)
		if strings.HasSuffix(path, ".git/info/refs") {
			service = string(ctx.QueryArgs().Peek("service"))
		}
	}
	origin := "http://" + string(ctx.Host())
	headers := &ctx.Request.Header
	reply := s.httpResponse(string(ctx.Method()), path, origin, query, service, string(headers.Peek("Git-Protocol")),
		string(headers.Peek("Authorization")), string(headers.Peek("Content-Encoding")), ctx.PostBody())
	if path != "/mcp" && path != "/api/graphql" {
		ctx.Response.Header.Set("X-GitHub-Request-Id", strconv.FormatUint(s.sequence.Add(1), 16))
	}
	ctx.Response.Header.SetContentType(reply.typ)
	if reply.challenge {
		ctx.Response.Header.Set("WWW-Authenticate", "Basic realm=\"Git forge\"")
	}
	if strings.HasPrefix(reply.typ, "application/x-git-") {
		ctx.Response.Header.Set("Cache-Control", "no-cache")
	}
	if reply.link != "" {
		ctx.Response.Header.Set("Link", reply.link)
	}
	if reply.scopes != "" {
		ctx.Response.Header.Set("X-OAuth-Scopes", reply.scopes)
	}
	if reply.allow != "" {
		ctx.Response.Header.Set("Allow", reply.allow)
	}
	ctx.SetStatusCode(reply.status)
	ctx.Response.SetBodyRaw(reply.body)
}

func environmentCall(client *fasthttp.Client, method, endpoint string, body []byte, timeout time.Duration) (int, []byte, error) {
	request, response := fasthttp.AcquireRequest(), fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(request)
	defer fasthttp.ReleaseResponse(response)
	request.Header.SetMethod(method)
	request.SetRequestURI(endpoint)
	if body != nil {
		request.Header.SetContentType("application/json")
		request.SetBodyRaw(body)
	}
	if err := client.DoTimeout(request, response, timeout); err != nil {
		return 0, nil, err
	}
	return response.StatusCode(), append([]byte(nil), response.Body()...), nil
}
func awaitTrial(origin string, port int, serveErr <-chan error) error {
	client := &fasthttp.Client{MaxResponseBodySize: 64 << 10}
	command := encoded(mapJSON{"port": port})
	for delay := 50 * time.Millisecond; ; delay = min(delay*2, 2*time.Second) {
		code, _, err := environmentCall(client, "POST", origin+"/register", command, 5*time.Second)
		if err == nil && ((code >= 200 && code < 300) || code == 409) {
			break
		}
		if code == 400 || code == 405 {
			return fmt.Errorf("registration returned %d", code)
		}
		select {
		case err = <-serveErr:
			return err
		case <-time.After(delay):
		}
	}
	for {
		code, data, err := environmentCall(client, "GET", origin+"/done", nil, 25*time.Second)
		if err == nil && code == 200 {
			var status struct {
				Done bool `json:"done"`
			}
			if json.Unmarshal(data, &status) == nil && status.Done {
				return nil
			}
		}
		select {
		case err = <-serveErr:
			return err
		case <-time.After(400 * time.Millisecond):
		}
	}
}
func run() error {
	env := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	login := os.Getenv("FORGE_LOGIN")
	if login == "" {
		login = "adaiversen"
	}
	token, ok := os.LookupEnv("MAKEFUN_SHOP_TOKEN")
	if !ok {
		token = os.Getenv("FORGE_TOKEN")
	}
	addr := "127.0.0.1:0"
	if env != "" {
		destination, err := url.Parse(env)
		if err != nil || destination.Host == "" || (destination.Scheme != "http" && destination.Scheme != "https") {
			return errors.New("invalid environment URL")
		}
		addr = "0.0.0.0:0"
	}
	s := newStore(login, token, os.Getenv("MAKEFUN_TRIAL_NOW"))
	listener, err := net.Listen("tcp4", addr)
	if err != nil {
		return err
	}
	defer listener.Close()
	server := &fasthttp.Server{Handler: s.handler, MaxRequestBodySize: maxBody, ReadTimeout: 30 * time.Second, IdleTimeout: 20 * time.Second, NoDefaultServerHeader: true}
	port := listener.Addr().(*net.TCPAddr).Port
	fmt.Println(port)
	if env == "" {
		return server.Serve(listener)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	return awaitTrial(env, port, serveErr)
}
func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
