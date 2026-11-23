#!/usr/bin/env python3
"""Install Vantry signaling/TURN on a user-supplied Linux server.

Run as root. TURN_ROOT is an extracted official coturn package tree.
Only the dedicated Vantry paths and two new service units are created.
"""
import argparse
import grp
import ipaddress
import json
import os
from pathlib import Path
import pwd
import secrets
import shutil
import subprocess

parser = argparse.ArgumentParser()
parser.add_argument("--binary", type=Path, required=True)
parser.add_argument("--turn-root", type=Path, required=True)
parser.add_argument("--user", required=True)
parser.add_argument("--public-ip", required=True)
parser.add_argument("--private-ip", required=True)
parser.add_argument("--turn-port", type=int, default=3478)
parser.add_argument("--relay-min", type=int, default=52000)
parser.add_argument("--relay-max", type=int, default=52063)
args = parser.parse_args()
if os.geteuid() != 0:
    raise SystemExit("install requires root")
ipaddress.IPv4Address(args.public_ip)
ipaddress.IPv4Address(args.private_ip)
if not 1024 < args.turn_port < 65536 or not 1024 < args.relay_min <= args.relay_max < 65536:
    raise SystemExit("invalid dedicated TURN ports")
account = pwd.getpwnam(args.user)
base = Path("/opt/vantry-rtc")
config = Path("/etc/vantry-rtc")
units = ["vantry-rtc-signaling.service", "vantry-rtc-turn.service"]
for path in [base, config, *(Path("/etc/systemd/system") / unit for unit in units)]:
    if path.exists():
        raise SystemExit("dedicated target already exists; review it before updating: " + str(path))
if not args.binary.is_file() or not (args.turn_root / "usr/bin/turnserver").is_file():
    raise SystemExit("reviewed binaries are missing")

def run(command):
    subprocess.run(command, check=True, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, timeout=20)

def private_file(path, content):
    path.write_text(content)
    path.chmod(0o600)
    os.chown(path, account.pw_uid, account.pw_gid)

base.mkdir(mode=0o755)
(base / "bin").mkdir(mode=0o755)
shutil.copy2(args.binary, base / "bin/relayrtc")
(base / "bin/relayrtc").chmod(0o755)
shutil.copytree(args.turn_root, base / "turn", symlinks=True)
config.mkdir(mode=0o700)
os.chown(config, account.pw_uid, account.pw_gid)
# The TURN shared key stays on the user's server in this 0600 configuration.
secret = secrets.token_urlsafe(48)
turn_config = f"""listening-ip={args.private_ip}
relay-ip={args.private_ip}
external-ip={args.public_ip}/{args.private_ip}
listening-port={args.turn_port}
min-port={args.relay_min}
max-port={args.relay_max}
realm=vantry-rtc
use-auth-secret
static-auth-secret={secret}
userdb=/var/lib/vantry-rtc/turn.sqlite
pidfile=/run/vantry-rtc-turn/turnserver.pid
no-tls
no-dtls
no-cli
no-tcp-relay
no-multicast-peers
# Both ICE endpoints use this explicitly configured TURN server.
denied-peer-ip=0.0.0.0-255.255.255.255
allowed-peer-ip={args.public_ip}
relay-threads=2
total-quota=32
user-quota=4
no-stdout-log
log-file=/dev/null
"""
private_file(config / "turn.conf", turn_config)
private_file(config / "signaling.json", json.dumps({
    "socket": "/run/vantry-rtc/signaling.sock",
    "turnUrls": [f"turn:{args.public_ip}:{args.turn_port}?transport=udp", f"turn:{args.public_ip}:{args.turn_port}?transport=tcp"],
    "turnSecretFile": str(config / "turn.conf"),
    "credentialSeconds": 600, "sessionIdleSeconds": 3600, "maxSessions": 8,
}, indent=2) + "\n")

common = f"""User={args.user}
Group={grp.getgrgid(account.pw_gid).gr_name}
UMask=0077
Restart=on-failure
RestartSec=2
TimeoutStopSec=5
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
"""
signaling = f"""[Unit]
Description=Vantry private RTC signaling
After=network.target

[Service]
Type=simple
{common}RuntimeDirectory=vantry-rtc
RuntimeDirectoryMode=0700
ExecStart=/opt/vantry-rtc/bin/relayrtc serve --config /etc/vantry-rtc/signaling.json
RestrictAddressFamilies=AF_UNIX
ReadWritePaths=/run/vantry-rtc
MemoryMax=128M

[Install]
WantedBy=multi-user.target
"""
turn = f"""[Unit]
Description=Vantry dedicated authenticated TURN
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
{common}RuntimeDirectory=vantry-rtc-turn
RuntimeDirectoryMode=0700
StateDirectory=vantry-rtc
StateDirectoryMode=0700
Environment=LD_LIBRARY_PATH=/opt/vantry-rtc/turn/usr/lib/x86_64-linux-gnu
ExecStart=/opt/vantry-rtc/turn/usr/bin/turnserver -c /etc/vantry-rtc/turn.conf
ReadWritePaths=/run/vantry-rtc-turn /var/lib/vantry-rtc
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
LimitNOFILE=16384
StandardOutput=null
StandardError=journal
MemoryMax=256M

[Install]
WantedBy=multi-user.target
"""
for name, content in zip(units, [signaling, turn]):
    path = Path("/etc/systemd/system") / name
    path.write_text(content)
    path.chmod(0o644)
try:
    run(["systemctl", "daemon-reload"])
    run(["systemctl", "enable", "--now", *units])
    for unit in units:
        run(["systemctl", "is-active", "--quiet", unit])
except Exception:
    subprocess.run(["systemctl", "stop", *units], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=10)
    raise
print(json.dumps({"services": units, "socket": "/run/vantry-rtc/signaling.sock", "turn_port": args.turn_port,
                  "relay_ports": [args.relay_min, args.relay_max], "secret_mode": "0600"}))
