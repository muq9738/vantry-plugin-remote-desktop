package relayrtc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testServer(t *testing.T) (*Server, string) {
	t.Helper()
	service, err := New(Config{TurnURLs: []string{"turn:127.0.0.1:3478?transport=udp"}, secret: []byte("test-only-turn-rest-secret-not-a-real-credential")})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(service)
	t.Cleanup(func() { _ = service.Close(); server.Close() })
	return service, server.URL
}
func request(t *testing.T, method, url, token, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	reply, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer reply.Body.Close()
	raw, err := io.ReadAll(reply.Body)
	if err != nil {
		t.Fatal(err)
	}
	return reply.StatusCode, raw
}
func createSession(t *testing.T, url string) SessionInfo {
	t.Helper()
	code, raw := request(t, "POST", url+"/v1/sessions", "", `{"video":{"width":1920,"height":1080,"fps":60,"bitrate":8000000,"profileLevelId":"42e01f","displayId":1}}`)
	if code != 200 {
		t.Fatalf("create status %d", code)
	}
	var info SessionInfo
	if json.Unmarshal(raw, &info) != nil {
		t.Fatal("session JSON invalid")
	}
	return info
}

func TestSessionRolesCredentialsAndVideo(t *testing.T) {
	_, url := testServer(t)
	info := createSession(t, url)
	if info.HostToken == "" || info.ViewerToken == "" || info.HostToken == info.ViewerToken || info.ICETransportPolicy != "relay" || len(info.ICEServers) != 1 {
		t.Fatal("session contract missing role authentication or TURN")
	}
	if delta := time.Until(info.ExpiresAt); delta < 590*time.Second || delta > 610*time.Second {
		t.Fatal("expiresAt is not the TURN credential expiration")
	}
	var video map[string]any
	_ = json.Unmarshal(info.Video, &video)
	if video["width"] != float64(1920) || video["fps"] != float64(60) || video["displayId"] != float64(1) {
		t.Fatal("video metadata was not preserved")
	}
	endpoint := url + "/v1/sessions/" + info.SessionID
	if code, _ := request(t, "GET", endpoint, "", ""); code != 401 {
		t.Fatal("unauthenticated session read accepted")
	}
	var viewer SessionInfo
	code, raw := request(t, "GET", endpoint, info.ViewerToken, "")
	if code != 200 || json.Unmarshal(raw, &viewer) != nil {
		t.Fatal("viewer cannot refresh TURN")
	}
	if viewer.HostToken != "" || viewer.ViewerToken != "" || viewer.ICEServers[0].Username == info.ICEServers[0].Username {
		t.Fatal("GET leaked tokens or mixed TURN role credentials")
	}
	if code, _ = request(t, "GET", endpoint, info.HostToken, ""); code != 200 {
		t.Fatal("host cannot refresh TURN")
	}
	if code, _ = request(t, "DELETE", endpoint, info.ViewerToken, ""); code != 403 {
		t.Fatal("viewer deleted the session")
	}
	if code, _ = request(t, "DELETE", endpoint, info.HostToken, ""); code != 200 {
		t.Fatal("host could not delete its session")
	}
	if code, _ = request(t, "GET", endpoint, info.ViewerToken, ""); code != 404 {
		t.Fatal("deleted session still readable")
	}
}

type testWS struct {
	net.Conn
	reader *bufio.Reader
}

func dialWS(t *testing.T, url string, info SessionInfo, role string) *testWS {
	t.Helper()
	host := strings.TrimPrefix(url, "http://")
	conn, err := net.DialTimeout("tcp", host, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	token := info.HostToken
	if role == "viewer" {
		token = info.ViewerToken
	}
	_, err = fmt.Fprintf(conn, "GET /v1/sessions/%s/signal?role=%s HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nAuthorization: Bearer %s\r\n\r\n", info.SessionID, role, host, token)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, nil)
	if err != nil || response.StatusCode != 101 {
		conn.Close()
		t.Fatal("authenticated websocket upgrade failed")
	}
	if response.Header.Get("Sec-WebSocket-Accept") != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		conn.Close()
		t.Fatal("RFC6455 handshake invalid")
	}
	client := &testWS{conn, reader}
	t.Cleanup(func() { _ = client.Close() })
	return client
}
func (c *testWS) send(t *testing.T, value string) {
	t.Helper()
	mask := []byte{1, 3, 5, 7}
	payload := []byte(value)
	header := []byte{0x81}
	if len(payload) < 126 {
		header = append(header, 0x80|byte(len(payload)))
	} else {
		header = append(header, 0xfe, byte(len(payload)>>8), byte(len(payload)))
	}
	header = append(header, mask...)
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	if _, err := c.Write(append(header, payload...)); err != nil {
		t.Fatal(err)
	}
}
func (c *testWS) message(t *testing.T) map[string]any {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	var h [2]byte
	if _, err := io.ReadFull(c.reader, h[:]); err != nil {
		t.Fatal(err)
	}
	n := uint64(h[1] & 127)
	if n == 126 {
		var raw [2]byte
		if _, err := io.ReadFull(c.reader, raw[:]); err != nil {
			t.Fatal(err)
		}
		n = uint64(binary.BigEndian.Uint16(raw[:]))
	}
	if n == 127 {
		var raw [8]byte
		if _, err := io.ReadFull(c.reader, raw[:]); err != nil {
			t.Fatal(err)
		}
		n = binary.BigEndian.Uint64(raw[:])
	}
	if n > maxSignalBytes {
		t.Fatal("unbounded server frame")
	}
	payload := make([]byte, int(n))
	if _, err := io.ReadFull(c.reader, payload); err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if json.Unmarshal(payload, &value) != nil {
		t.Fatal("invalid signal JSON")
	}
	return value
}
func assertPeer(t *testing.T, c *testWS, connected bool) {
	t.Helper()
	value := c.message(t)
	if value["type"] != "peer" || value["connected"] != connected {
		t.Fatal("peer lifecycle event incorrect")
	}
}

