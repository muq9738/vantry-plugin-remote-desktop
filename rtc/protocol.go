// Package rtc implements hardware H.264 remote desktop over WebRTC.
// LAN sharing serves signaling on the host. Cross-network sharing uses an
// explicitly configured private SSH/TURN relay. Neither route has a public default.
package rtc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"net/url"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	root "github.com/muq9738/vantry-plugin-remote-desktop"
	"github.com/pion/webrtc/v4"
)

const CaptureTimeURI = "http://www.webrtc.org/experiments/rtp-hdrext/abs-capture-time"
const MaxNativePayload = 64 << 20

const h264HighProfileLevelID = "640033"
const h264ConstrainedHighProfileLevelID = "640c33"

var ErrClosed = errors.New("rtc: session closed")
var safeID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var safeSSHHost = regexp.MustCompile(`^[A-Za-z0-9_@.:-]+$`)

type VideoConfig struct {
	Width          int    `json:"width"`
	Height         int    `json:"height"`
	FPS            int    `json:"fps"`
	Bitrate        int    `json:"bitrate"`
	ProfileLevelID string `json:"profileLevelId"`
	DisplayID      uint32 `json:"displayId,omitempty"`
}

func Quality(name string) (VideoConfig, error) {
	profile := h264HighProfileLevelID
	if runtime.GOOS == "darwin" {
		profile = h264ConstrainedHighProfileLevelID
	}
	switch name {
	case "ultra":
		return VideoConfig{2560, 1440, 60, 8000000, profile, 0}, nil
	case "", "balanced":
		return VideoConfig{1920, 1080, 30, 4000000, profile, 0}, nil
	case "low":
		return VideoConfig{1280, 720, 30, 1500000, profile, 0}, nil
	default:
		return VideoConfig{}, fmt.Errorf("rtc: unknown quality %q", name)
	}
}

type SessionInfo struct {
	SessionID          string             `json:"sessionId"`
	HostToken          string             `json:"hostToken,omitempty"`
	ViewerToken        string             `json:"viewerToken,omitempty"`
	ICEServers         []webrtc.ICEServer `json:"iceServers"`
	ICETransportPolicy string             `json:"iceTransportPolicy"`
	ExpiresAt          time.Time          `json:"expiresAt"`
	Video              VideoConfig        `json:"video"`
	Transport          string             `json:"transport,omitempty"`
	TurnTCPPort        int                `json:"turnTcpPort,omitempty"`
}

func (s SessionInfo) validateICE() error {
	if s.Transport == "lan" {
		if s.ICETransportPolicy == "all" && len(s.ICEServers) == 0 {
			return nil
		}
		return errors.New("rtc: LAN sharing cannot use an external relay")
	}
	if s.ICETransportPolicy != "relay" || len(s.ICEServers) == 0 {
		return errors.New("rtc: relay did not supply relay-only TURN configuration")
	}
	for _, server := range s.ICEServers {
		credential, ok := server.Credential.(string)
		if len(server.URLs) == 0 || server.Username == "" || !ok || credential == "" {
			return errors.New("rtc: unauthenticated TURN configuration")
		}
		for _, raw := range server.URLs {
			if !strings.HasPrefix(raw, "turn:") && !strings.HasPrefix(raw, "turns:") {
				return errors.New("rtc: only TURN relay candidates are allowed")
			}
		}
	}
	return nil
}

type SignalMessage struct {
	Type      string                   `json:"type"`
	SDP       string                   `json:"sdp,omitempty"`
	Candidate *webrtc.ICECandidateInit `json:"candidate,omitempty"`
	Connected bool                     `json:"connected,omitempty"`
}

type Ticket struct{ SSHHost, SessionID, Token string }

func (t Ticket) String() string {
	if !validSSH(t.SSHHost) || !safeID.MatchString(t.SessionID) || t.Token == "" {
		return ""
	}
	u := url.URL{Scheme: "vantry-rtc", Host: t.SSHHost, Path: "/" + t.SessionID}
	q := url.Values{}
	q.Set("token", t.Token)
	u.RawQuery = q.Encode()
	return u.String()
}
func ParseTicket(raw string) (Ticket, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "vantry-rtc" || u.User != nil {
		return Ticket{}, errors.New("rtc: use a remote desktop connection link")
	}
	t := Ticket{u.Host, strings.TrimPrefix(u.Path, "/"), u.Query().Get("token")}
	if !validSSH(t.SSHHost) || !safeID.MatchString(t.SessionID) || len(t.Token) < 32 || len(t.Token) > 256 {
		return Ticket{}, errors.New("rtc: invalid connection ticket")
	}
	return t, nil
}
func validSSH(s string) bool {
	return len(s) > 0 && len(s) <= 255 && !strings.HasPrefix(s, "-") && safeSSHHost.MatchString(s)
}

