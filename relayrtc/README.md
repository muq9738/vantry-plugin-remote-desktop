# Vantry private RTC relay

This independent Go submodule provides WebRTC signaling over SSH/Unix sockets and short-lived coturn credentials. Video, H264, decoding and keyboard/mouse delivery belong to the parent plugin's `rtc/` and `native/` components. This service does not proxy JPEG or expose an HTTP control port to the Internet.

## Private transport

This service is optional. LAN sharing does not contact it. For cross-network access, deploy it on your own Linux server and configure an SSH alias on each computer. An example entrypoint, using an alias chosen by the user, is:

```sh
ssh -T my-relay /opt/vantry-rtc/bin/relayrtc proxy --socket /run/vantry-rtc/signaling.sock
```

The stream carries ordinary HTTP/1.1, including WebSocket upgrade, directly to a Unix socket. Stdout is protocol only. The socket is 0600 in a 0700 runtime directory; The user-configured host uses an existing authorized SSH account. No SSH key is copied by this module.

## Session contract

- `POST /v1/sessions` accepts `{video:{width,height,fps,bitrate,profileLevelId,displayId}}`. Video metadata is an opaque object, preserved in POST/GET replies and never used as authorization.
- POST returns `sessionId`, `hostToken`, `viewerToken`, `iceServers`, `iceTransportPolicy:"relay"`, `expiresAt`, `sessionExpiresAt`, `video`, and optional `transport`/`turnTcpPort`.
- `expiresAt` is the RFC3339 expiration of the returned TURN credentials, normally ten minutes. It is not session expiration. An active host or authorized GET keeps the session alive; `sessionExpiresAt` describes the separate idle window. Sessions are in memory and end on host DELETE, idle expiry or service restart.
- `GET /v1/sessions/{id}` accepts the host or viewer Bearer token and returns fresh role-specific TURN credentials. It never returns either role token. Clients refresh before expiry, update ICE configuration and negotiate a host-owned ICE restart.
- `DELETE /v1/sessions/{id}` accepts only the host Bearer token.
- `GET /v1/sessions/{id}/signal?role=host|viewer` upgrades to WebSocket with the corresponding Bearer token.
- The server forwards `offer` (host), `answer` (viewer), `ice` and `bye`. `sdp` is a string; `candidate` is the standard `ICECandidateInit` object or null. Both peers receive `{type:"peer",connected:true}` when present; the remaining peer receives false on disconnect. A new authenticated connection for a role replaces its old connection.
- Unavailable peers or invalid messages produce a small `error` event; slow queues close the connection rather than silently dropping negotiation. Queues are bounded to 32 messages and individual messages to 64 KiB. Standard fragmented text messages and WebSocket ping/pong are supported.

No request, session ID, role token, SDP, ICE password or TURN key is logged. TURN REST HMAC and the RFC6455 upgrade calculation are standard wire protocols; there is no application content hash, build identity or source/version compatibility lock.

## Self-hosting

Build `./cmd/relayrtc` for your Linux server, and prepare an extracted coturn package tree with its runtime libraries. Install only onto a server you control:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o relayrtc ./cmd/relayrtc
# On your Linux server, with paths and IPs belonging to that server:
sudo python3 deploy/install.py --binary /path/to/relayrtc --turn-root /path/to/coturn-tree \
  --user YOUR_SSH_ACCOUNT --public-ip YOUR_PUBLIC_IP --private-ip YOUR_INTERFACE_IP
```

The installer requires the account and addresses explicitly. It creates only the two dedicated service units and their application directories, and refuses to overwrite an existing installation. It does not require any other private relay or IDE service. Permit the configured TURN ports if choosing public TURN. If public TURN cannot be reached, explicitly enable the SSH/TCP route on your own server:

```sh
sudo python3 deploy/enable_ssh_turn.py --binary /path/to/relayrtc --user YOUR_SSH_ACCOUNT
```

Both computers must configure SSH access to this same server, using their own local keys. Their SSH aliases need not match. No TURN secret or server private key goes into the app. Empty relay configuration selects LAN; it never selects a public or private default server.

## Optional loopback SSH/TCP TURN mode

When public TURN UDP/TCP ports cannot be reached, `deploy/enable_ssh_turn.py` adds a dedicated loopback coturn instance:

- `vantry-rtc-turn-ssh.service`: listens on `127.0.0.1:3479` (TCP only).
- Relay allocations: loopback UDP with `allow-loopback-peers` and authenticated TURN REST credentials.
- Session replies advertise `transport:"ssh-tcp"`, `turnTcpPort:3479`, and `turn:127.0.0.1:3479?transport=tcp`.
- Each client keeps a local ephemeral TCP listener and forwards connections via `ssh -W 127.0.0.1:3479 <your-relay-alias>`, keeping WebRTC/H.264/DTLS-SRTP and reliable input channels intact without opening public UDP rules.

## Build and checks

```sh
go test -race ./...
go vet ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o relayrtc ./cmd/relayrtc
```

There are no third-party Go dependencies. Tests cover role permissions, metadata, credential/session expiration distinction, WebSocket signaling, role replacement, cleanup, socket permissions, stdio proxying and the SSH/TCP configuration contract.