func TestSignalForwardingRoleReplacementAndDeletion(t *testing.T) {
	_, url := testServer(t)
	info := createSession(t, url)
	host := dialWS(t, url, info, "host")
	viewer := dialWS(t, url, info, "viewer")
	assertPeer(t, host, true)
	assertPeer(t, viewer, true)
	host.send(t, `{"type":"offer","sdp":"test-offer"}`)
	if value := viewer.message(t); value["type"] != "offer" || value["sdp"] != "test-offer" {
		t.Fatal("offer was not forwarded")
	}
	viewer.send(t, `{"type":"answer","sdp":"test-answer"}`)
	if value := host.message(t); value["type"] != "answer" {
		t.Fatal("answer was not forwarded")
	}
	host.send(t, `{"type":"ice","candidate":{"candidate":"candidate:test","sdpMid":"0","sdpMLineIndex":0}}`)
	if value := viewer.message(t); value["type"] != "ice" {
		t.Fatal("ICE object was not forwarded")
	}
	viewer.send(t, `{"type":"ice","candidate":null}`)
	if value := host.message(t); value["type"] != "ice" || value["candidate"] != nil {
		t.Fatal("ICE end marker was not forwarded")
	}
	viewer.send(t, `{"type":"offer","sdp":"wrong-role"}`)
	if value := viewer.message(t); value["type"] != "error" {
		t.Fatal("viewer offered instead of host")
	}
	replacement := dialWS(t, url, info, "viewer")
	assertPeer(t, host, false)
	assertPeer(t, host, true)
	assertPeer(t, replacement, true)
	_ = viewer.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := viewer.reader.ReadByte(); err == nil {
		t.Fatal("replaced role connection stayed live")
	}
	request(t, "DELETE", url+"/v1/sessions/"+info.SessionID, info.HostToken, "")
	_ = host.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := host.reader.ReadByte(); err == nil {
		t.Fatal("deleted session retained host connection")
	}
}

func TestCredentialExpiryDoesNotDeleteActiveSession(t *testing.T) {
	service, url := testServer(t)
	info := createSession(t, url)
	host := dialWS(t, url, info, "host")
	service.mu.Lock()
	service.sessions[info.SessionID].touched = time.Now().Add(-2 * time.Hour)
	service.mu.Unlock()
	service.expire()
	if code, _ := request(t, "GET", url+"/v1/sessions/"+info.SessionID, info.ViewerToken, ""); code != 200 {
		t.Fatal("TURN expiration incorrectly removed an active host")
	}
	_ = host.Close()
	deadline := time.Now().Add(time.Second)
	for {
		service.mu.Lock()
		gone := service.sessions[info.SessionID].peers["host"] == nil
		service.mu.Unlock()
		if gone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("host disconnect not observed")
		}
		time.Sleep(time.Millisecond)
	}
	service.mu.Lock()
	service.sessions[info.SessionID].touched = time.Now().Add(-2 * time.Hour)
	service.mu.Unlock()
	service.expire()
	if code, _ := request(t, "GET", url+"/v1/sessions/"+info.SessionID, info.ViewerToken, ""); code != 404 {
		t.Fatal("idle session never expired")
	}
}

func TestPrivateUnixSocketAndStdioProxy(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "vantry-rtc-test-")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	path := filepath.Join(dir, "signal.sock")
	go func() {
		done <- Serve(ctx, Config{Socket: path, TurnURLs: []string{"turn:127.0.0.1:3478"}, secret: []byte("test-only-secret")})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(4 * time.Second):
			t.Error("Unix signaling server did not stop")
			return
		}
		_ = os.RemoveAll(dir)
	})
	deadline := time.Now().Add(2 * time.Second)
	for {
		if info, err := os.Stat(path); err == nil {
			if info.Mode().Perm() != 0600 {
				t.Fatal("socket permissions are not private")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("socket not created")
		}
		time.Sleep(time.Millisecond)
	}
	var response bytes.Buffer
	if err = Proxy(ctx, path, strings.NewReader("GET /v1/health HTTP/1.1\r\nHost: relay\r\nConnection: close\r\n\r\n"), &response); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(response.String(), `"state":"ready"`) {
		t.Fatal("proxy did not carry the HTTP response")
	}
}

func TestSessionCapacityAndMalformedVideo(t *testing.T) {
	service, url := testServer(t)
	service.config.MaxSessions = 1
	if code, _ := request(t, "POST", url+"/v1/sessions", "", `{"video":"invalid"}`); code != 400 {
		t.Fatal("invalid video accepted")
	}
	createSession(t, url)
	if code, _ := request(t, "POST", url+"/v1/sessions", "", `{}`); code != 503 {
		t.Fatal("session capacity not enforced")
	}
}