// Relay is the private signaling contract. Test implementations may use a local
// authenticated Unix/HTTP fixture; production uses SSHRelay below.
type Relay interface {
	Create(context.Context, VideoConfig) (SessionInfo, error)
	Get(context.Context, string, string) (SessionInfo, error)
	Signal(context.Context, string, string, string) (*SignalConn, error)
	Delete(context.Context, string, string) error
}

type SignalConn struct {
	Conn   *websocket.Conn
	close  func()
	writes chan struct{}
}

func NewSignalConn(c *websocket.Conn, cleanup func()) *SignalConn {
	return &SignalConn{Conn: c, close: cleanup, writes: make(chan struct{}, 1)}
}
func (c *SignalConn) Send(ctx context.Context, m SignalMessage) error {
	select {
	case c.writes <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-c.writes }()
	_ = c.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return c.Conn.WriteJSON(m)
}
func (c *SignalConn) Read() (SignalMessage, error) {
	var m SignalMessage
	err := c.Conn.ReadJSON(&m)
	return m, err
}
func (c *SignalConn) Close() error {
	err := c.Conn.Close()
	if c.close != nil {
		c.close()
	}
	return err
}

// EncodedFrame is an Annex-B access unit with its actual wall-clock capture time.
type EncodedFrame struct {
	CapturedAt time.Time
	KeyFrame   bool
	Data       []byte
}
type EncodedSource interface {
	Next(context.Context) (EncodedFrame, error)
	RequestKeyFrame() error
	SetBitrate(int) error
	Close() error
}

// FrameDecoder outputs RGBA frames through a callback. The receiver owns buffers
// until the next frame request; callers must not mutate returned image data.
type FrameDecoder interface {
	Decode(context.Context, EncodedFrame) error
	Close() error
}
type DecodedFrame struct {
	CapturedAt time.Time
	DecodedAt  time.Time
	Image      *image.RGBA
	Release    func()
	Dropped    bool
}

type HostConfig struct {
	// Empty SSHHost selects LAN sharing; no external relay is inferred.
	LANListenAddr string
	Relay         Relay
	SSHHost       string
	HelperPath    string
	DisplayID     uint32
	Quality       string
	InputSink     root.InputSink
	// SourceFactory injects a real synthetic encoder for acceptance, never a
	// production fallback when capture permission is absent.
	SourceFactory func(context.Context, VideoConfig) (EncodedSource, error)
}

type ConnectConfig struct {
	// SSHHost can select this viewer's own alias for the configured relay.
	SSHHost        string
	Ticket         string
	HelperPath     string
	Relay          Relay
	DecoderFactory func(context.Context, func(DecodedFrame)) (FrameDecoder, error)
}

type Stats struct {
	Connected           bool    `json:"connected"`
	Mode                string  `json:"mode"`
	Transport           string  `json:"transport"`
	Codec               string  `json:"codec"`
	Hardware            bool    `json:"hardware"`
	RelayHost           string  `json:"relayHost"`
	LocalCandidateType  string  `json:"localCandidateType"`
	RemoteCandidateType string  `json:"remoteCandidateType"`
	CandidateProtocol   string  `json:"candidateProtocol"`
	Width               int     `json:"width"`
	Height              int     `json:"height"`
	FPS                 float64 `json:"fps"`
	Kbps                float64 `json:"kbps"`
	RTTMs               float64 `json:"rttMs"`
	FirstFrameMs        float64 `json:"firstFrameMs"`
	FrameAgeMs          float64 `json:"frameAgeMs"`
	FrameAgeP50Ms       float64 `json:"frameAgeP50Ms"`
	FrameAgeP95Ms       float64 `json:"frameAgeP95Ms"`
	ClockOffsetMs       float64 `json:"clockOffsetMs"`
	ClockSynchronized   bool    `json:"clockSynchronized"`
	Frames              uint64  `json:"frames"`
	Dropped             uint64  `json:"dropped"`
	Error               string  `json:"error,omitempty"`
}

func jsonData(v any) []byte { b, _ := json.Marshal(v); return b }
