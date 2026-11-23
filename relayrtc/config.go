// Package relayrtc serves private SSH/Unix-socket WebRTC signaling and
// standard short-lived TURN REST credentials. It never handles video frames.
package relayrtc

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

type Config struct {
	Socket          string   `json:"socket"`
	TurnURLs        []string `json:"turnUrls"`
	TurnSecretFile  string   `json:"turnSecretFile"`
	CredentialSecs  int      `json:"credentialSeconds"`
	SessionIdleSecs int      `json:"sessionIdleSeconds"`
	MaxSessions     int      `json:"maxSessions"`
	Transport       string   `json:"transport,omitempty"`
	TurnTCPPort     int      `json:"turnTcpPort,omitempty"`
	secret          []byte
}

func (c Config) normalized() (Config, error) {
	if c.Socket == "" {
		c.Socket = "/run/vantry-rtc/signaling.sock"
	}
	if c.CredentialSecs == 0 {
		c.CredentialSecs = 600
	}
	if c.SessionIdleSecs == 0 {
		c.SessionIdleSecs = 3600
	}
	if c.MaxSessions == 0 {
		c.MaxSessions = 8
	}
	if c.CredentialSecs < 60 || c.CredentialSecs > 86400 || c.SessionIdleSecs < 60 || c.MaxSessions < 1 || c.MaxSessions > 256 {
		return c, errors.New("invalid signaling limits")
	}
	if c.Transport != "" && c.Transport != "ssh-tcp" {
		return c, errors.New("invalid TURN transport")
	}
	if c.Transport == "ssh-tcp" && (c.TurnTCPPort < 1025 || c.TurnTCPPort > 65535) {
		return c, errors.New("invalid SSH TURN port")
	}
	if len(c.TurnURLs) == 0 {
		return c, errors.New("TURN URLs are required")
	}
	for _, url := range c.TurnURLs {
		if !strings.HasPrefix(url, "turn:") && !strings.HasPrefix(url, "turns:") {
			return c, errors.New("invalid TURN URL")
		}
	}
	if len(c.secret) == 0 {
		info, err := os.Stat(c.TurnSecretFile)
		if err != nil {
			return c, fmt.Errorf("read TURN secret configuration: %w", err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return c, errors.New("TURN secret configuration must be private (0600)")
		}
		raw, err := os.ReadFile(c.TurnSecretFile)
		if err != nil {
			return c, err
		}
		// A single coturn configuration is the secret's source of truth.
		// No second copy of a server credential is sent to either client.
		for _, line := range strings.Split(string(raw), "\n") {
			if value, ok := strings.CutPrefix(strings.TrimSpace(line), "static-auth-secret="); ok {
				c.secret = []byte(strings.TrimSpace(value))
				break
			}
		}
		if len(c.secret) < 32 {
			return c, errors.New("TURN static-auth-secret is missing or too short")
		}
	}
	return c, nil
}

type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username"`
	Credential string   `json:"credential"`
}

func (c Config) iceServer(sessionID, role string, expires time.Time) ICEServer {
	username := fmt.Sprintf("%d:vantry-%s-%s", expires.Unix(), sessionID, role)
	// This HMAC-SHA1 is the existing TURN REST authentication protocol used
	// by coturn/WebRTC, not an application content identity or source lock.
	mac := hmac.New(sha1.New, c.secret)
	_, _ = mac.Write([]byte(username))
	return ICEServer{URLs: append([]string(nil), c.TurnURLs...), Username: username, Credential: base64.StdEncoding.EncodeToString(mac.Sum(nil))}
}

func randomToken(bytes int) (string, error) {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}
