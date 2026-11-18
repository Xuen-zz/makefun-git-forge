package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

func testHTTPConnection(t *testing.T, addr string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn, bufio.NewReader(conn)
}

func testHTTPResponse(t *testing.T, reader *bufio.Reader, status int) string {
	t.Helper()
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != status {
		t.Fatalf("response %d, expected %d, body %s", response.StatusCode, status, data)
	}
	return string(data)
}

func TestHTTPFramingAndCacheInvalidation(t *testing.T) {
	store := newStore("adaiversen", "test-token", "")
	repo := store.createRepo(mapJSON{"name": "cache"})
	issue := store.createIssue(repo, mapJSON{"title": "original"})
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	server := forgeHTTPServer{store: store}
	go server.Serve(listener)
	conn, reader := testHTTPConnection(t, listener.Addr().String())
	other, otherReader := testHTTPConnection(t, listener.Addr().String())
	get := "GET /api/v3/repos/adaiversen/cache/issues/1 HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer test-token\r\n\r\n"

	// Two requests in one TCP write must each produce a complete response.
	if _, err := io.WriteString(conn, get+get); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if !strings.Contains(testHTTPResponse(t, reader, 200), `"title":"original"`) {
			t.Fatal("incorrect original issue")
		}
	}
	patch := `{"title":"edited"}`
	fmt.Fprintf(other, "PATCH /api/v3/repos/adaiversen/cache/issues/1 HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer test-token\r\nContent-Length: %d\r\n\r\n%s", len(patch), patch)
	testHTTPResponse(t, otherReader, 200)
	io.WriteString(conn, get)
	if !strings.Contains(testHTTPResponse(t, reader, 200), `"title":"edited"`) {
		t.Fatal("connection cache survived an issue edit")
	}

	// Chunked GraphQL mutations must invalidate responses cached by another connection.
	query := fmt.Sprintf(`mutation { closeIssue(input: {issueId: %q}) { issue { state } } }`, issue.NodeID)
	body, err := json.Marshal(mapJSON{"query": query})
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(other, "POST /api/graphql HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer test-token\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n%s\r\n0\r\n\r\n", len(body), body)
	if data := testHTTPResponse(t, otherReader, 200); strings.Contains(data, `"errors"`) {
		t.Fatal(data)
	}
	io.WriteString(conn, get)
	if !strings.Contains(testHTTPResponse(t, reader, 200), `"state":"closed"`) {
		t.Fatal("connection cache survived a GraphQL mutation")
	}

	// A reused connection must still authenticate the current request.
	io.WriteString(conn, strings.ReplaceAll(get, "test-token", "wrong-token"))
	testHTTPResponse(t, reader, 401)

	// Expect/continue leaves the body unread until the interim response is sent.
	body = []byte(`{"title":"continued"}`)
	fmt.Fprintf(other, "POST /api/v3/repos/adaiversen/cache/issues HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer test-token\r\nContent-Length: %d\r\nExpect: 100-continue\r\n\r\n", len(body))
	testHTTPResponse(t, otherReader, 100)
	other.Write(body)
	testHTTPResponse(t, otherReader, 201)
}
