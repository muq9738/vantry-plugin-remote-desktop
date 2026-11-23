package relayrtc

import (
	"net/http/httptest"
	"testing"
)

func TestSessionAdvertisesSSHTurnWithoutPublicRoute(t *testing.T) {
	service, err := New(Config{TurnURLs: []string{"turn:127.0.0.1:3479?transport=tcp"}, Transport: "ssh-tcp", TurnTCPPort: 3479, secret: []byte("test-only-turn-rest-secret")})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(service)
	t.Cleanup(func() { _ = service.Close(); server.Close() })
	info := createSession(t, server.URL)
	if info.Transport != "ssh-tcp" || info.TurnTCPPort != 3479 || len(info.ICEServers) != 1 || len(info.ICEServers[0].URLs) != 1 || info.ICEServers[0].URLs[0] != "turn:127.0.0.1:3479?transport=tcp" {
		t.Fatal("session does not give the client its private SSH TURN route")
	}
}
