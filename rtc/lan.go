package rtc

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// LANTicket bootstraps TLS to the selected host. It carries the public
// certificate and a session-scoped access token; it never names a relay.
// Video and input retain WebRTC's DTLS-SRTP and data channels.
type LANTicket struct {
	Address, SessionID, Token string
	Certificate               []byte
}

func lanIP(ip net.IP) bool {
	return ip != nil && (ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast())
}

func ParseLANTicket(raw string) (LANTicket, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "vantry-lan" || u.User != nil || u.Fragment != "" {
		return LANTicket{}, errors.New("rtc: use a LAN connection link from the host")
	}
	host, port, err := net.SplitHostPort(u.Host)
	n, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || n < 1 || n > 65535 || !lanIP(net.ParseIP(host)) {
		return LANTicket{}, errors.New("rtc: LAN connection links require a private network address")
	}
	id, token := strings.TrimPrefix(u.Path, "/"), u.Query().Get("token")
	certificate, err := base64.RawURLEncoding.DecodeString(u.Query().Get("cert"))
	if !safeID.MatchString(id) || len(token) < 32 || len(token) > 256 || err != nil || len(certificate) > 8192 {
		return LANTicket{}, errors.New("rtc: invalid LAN connection link")
	}
	cert, err := x509.ParseCertificate(certificate)
	if err != nil || cert.VerifyHostname(host) != nil {
		return LANTicket{}, errors.New("rtc: LAN link certificate does not identify its host")
	}
	return LANTicket{u.Host, id, token, certificate}, nil
}

func (t LANTicket) String() string {
	u := url.URL{Scheme: "vantry-lan", Host: t.Address, Path: "/" + t.SessionID}
	u.RawQuery = url.Values{"token": {t.Token}, "cert": {base64.RawURLEncoding.EncodeToString(t.Certificate)}}.Encode()
	return u.String()
}

// LANRelay is signaling served by the host itself, without SSH, a TURN
// server, an external STUN server, or an Internet fallback. It has one host
// and one viewer for one explicitly enabled sharing session.
type LANRelay struct {
	mu       sync.Mutex
	info     SessionInfo
	peers    map[string]*SignalConn
	server   *http.Server
	listener net.Listener
	address  string
	addrs    []string
	der      []byte
	client   *http.Client
	tls      *tls.Config
	once     sync.Once
	wg       sync.WaitGroup
	closed   bool
}

func lanAddresses(listener net.Addr) []string {
	host, port, _ := net.SplitHostPort(listener.String())
	if ip := net.ParseIP(host); ip != nil && !ip.IsUnspecified() {
		return []string{net.JoinHostPort(host, port)}
	}
	interfaces, _ := net.Interfaces()
	// Physical interfaces precede bridges/VPNs in the copied connection link.
	sort.SliceStable(interfaces, func(i, j int) bool {
		physical := func(name string) bool {
			return strings.HasPrefix(name, "en") || strings.HasPrefix(name, "eth") || strings.HasPrefix(name, "wlan")
		}
		return physical(interfaces[i].Name) && !physical(interfaces[j].Name)
	})
	var result []string
	for _, nic := range interfaces {
		if nic.Flags&net.FlagUp == 0 || nic.Flags&net.FlagLoopback != 0 {
			continue
		}
		addresses, _ := nic.Addrs()
		for _, address := range addresses {
			ip, _, err := net.ParseCIDR(address.String())
			if err == nil && ip.To4() != nil && lanIP(ip) {
				result = append(result, net.JoinHostPort(ip.String(), port))
			}
		}
	}
	if len(result) == 0 {
		result = []string{net.JoinHostPort("127.0.0.1", port)}
	}
	return result
}

