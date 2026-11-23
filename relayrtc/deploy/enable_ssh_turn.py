#!/usr/bin/env python3
"""Enable the isolated SSH/TCP TURN route; preserve the public TURN instance."""
import json
import os
from pathlib import Path
import pwd
import grp
import subprocess
import argparse

parser=argparse.ArgumentParser()
parser.add_argument('--binary',type=Path,required=True)
parser.add_argument('--user',required=True)
args=parser.parse_args()
if os.geteuid()!=0:raise SystemExit('requires root')
user=pwd.getpwnam(args.user)
config=Path('/etc/vantry-rtc')
unit=Path('/etc/systemd/system/vantry-rtc-turn-ssh.service')
if unit.exists():raise SystemExit('SSH TURN service already exists; review before updating')
secret=None
for line in (config/'turn.conf').read_text().splitlines():
    if line.startswith('static-auth-secret='):secret=line.split('=',1)[1]
if not secret:raise SystemExit('existing application TURN secret missing')
baseline={name:subprocess.check_output(['systemctl','show',name,'--property=MainPID','--value'],text=True).strip() for name in ['vantry-rtc-turn.service']}
text=f'''listening-ip=127.0.0.1
relay-ip=127.0.0.1
listening-port=3479
min-port=52100
max-port=52163
realm=vantry-rtc
use-auth-secret
static-auth-secret={secret}
userdb=/var/lib/vantry-rtc/turn-ssh.sqlite
pidfile=/run/vantry-rtc-turn-ssh/turnserver.pid
allow-loopback-peers
no-udp
no-tls
no-dtls
no-cli
no-tcp-relay
no-multicast-peers
denied-peer-ip=0.0.0.0-255.255.255.255
allowed-peer-ip=127.0.0.1
relay-threads=2
total-quota=32
user-quota=4
no-stdout-log
log-file=/dev/null
'''
p=config/'turn-ssh.conf';p.write_text(text);p.chmod(0o600);os.chown(p,user.pw_uid,user.pw_gid)
unit.write_text(f'''[Unit]
Description=Vantry private SSH TCP TURN
After=network.target

[Service]
Type=simple
User={user.pw_name}
Group={grp.getgrgid(user.pw_gid).gr_name}
UMask=0077
RuntimeDirectory=vantry-rtc-turn-ssh
RuntimeDirectoryMode=0700
StateDirectory=vantry-rtc
StateDirectoryMode=0700
Environment=LD_LIBRARY_PATH=/opt/vantry-rtc/turn/usr/lib/x86_64-linux-gnu
ExecStart=/opt/vantry-rtc/turn/usr/bin/turnserver -c /etc/vantry-rtc/turn-ssh.conf
Restart=on-failure
RestartSec=2
TimeoutStopSec=5
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/run/vantry-rtc-turn-ssh /var/lib/vantry-rtc
RestrictAddressFamilies=AF_INET AF_UNIX
StandardOutput=null
StandardError=journal
LimitNOFILE=16384
MemoryMax=256M

[Install]
WantedBy=multi-user.target
''');unit.chmod(0o644)
subprocess.run(['systemctl','daemon-reload'],check=True)
subprocess.run(['systemctl','enable','--now',unit.name],check=True,capture_output=True)
# Change only this application's signaling service and its owned binary.
subprocess.run(['install','-m','0755',str(args.binary),'/opt/vantry-rtc/bin/relayrtc.next'],check=True)
os.replace('/opt/vantry-rtc/bin/relayrtc.next','/opt/vantry-rtc/bin/relayrtc')
p=config/'signaling.json';settings=json.loads(p.read_text())
settings.update(turnUrls=['turn:127.0.0.1:3479?transport=tcp'],transport='ssh-tcp',turnTcpPort=3479)
p.write_text(json.dumps(settings,indent=2)+'\n');p.chmod(0o600);os.chown(p,user.pw_uid,user.pw_gid)
subprocess.run(['systemctl','restart','vantry-rtc-signaling.service'],check=True)
for name in [unit.name,'vantry-rtc-signaling.service']:
    subprocess.run(['systemctl','is-active','--quiet',name],check=True)
for name,pid in baseline.items():
    current=subprocess.check_output(['systemctl','show',name,'--property=MainPID','--value'],text=True).strip()
    if current!=pid:raise SystemExit('pre-existing service process changed: '+name)
print(json.dumps({'ssh_turn':'127.0.0.1:3479','relay_ports':[52100,52163],'transport':'ssh-tcp','previous_services_untouched':True}))
