#!/usr/bin/env python3
"""Install versioned OpenFlux support without changing binary, cookies or routing."""
import argparse, json, os, pathlib, shutil, subprocess, tempfile, time
from router_integration import patch_core, patch_panel

MODULES = ('openflux_release.py', 'openflux_auth.py', 'cookie_import.py',
           'recovery_crypto.py', 'recovery_inbox.py', 'router_integration.py')
ROOT = pathlib.Path(__file__).resolve().parent

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--role', choices=('client', 'server'), required=True)
    parser.add_argument('--expected-exit-ip', help='Optional client health-check exit IP')
    parser.add_argument('--disk-token-file', type=pathlib.Path)
    parser.add_argument('--disk-path', default='disk:/OpenFlux Recovery/server-recovery.json')
    parser.add_argument('--sender-public-key', type=pathlib.Path)
    args = parser.parse_args()
    if os.geteuid() != 0: parser.error('Run with sudo')
    if bool(args.disk_token_file) != bool(args.sender_public_key):
        parser.error('Pair --disk-token-file with --sender-public-key')
    if args.role == 'client' and args.disk_token_file: parser.error('Disk receiver belongs on the server')
    files = {}
    lib = pathlib.Path('/usr/local/lib/router-updater' if args.role == 'client' else '/usr/local/lib/openflux-integration')
    for name in MODULES:
        data = (ROOT / name).read_bytes(); compile(data, name, 'exec')
        files[lib / name] = (data, 0o644)
    if args.role == 'client':
        core = lib / 'router_update_core.py'
        panel = pathlib.Path('/usr/local/lib/router-panel/router-panel.py')
        # Refuse unknown layouts before the first write. Preserve each router's runtime hooks.
        files[core] = (patch_core(core.read_text()).encode(), 0o644)
        files[panel] = (patch_panel(panel.read_text()).encode(), 0o644)
        if args.expected_exit_ip:
            import ipaddress
            settings = pathlib.Path('/etc/router/updater/settings.json')
            data = json.loads(settings.read_text()) if settings.exists() else {}
            data['openflux_expected_exit_ip'] = str(ipaddress.ip_address(args.expected_exit_ip))
            files[settings] = (json.dumps(data, indent=2).encode(), 0o600)
        files[pathlib.Path('/usr/local/sbin/openflux-client-release')] = (b'''#!/bin/sh
set -eu
case "${1:-status}" in
 status) exec /usr/bin/python3 /usr/local/lib/router-updater/router_update_core.py status ;;
 check|prepare|install|rollback) action="$1" ;;
 stage|download) action=prepare ;;
 activate) action=install ;;
 *) echo 'Usage: openflux-client-release status|check|download|install|rollback' >&2; exit 2 ;;
esac
exec /usr/bin/python3 /usr/local/lib/router-updater/router_update_core.py "$action" openflux
''', 0o755)
    else:
        from cryptography.hazmat.primitives import serialization
        from cryptography.hazmat.primitives.asymmetric import ed25519, rsa
        cfg = pathlib.Path('/etc/openflux-recovery')
        if args.disk_token_file:
            token = args.disk_token_file.read_text().strip()
            if not token or '\n' in token: raise ValueError('Invalid Disk token')
            sender = serialization.load_pem_public_key(args.sender_public_key.read_bytes())
            if not isinstance(sender, ed25519.Ed25519PublicKey): raise ValueError('Expected Ed25519 sender key')
            files[cfg / 'disk-token'] = (token.encode(), 0o600)
            files[cfg / 'sender-public.pem'] = (args.sender_public_key.read_bytes(), 0o600)
            files[cfg / 'config.json'] = (json.dumps({'path':args.disk_path}).encode(), 0o600)
            if not (cfg / 'server-private.pem').exists():
                key = rsa.generate_private_key(public_exponent=65537, key_size=3072)
                files[cfg / 'server-private.pem'] = (key.private_bytes(serialization.Encoding.PEM,
                    serialization.PrivateFormat.PKCS8, serialization.NoEncryption()), 0o600)
                files[cfg / 'server-public.pem'] = (key.public_key().public_bytes(
                    serialization.Encoding.PEM, serialization.PublicFormat.SubjectPublicKeyInfo), 0o644)
    files[pathlib.Path('/usr/local/sbin/openflux-auth')] = (
        ('#!/bin/sh\nexec /usr/bin/python3 '+str(lib / 'openflux_auth.py')+' "$@"\n').encode(), 0o755)
    files[pathlib.Path('/usr/local/sbin/openflux-yandex-cookie-import')] = ((ROOT / 'cookie_import.py').read_bytes(), 0o755)
    timers = [('openflux-auth-status', '/usr/local/sbin/openflux-auth status --quiet', 15)]
    cfg = pathlib.Path('/etc/openflux-recovery')
    if args.role == 'server' and (args.disk_token_file or (cfg / 'config.json').exists()):
        timers.append(('openflux-recovery-inbox', '/usr/bin/python3 '+str(lib / 'recovery_inbox.py'), 60))
    for name, cmd, seconds in timers:
        unit = pathlib.Path('/etc/systemd/system') / (name + '.service')
        files[unit] = (('[Unit]\nDescription=OpenFlux status and recovery\nAfter=network-online.target\n'
            '[Service]\nType=oneshot\nUMask=0077\nNice=10\nTimeoutStartSec=150\nExecStart='+cmd+'\n').encode(), 0o644)
        files[unit.with_suffix('.timer')] = (('[Unit]\nDescription=OpenFlux support poll\n'
            '[Timer]\nOnBootSec=30\nOnUnitActiveSec='+str(seconds)+'\nAccuracySec=2\n'
            '[Install]\nWantedBy=timers.target\n').encode(), 0o644)
    for path, (data, mode) in files.items():
        if path.suffix == '.py': compile(data, str(path), 'exec')
    checkpoint = pathlib.Path('/var/backups/openflux-integration') / (time.strftime('%Y%m%dT%H%M%SZ', time.gmtime())+'-'+str(os.getpid()))
    checkpoint.mkdir(parents=True, mode=0o700)
    manifest = []
    for path in files:
        manifest.append({'path':str(path), 'present':path.exists()})
        if path.exists():
            dest = checkpoint / str(path).lstrip('/'); dest.parent.mkdir(parents=True, exist_ok=True); shutil.copy2(path, dest)
    (checkpoint / 'manifest.json').write_text(json.dumps(manifest, indent=2))
    enabled_before = {name:subprocess.run(['systemctl','is-enabled',name+'.timer'],capture_output=True).returncode == 0 for name,_,_ in timers}
    try:
        for path, (data, mode) in files.items():
            path.parent.mkdir(parents=True, exist_ok=True, mode=0o700 if str(path).startswith('/etc/openflux-recovery/') else 0o755)
            fd, temp = tempfile.mkstemp(dir=path.parent)
            try:
                with os.fdopen(fd,'wb') as f: f.write(data); f.flush(); os.fsync(f.fileno())
                os.chmod(temp,mode); os.replace(temp,path)
            finally:
                if os.path.exists(temp): os.unlink(temp)
        subprocess.run(['systemctl','daemon-reload'],check=True)
        for name, _, _ in timers: subprocess.run(['systemctl','enable','--now',name+'.timer'],check=True,capture_output=True)
        if args.role == 'client': subprocess.run(['systemctl','try-restart','router-panel.service'],check=True)
        subprocess.run(['/usr/local/sbin/openflux-auth','status','--quiet'],check=True)
    except BaseException:
        for record in manifest:
            path = pathlib.Path(record['path'])
            if record['present']: shutil.copy2(checkpoint / str(path).lstrip('/'),path)
            else: path.unlink(missing_ok=True)
        for name, was_enabled in enabled_before.items():
            if not was_enabled: subprocess.run(['systemctl','disable','--now',name+'.timer'],capture_output=True)
        subprocess.run(['systemctl','daemon-reload'],check=False)
        if args.role == 'client': subprocess.run(['systemctl','try-restart','router-panel.service'],check=False)
        raise
    print(json.dumps({'installed':args.role,'checkpoint':str(checkpoint),'binary_and_router_mode':'unchanged'}))

if __name__ == '__main__': main()