func newLANServer(listen string) (*LANRelay, error) {
	if listen == "" {
		listen = "0.0.0.0:0"
	}
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, err
	}
	failed := true
	defer func() {
		if failed {
			_ = listener.Close()
		}
	}()
	addresses := lanAddresses(listener.Addr())
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	certificate := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "Vantry LAN session"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().AddDate(1, 0, 0), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true}
	for _, address := range addresses {
		host, _, _ := net.SplitHostPort(address)
		certificate.IPAddresses = append(certificate.IPAddresses, net.ParseIP(host))
	}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	r := &LANRelay{peers: map[string]*SignalConn{}, listener: listener, address: addresses[0], addrs: addresses, der: der}
	roots := x509.NewCertPool()
	parsed, _ := x509.ParseCertificate(der)
	roots.AddCert(parsed)
	r.tls = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	r.client = &http.Client{Transport: &http.Transport{TLSClientConfig: r.tls, Proxy: nil}, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r.server = &http.Server{Handler: http.HandlerFunc(r.serve), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		_ = r.server.Serve(tls.NewListener(listener, &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}))
	}()
	failed = false
	return r, nil
}

func newLANClient(ticket LANTicket) (*LANRelay, error) {
	certificate, err := x509.ParseCertificate(ticket.Certificate)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	r := &LANRelay{address: ticket.Address, der: ticket.Certificate}
	r.tls = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	r.client = &http.Client{Transport: &http.Transport{TLSClientConfig: r.tls, Proxy: nil}, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return r, nil
}

func lanToken(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func (r *LANRelay) Create(ctx context.Context, video VideoConfig) (SessionInfo, error) {
	if err := ctx.Err(); err != nil {
		return SessionInfo{}, err
	}
	if err := validateVideo(video); err != nil {
		return SessionInfo{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.server == nil || r.closed || r.info.SessionID != "" {
		return SessionInfo{}, errors.New("rtc: LAN sharing is unavailable")
	}
	id, err := lanToken(18)
	if err != nil {
		return SessionInfo{}, err
	}
	host, err := lanToken(32)
	if err != nil {
		return SessionInfo{}, err
	}
	viewer, err := lanToken(32)
	if err != nil {
		return SessionInfo{}, err
	}
	r.info = SessionInfo{SessionID: id, HostToken: host, ViewerToken: viewer, ICETransportPolicy: "all", Video: video, Transport: "lan"}
	return r.info, nil
}

func (r *LANRelay) Ticket(session, token string) string {
	return (LANTicket{r.address, session, token, r.der}).String()
}

func (r *LANRelay) role(id, token string) string {
	if r.closed || id == "" || id != r.info.SessionID || token == "" {
		return ""
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(r.info.HostToken)) == 1 {
		return "host"
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(r.info.ViewerToken)) == 1 {
		return "viewer"
	}
	return ""
}

func (r *LANRelay) Get(ctx context.Context, id, token string) (SessionInfo, error) {
	if r.server != nil {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.role(id, token) == "" {
			return SessionInfo{}, errors.New("rtc: LAN session is no longer available")
		}
		info := r.info
		info.HostToken, info.ViewerToken = "", ""
		return info, nil
	}
	request, err := http.NewRequestWithContext(ctx, "GET", "https://"+r.address+"/v1/sessions/"+url.PathEscape(id), nil)
	if err != nil {
		return SessionInfo{}, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := r.client.Do(request)
	if err != nil {
		return SessionInfo{}, errors.New("rtc: LAN host is unreachable; connect both computers to the same local network")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return SessionInfo{}, fmt.Errorf("rtc: LAN host rejected the connection (HTTP %d)", response.StatusCode)
	}
	var info SessionInfo
	err = json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&info)
	if err == nil {
		err = validateSession(info, false)
	}
	if err == nil && info.SessionID != id {
		err = errors.New("rtc: LAN host returned a different sharing session")
	}
	if err == nil && (info.Transport != "lan" || len(info.ICEServers) != 0) {
		err = errors.New("rtc: LAN sharing cannot use an external relay")
	}
	return info, err
}

func (r *LANRelay) Delete(ctx context.Context, id, token string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.role(id, token) != "host" {
		return errors.New("rtc: LAN session cannot be stopped with this token")
	}
	for _, peer := range r.peers {
		_ = peer.Close()
	}
	r.info = SessionInfo{}
	return nil
}

func (r *LANRelay) Signal(ctx context.Context, id, role, token string) (*SignalConn, error) {
	if !safeID.MatchString(id) || (role != "host" && role != "viewer") {
		return nil, errors.New("rtc: invalid LAN signaling role")
	}
	dialer := websocket.Dialer{TLSClientConfig: r.tls, Proxy: nil, HandshakeTimeout: 10 * time.Second}
	conn, response, err := dialer.DialContext(ctx, "wss://"+r.address+"/v1/sessions/"+url.PathEscape(id)+"/signal?role="+role, http.Header{"Authorization": {"Bearer " + token}})
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		return nil, errors.New("rtc: LAN signaling connection failed")
	}
	conn.SetReadLimit(64 << 10)
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	return NewSignalConn(conn, func() { stop() }), nil
}

func (r *LANRelay) serve(w http.ResponseWriter, request *http.Request) {
	host, _, _ := net.SplitHostPort(request.RemoteAddr)
	if !lanIP(net.ParseIP(host)) {
		http.Error(w, "local network only", http.StatusForbidden)
		return
	}
	parts := strings.Split(strings.Trim(request.URL.Path, "/"), "/")
	if request.Method != "GET" || len(parts) < 3 || len(parts) > 4 || parts[0] != "v1" || parts[1] != "sessions" {
		http.NotFound(w, request)
		return
	}
	r.mu.Lock()
	role := r.role(parts[2], strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer "))
	info := r.info
	info.HostToken, info.ViewerToken = "", ""
	r.mu.Unlock()
	if role == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if len(parts) == 3 {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(info)
		return
	}
	if parts[3] != "signal" || request.URL.Query().Get("role") != role {
		http.NotFound(w, request)
		return
	}
	upgrader := websocket.Upgrader{HandshakeTimeout: 5 * time.Second, CheckOrigin: func(req *http.Request) bool { return req.Header.Get("Origin") == "" }}
	conn, err := upgrader.Upgrade(w, request, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(64 << 10)
	peer := NewSignalConn(conn, nil)
	otherRole := "viewer"
	if role == "viewer" {
		otherRole = "host"
	}
	r.mu.Lock()
	if r.closed || r.peers[role] != nil || r.role(parts[2], strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")) != role {
		r.mu.Unlock()
		_ = peer.Close()
		return
	}
	r.peers[role] = peer
	other := r.peers[otherRole]
	r.wg.Add(1)
	r.mu.Unlock()
	defer r.wg.Done()
	defer func() {
		_ = peer.Close()
		r.mu.Lock()
		if r.peers[role] == peer {
			delete(r.peers, role)
		}
		remaining := r.peers[otherRole]
		r.mu.Unlock()
		if remaining != nil {
			_ = remaining.Send(context.Background(), SignalMessage{Type: "peer", Connected: false})
		}
	}()
	if other != nil {
		if err := peer.Send(request.Context(), SignalMessage{Type: "peer", Connected: true}); err != nil {
			return
		}
		if err := other.Send(request.Context(), SignalMessage{Type: "peer", Connected: true}); err != nil {
			return
		}
	}
	for {
		message, err := peer.Read()
		if err != nil {
			return
		}
		if message.Type != "ice" && message.Type != "offer" && message.Type != "answer" {
			return
		}
		if (message.Type == "offer" && role != "host") || (message.Type == "answer" && role != "viewer") {
			return
		}
		r.mu.Lock()
		receiver := r.peers[otherRole]
		r.mu.Unlock()
		if receiver != nil && receiver.Send(request.Context(), message) != nil {
			return
		}
	}
}

func (r *LANRelay) Close() {
	r.once.Do(func() {
		r.mu.Lock()
		r.closed = true
		peers := make([]*SignalConn, 0, len(r.peers))
		for _, peer := range r.peers {
			peers = append(peers, peer)
		}
		r.mu.Unlock()
		for _, peer := range peers {
			_ = peer.Close()
		}
		if r.server != nil {
			_ = r.server.Close()
		}
		if r.client != nil {
			r.client.CloseIdleConnections()
		}
		r.wg.Wait()
	})
}
