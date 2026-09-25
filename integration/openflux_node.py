#!/usr/bin/env python3
"""Install and maintain OpenFlux itself. Router configuration is outside this tool."""
import argparse, contextlib, hashlib, json, os, pathlib, platform, pwd, re, shlex
import shutil, subprocess, sys, tempfile, time, urllib.request

REPO = 'LevL-max/OpenFlux-mod'
CONFIG = pathlib.Path('/etc/openflux/node.json')
STATE = pathlib.Path('/var/lib/openflux-updater')
SELF_ASSET = 'openflux-node.py'
HELPER = pathlib.Path('/usr/local/sbin/openflux-yandex-cookie-import')
COMMAND_DIR = pathlib.Path('/usr/local/sbin')
UNIT_DIR = pathlib.Path('/etc/systemd/system')
RUNTIME_DIR = pathlib.Path('/var/lib/openflux/runtime')

def read(path, default=None):
    try: return json.loads(pathlib.Path(path).read_text())
    except FileNotFoundError: return default

def save(path, value, mode=0o600):
    path = pathlib.Path(path); path.parent.mkdir(parents=True, exist_ok=True)
    fd, name = tempfile.mkstemp(dir=path.parent, prefix='.openflux-')
    try:
        with os.fdopen(fd, 'w') as f:
            json.dump(value, f, indent=2); f.write('\n'); f.flush(); os.fsync(f.fileno())
        os.chmod(name, mode); os.replace(name, path)
    finally:
        if os.path.exists(name): os.unlink(name)

def sha(path): return hashlib.sha256(pathlib.Path(path).read_bytes()).hexdigest()

def run(argv, check=True, timeout=60):
    p = subprocess.run(argv, capture_output=True, text=True, timeout=timeout)
    if check and p.returncode: raise RuntimeError('Command failed: '+argv[0]+' (exit '+str(p.returncode)+'). Check its service log.')
    return p

def config():
    c = read(CONFIG)
    if not c: raise ValueError('Run install or adopt first; configuration is /etc/openflux/node.json')
    validate(c); return c

def validate(c):
    if c.get('role') not in ('client', 'server'): raise ValueError('Choose client or server')
    if c.get('backend') not in ('systemd', 'docker'): raise ValueError('Choose systemd or docker')
    for field in ('binary', 'library', 'cookie_store'):
        if not pathlib.Path(c.get(field, '')).is_absolute(): raise ValueError(field+' must be an absolute path')
    if not re.fullmatch(r'[A-Za-z0-9_.@-]+', c.get('service' if c['backend']=='systemd' else 'container', '')):
        raise ValueError('Invalid service/container name')
    if c.get('channel') not in ('stable', 'prerelease'): raise ValueError('Channel must be stable or prerelease')
    args = c.get('args', [])
    if not isinstance(args, list) or not all(isinstance(x, str) and '\x00' not in x for x in args):
        raise ValueError('args must be an array of arguments, not a shell command')

def fetch(url, path=None, limit=70*1024*1024):
    if not url.startswith('https://'): raise ValueError('HTTPS is required')
    with urllib.request.urlopen(urllib.request.Request(url, headers={'User-Agent':'OpenFluxUpdater/2'}), timeout=45) as r:
        data = r.read(limit+1)
    if len(data)>limit: raise ValueError('Download exceeds size limit')
    if path: pathlib.Path(path).write_bytes(data)
    else: return json.loads(data)

def support(c):
    sys.path.insert(0, c['library'])
    import openflux_release
    return openflux_release

def releases(c):
    mod = support(c)
    rows = fetch('https://api.github.com/repos/'+REPO+'/releases?per_page=100', limit=8*1024*1024)
    choices = []
    for row in rows:
        if row.get('draft') or not row.get('published_at') or (row.get('prerelease') and c['channel']=='stable'): continue
        try: mod.version(row.get('tag_name'))
        except ValueError: continue
        choices.append(row)
    if not choices: raise ValueError('No published release in this channel')
    return sorted(choices, key=lambda r:mod.version(r['tag_name']), reverse=True)

def identify(c, rows=None):
    digest = sha(c['binary']); recorded = read(STATE/'installed.json', {})
    if recorded.get('sha256')==digest: return recorded
    if c.get('router_updater'):
        recorded = read('/var/lib/router-updater/openflux/installed.json', {})
        if recorded.get('sha256')==digest: return recorded
    for row in (rows if rows is not None else releases(c)):
        if any(a['name']=='openflux-linux-amd64' and a.get('digest')=='sha256:'+digest for a in row.get('assets', [])):
            return {'version':row['tag_name'], 'sha256':digest}
    return {'version':'unknown', 'sha256':digest}

def candidate(c):
    rows = releases(c); mod = support(c); newest = mod.metadata(rows[0]); installed = identify(c, rows)
    newest['installed'] = installed; newest['update_available'] = not mod.is_current(installed, newest)
    return newest

def verify_download(row, directory):
    # Bootstrap and updates use the same published digests and release manifest.
    base = 'https://github.com/'+REPO+'/releases/'
    tag = row['tag_name']
    if not re.fullmatch(r'v\d+\.\d+\.\d+(?:-rc\d+)?', tag) or row.get('html_url')!=base+'tag/'+tag:
        raise ValueError('Unexpected release identity')
    required = ('openflux-linux-amd64','openflux-yandex-cookie-import','openflux-integration-linux.tar.gz',SELF_ASSET,'SHA256SUMS')
    assets = {}
    for name in required:
        found = [a for a in row.get('assets', []) if a.get('name')==name]
        if len(found)!=1: raise ValueError('Missing or duplicate asset '+name)
        a = found[0]
        if a.get('browser_download_url')!=base+'download/'+tag+'/'+name or not re.fullmatch(r'sha256:[0-9a-f]{64}', a.get('digest','')):
            raise ValueError('Unverified asset '+name)
        fetch(a['browser_download_url'], directory/name, 70*1024*1024 if name=='openflux-linux-amd64' else 2*1024*1024)
        if sha(directory/name)!=a['digest'][7:]: raise ValueError('Asset digest mismatch: '+name)
        assets[name] = a['digest'][7:]
    sums = {}
    for line in (directory/'SHA256SUMS').read_text().splitlines():
        match = re.fullmatch(r'([0-9a-f]{64})\s+\*?(?:dist/)?([\w.-]+)', line.strip())
        if not match or match[2] in sums: raise ValueError('Invalid checksum manifest')
        sums[match[2]] = match[1]
    if any(sums.get(n)!=d for n,d in assets.items() if n!='SHA256SUMS'): raise ValueError('Release manifest mismatch')
    compile((directory/SELF_ASSET).read_text(), SELF_ASSET, 'exec')
    os.chmod(directory/'openflux-linux-amd64', 0o755)
    p = run([str(directory/'openflux-linux-amd64'),'--help'])
    if 'yandex-cookie-store' not in p.stdout+p.stderr: raise ValueError('Unsupported release binary')
    return assets

def atomic_file(source, target, mode=0o755):
    target = pathlib.Path(target); target.parent.mkdir(parents=True, exist_ok=True)
    fd, temp = tempfile.mkstemp(dir=target.parent, prefix='.openflux-'); os.close(fd)
    try: shutil.copyfile(source, temp); os.chmod(temp, mode); os.replace(temp, target)
    finally:
        if os.path.exists(temp): os.unlink(temp)

def active(c):
    if c['backend']=='systemd': return run(['systemctl','is-active','--quiet',c['service']],check=False).returncode==0
    p = run(['docker','inspect','--format={{.State.Running}}',c['container']],check=False)
    return p.returncode==0 and p.stdout.strip()=='true'

def runtime_action(c, action):
    if c['backend']=='systemd': run(['systemctl', action, c['service']])
    else: run(['docker', action, c['container']])

def health(c, seconds=100):
    support(c); import openflux_auth
    deadline = time.monotonic()+seconds; first_running = None
    while time.monotonic()<deadline:
        if not active(c): raise RuntimeError('OpenFlux stopped during its health check')
        if c.get('transport','yandex')=='yandex':
            status = openflux_auth.status()
            if status.get('state')=='auth_blocked': raise RuntimeError('AUTH_BLOCKED: import fresh browser cookies')
            ready = status.get('state')=='connected'
        else:
            first_running = first_running or time.monotonic()
            ready = time.monotonic()-first_running>=15
        if ready:
            if c['role']=='server': return
            p = run(['curl','-q','--fail','--silent','--noproxy','','--proxy','socks5h://'+c.get('socks','127.0.0.1:1080'),'--max-time','12','https://api.ipify.org'],check=False,timeout=15)
            if p.returncode==0 and (not c.get('expected_exit_ip') or p.stdout.strip()==c['expected_exit_ip']): return
        time.sleep(2)
    raise RuntimeError('OpenFlux health check failed; previous release will be restored')

def stage(c):
    STATE.mkdir(parents=True,exist_ok=True); mod = support(c); rows = releases(c); row = rows[0]
    current = identify(c, rows)
    if current['version']=='unknown': raise ValueError('Current binary is not a recognized release; identify it before updating')
    if mod.version(row['tag_name'])<mod.version(current['version']): raise ValueError('Downgrade requires rollback')
    if row['tag_name']==current['version']:
        expected = next(a['digest'][7:] for a in row['assets'] if a['name']=='openflux-linux-amd64')
        if expected!=current['sha256']: raise ValueError('Same-version binary replacement is refused')
        return {'version':current['version'],'current':True}
    directory = pathlib.Path(tempfile.mkdtemp(prefix='candidate-',dir=STATE))
    assets = verify_download(row, directory)
    mod.extract_bundle(directory/'openflux-integration-linux.tar.gz',directory/'integration')
    data = {'version':row['tag_name'],'directory':str(directory),'assets':assets,'base_sha256':current['sha256']}
    save(STATE/'staged.json', data); return data

def checkpoint(c):
    directory = pathlib.Path(tempfile.mkdtemp(prefix='backup-',dir=STATE)); paths = [pathlib.Path(c['binary'])]
    paths += [pathlib.Path(c['library'])/name for name in support(c).MODULES]
    paths += [pathlib.Path(c['library'])/'openflux_node.py',HELPER]
    records = []
    for i,p in enumerate(paths):
        item = {'path':str(p),'present':p.exists(),'copy':str(i)}
        if p.exists():
            shutil.copy2(p,directory/str(i)); item.update(sha256=sha(p),mode=p.stat().st_mode&0o777)
        records.append(item)
    data = {'directory':str(directory),'files':records,'installed':identify(c),'active':active(c),'config':c}
    save(directory/'checkpoint.json',data); return data

def restore(data):
    c = data['config']; runtime_action(c,'stop')
    for item in data['files']:
        p = pathlib.Path(item['path'])
        if item['present']:
            source = pathlib.Path(data['directory'])/item['copy']
            if sha(source)!=item['sha256']: raise ValueError('Rollback checksum mismatch')
            atomic_file(source,p,item['mode'])
        else: p.unlink(missing_ok=True)
    if data['active']: runtime_action(c,'start')
    save(STATE/'installed.json',data['installed']); (STATE/'transaction.json').unlink(missing_ok=True)
    if data.get('watchdog'): run(['systemctl','stop',data['watchdog']+'.timer'],check=False)

def install_staged(c):
    if (STATE/'transaction.json').exists(): raise ValueError('Interrupted update exists; run openfluxctl rollback first')
    data = read(STATE/'staged.json')
    if not data: raise ValueError('Run openfluxctl download first')
    if sha(c['binary'])!=data['base_sha256']: raise ValueError('Installed binary changed after download')
    directory = pathlib.Path(data['directory'])
    for name,digest in data['assets'].items():
        if sha(directory/name)!=digest: raise ValueError('Staged release changed')
    support(c).extract_bundle(directory/'openflux-integration-linux.tar.gz',directory/'integration')
    prior = checkpoint(c)
    watchdog = 'openflux-update-return-'+str(int(time.time()))
    prior['watchdog']=watchdog; save(pathlib.Path(prior['directory'])/'checkpoint.json',prior);save(STATE/'transaction.json',prior)
    guard=next(x for x in prior['files'] if x['path']==str(pathlib.Path(c['library'])/'openflux_node.py'))
    # Independent recovery survives a lost SSH session or a killed updater process.
    run(['systemd-run','--quiet','--unit='+watchdog,'--on-active=180s','/usr/bin/python3',str(pathlib.Path(prior['directory'])/guard['copy']),'recover','--checkpoint',str(pathlib.Path(prior['directory'])/'checkpoint.json')])
    try:
        runtime_action(c,'stop')
        atomic_file(directory/'openflux-linux-amd64',c['binary'])
        for name in support(c).MODULES:
            source = directory/'integration'/name; compile(source.read_text(),name,'exec')
            atomic_file(source,pathlib.Path(c['library'])/name,0o644)
        atomic_file(directory/SELF_ASSET,pathlib.Path(c['library'])/'openflux_node.py')
        atomic_file(directory/'openflux-yandex-cookie-import',HELPER)
        runtime_action(c,'start')
        if c['backend']=='docker':
            actual=run(['docker','exec',c['container'],'sha256sum','/usr/local/bin/openflux']).stdout.split()[0]
            if actual!=sha(c['binary']):raise RuntimeError('Container did not load the new release binary')
        health(c)
        if not prior['active']: runtime_action(c,'stop')
        run(['systemctl','stop',watchdog+'.timer'])
        save(STATE/'rollback.json',prior)
        save(STATE/'installed.json',{'version':data['version'],'sha256':sha(c['binary'])})
        (STATE/'transaction.json').unlink(); (STATE/'staged.json').unlink()
        return {'installed':data['version'],'health':'passed','previous_active_state_restored':True}
    except BaseException:
        restore(prior); run(['systemctl','stop',watchdog+'.timer'],check=False); raise

def write_wrappers(c):
    library = pathlib.Path(c['library'])
    atomic_file(pathlib.Path(__file__).resolve(),library/'openflux_node.py')
    for name,target in [('openfluxctl','openflux_node.py'),('openflux-auth','openflux_auth.py')]:
        p = COMMAND_DIR/name
        p.write_text('#!/bin/sh\nexec /usr/bin/python3 '+shlex.quote(str(library/target))+' "$@"\n'); p.chmod(0o755)
    (UNIT_DIR/'openflux-auth-status.service').write_text('[Unit]\nDescription=OpenFlux authentication status\n[Service]\nType=oneshot\nUMask=0077\nExecStart='+str(COMMAND_DIR/'openflux-auth')+' status --quiet\n')
    (UNIT_DIR/'openflux-auth-status.timer').write_text('[Unit]\nDescription=OpenFlux status poll\n[Timer]\nOnBootSec=30\nOnUnitActiveSec=15\n[Install]\nWantedBy=timers.target\n')
    run(['systemctl','daemon-reload']); run(['systemctl','enable','--now','openflux-auth-status.timer'])

def common_profile(a):
    return {'schema':1,'role':a.role,'backend':a.backend or ('docker' if a.role=='server' else 'systemd'),'binary':a.binary or '/opt/openflux/bin/openflux',
            'library':a.library or '/usr/local/lib/openflux-integration','service':a.service or 'openflux.service',
            'container':a.container or 'openflux','cookie_store':a.cookie_store or '/var/lib/openflux/cookies.json',
            'document_url':a.document_url,'transport':a.transport,'socks':a.socks,'channel':a.channel,
            'args':read(a.args_file,[]) if a.args_file else [],'cookie_user':'openflux' if a.role=='client' else 'root',
            'router_updater':bool(a.router_updater),'owns_runtime':a.action=='install'}

def runtime_script(c):
    directory=RUNTIME_DIR;directory.mkdir(parents=True,exist_ok=True,mode=0o700)
    data='#!/bin/sh\nset -eu\niptables -C OUTPUT -p tcp --tcp-flags RST RST -j DROP 2>/dev/null || iptables -A OUTPUT -p tcp --tcp-flags RST RST -j DROP\nexec /usr/local/bin/openflux '+shlex.join(c['args'])+'\n'
    with tempfile.NamedTemporaryFile('w',dir=directory,delete=False) as f:f.write(data);temp=f.name
    os.chmod(temp,0o700);os.replace(temp,directory/'run.sh')

def setup(a):
    if platform.system()!='Linux' or platform.machine()!='x86_64': raise ValueError('This release supports Linux amd64 with systemd')
    if CONFIG.exists(): raise ValueError('Already configured; use openfluxctl configure or update')
    c = common_profile(a); validate(c)
    if c['role']=='client' and c['backend']=='docker': raise ValueError('Use systemd for a client')
    if a.action=='adopt':
        if not pathlib.Path(c['binary']).is_file(): raise ValueError('Existing binary does not exist')
        if not (pathlib.Path(c['library'])/'openflux_auth.py').is_file(): raise ValueError('Install the release integration support first')
        if c['backend']=='docker':
            inspection = json.loads(run(['docker','inspect',c['container']]).stdout)[0]
            if not any(m.get('Source')==c['binary'] and m.get('Destination')=='/usr/local/bin/openflux' for m in inspection.get('Mounts',[])):
                raise ValueError('Docker adoption requires the release binary mounted at /usr/local/bin/openflux')
        current = identify(c)
        if current['version']=='unknown': raise ValueError('Existing binary is not a published release')
        legacy_recovery=read('/etc/openflux-recovery/config.json',{})
        if legacy_recovery.get('path'):c.update(recovery_path=legacy_recovery['path'],recovery_enabled=True)
        save(CONFIG,c); STATE.mkdir(parents=True,exist_ok=True); save(STATE/'installed.json',current); write_wrappers(c)
        return {'adopted':c['role'],'version':current['version'],'runtime':'unchanged'}
    if c['role']=='server' and c['backend']!='docker': raise ValueError('New servers use Docker to isolate their RST rule; existing systemd services can be adopted')
    if c['transport']=='yandex' and not c['document_url']: raise ValueError('Yandex transport requires --document-url')
    if not c['args']:
        c['args']=['--client' if c['role']=='client' else '--exit-node','--transport',c['transport']]
        if c['transport']=='yandex':c['args']+=['--url',c['document_url'],'--yandex-cookie-store',c['cookie_store']]
        if c['role']=='client':c['args']+=['--socks5',c['socks']]
    if pathlib.Path(c['binary']).exists(): raise ValueError('Binary exists; use adopt to preserve the existing installation')
    store=pathlib.Path(c['cookie_store'])
    if store.parent.exists() and any(store.parent.iterdir()):raise ValueError('Use an empty dedicated cookie directory for a new installation; use adopt for an existing node')
    if c['backend']=='docker':
        run(['docker','version'])
        if run(['docker','inspect',c['container']],check=False).returncode==0:raise ValueError('Container name already exists; use adopt or choose another name')
    elif (UNIT_DIR/c['service']).exists():raise ValueError('Service already exists; use adopt or choose another service name')
    run(['systemctl','--version']); run(['curl','--version'])
    try: import cryptography
    except ImportError: raise ValueError('Install python3-cryptography first') from None
    STATE.mkdir(parents=True,exist_ok=True)
    rows = fetch('https://api.github.com/repos/'+REPO+'/releases?per_page=100',limit=8*1024*1024)
    def key(r):
        m=re.fullmatch(r'v(\d+)\.(\d+)\.(\d+)(?:-rc(\d+))?',r.get('tag_name',''))
        return tuple(map(int,m.group(1,2,3)))+(1 if m[4] is None else 0,int(m[4] or 0)) if m else (-1,)
    rows=[r for r in rows if not r.get('draft') and r.get('published_at') and (a.channel=='prerelease' or not r.get('prerelease')) and key(r)!=(-1,)]
    if not rows: raise ValueError('No supported published release')
    row=max(rows,key=key); directory=pathlib.Path(tempfile.mkdtemp(prefix='initial-',dir=STATE)); verify_download(row,directory)
    # Use the verified release's archive validator, without executing archive files.
    import tarfile
    names={'openflux_release.py','openflux_auth.py','cookie_import.py','recovery_crypto.py','recovery_inbox.py','router_integration.py','install.py','README.md'}
    library=pathlib.Path(c['library']); seen=set(); contents={}
    with tarfile.open(directory/'openflux-integration-linux.tar.gz','r:gz') as tar:
        for item in tar.getmembers():
            name=item.name.removeprefix('integration/')
            if item.name!='integration/'+name or name not in names or name in seen or not item.isfile() or item.size>512*1024: raise ValueError('Invalid integration archive')
            seen.add(name); content=tar.extractfile(item).read()
            if name.endswith('.py'): compile(content,name,'exec')
            contents[name]=content
    if seen!=names: raise ValueError('Incomplete integration archive')
    image='ghcr.io/'+REPO.lower()+':'+row['tag_name']
    if c['backend']=='docker':run(['docker','pull',image],timeout=180)
    library.mkdir(parents=True,exist_ok=True)
    for name,content in contents.items():(library/name).write_bytes(content)
    if c['role']=='client':
        try: pwd.getpwnam('openflux')
        except KeyError: run(['useradd','--system','--no-create-home','--shell','/usr/sbin/nologin','openflux'])
    store.parent.mkdir(parents=True,exist_ok=True,mode=0o700)
    account=pwd.getpwnam(c['cookie_user']); os.chown(store.parent,account.pw_uid,account.pw_gid)
    atomic_file(directory/'openflux-linux-amd64',c['binary'])
    atomic_file(directory/'openflux-yandex-cookie-import',HELPER)
    save(CONFIG,c); write_wrappers(c); atomic_file(directory/SELF_ASSET,library/'openflux_node.py')
    if c['backend']=='systemd':
        unit='[Unit]\nDescription=OpenFlux '+c['role']+'\nAfter=network-online.target\n[Service]\nExecStart='+str(COMMAND_DIR/'openfluxctl')+' run\nRestart=on-failure\nRestartSec=5\nUMask=0077\n[Install]\nWantedBy=multi-user.target\n'
        (UNIT_DIR/c['service']).write_text(unit); run(['systemctl','daemon-reload']); run(['systemctl','enable',c['service']])
    else:
        # RST suppression is confined to this container's network namespace.
        runtime_script(c)
        run(['docker','create','--name',c['container'],'--restart=unless-stopped','--cap-add=NET_RAW','--cap-add=NET_ADMIN',
             '--mount','type=bind,src='+c['binary']+',dst=/usr/local/bin/openflux,readonly',
             '--mount','type=bind,src='+str(store.parent)+',dst='+str(store.parent),
             '--mount','type=bind,src='+str(RUNTIME_DIR)+',dst=/run/openflux-runtime,readonly',
             '--entrypoint','/bin/sh',image,'/run/openflux-runtime/run.sh'])
    save(STATE/'installed.json',{'version':row['tag_name'],'sha256':sha(c['binary'])})
    return {'installed':row['tag_name'],'role':c['role'],'next':'Import cookies if needed, then run openfluxctl start','config':str(CONFIG)}

def main():
    parser=argparse.ArgumentParser(description=__doc__)
    sub=parser.add_subparsers(dest='action',required=True)
    for command in ('install','adopt'):
        p=sub.add_parser(command); p.add_argument('--role',choices=('client','server'),required=True)
        p.add_argument('--backend',choices=('systemd','docker'))
        for field in ('binary','library','service','container','cookie-store','document-url','args-file'): p.add_argument('--'+field)
        p.add_argument('--transport',default='yandex'); p.add_argument('--socks',default='127.0.0.1:1080')
        p.add_argument('--channel',choices=('stable','prerelease'),default='stable'); p.add_argument('--router-updater',action='store_true')
    for command in ('status','check','download','update','rollback','start','stop','restart','run','menu'): sub.add_parser(command)
    p=sub.add_parser('configure'); p.add_argument('--channel',choices=('stable','prerelease')); p.add_argument('--args-file'); p.add_argument('--transport'); p.add_argument('--document-url')
    p=sub.add_parser('cookies'); p.add_argument('operation',choices=('import','package','send')); p.add_argument('--file',default='-'); p.add_argument('--output')
    p=sub.add_parser('recovery'); p.add_argument('--server-public-key'); p.add_argument('--sender-public-key'); p.add_argument('--disk-token-file'); p.add_argument('--disk-path')
    p=sub.add_parser('recover'); p.add_argument('--checkpoint',required=True)
    a=parser.parse_args()
    if os.geteuid()!=0: parser.error('Run with sudo')
    if a.action in ('install','adopt'): result=setup(a)
    elif a.action=='recover': restore(read(a.checkpoint)); result={'restored':True}
    else:
        c=config(); mod=support(c)
        if a.action=='menu':
            choices={'1':['status'],'2':['check'],'3':['update'],'4':['rollback'],'5':['cookies','import'],'6':['recovery'],'7':['restart']}
            while True:
                print('\nOpenFlux — '+c['role']+'\n1. Status\n2. Check releases\n3. Update\n4. Rollback\n5. Import browser cookies\n6. Configure Yandex Disk recovery\n7. Restart OpenFlux\n0. Exit')
                choice=input('Choose: ').strip()
                if choice=='0': return
                if choice not in choices: continue
                argv=choices[choice][:];payload=None
                if choice in ('3','4','7') and input('This can briefly interrupt OpenFlux. Continue? [y/N] ').strip().lower()!='y': continue
                if choice=='5':
                    filename=input('cURL file path, or Enter to paste cookies: ').strip()
                    if filename:argv += ['--file',filename]
                    else:
                        print('Paste Copy as cURL (bash). Finish with END on its own line.')
                        lines=[]
                        while True:
                            line=input()
                            if line=='END':break
                            lines.append(line)
                            if sum(len(x) for x in lines)>131072:raise ValueError('Cookie input too large')
                        payload='\n'.join(lines)
                if choice=='6':
                    token=input('Path to OAuth token file (blank to keep existing): ').strip()
                    peer=input('Path to '+('client signing' if c['role']=='server' else 'server RSA')+' public key (blank to keep existing): ').strip()
                    path=input('Disk path [disk:/OpenFlux Recovery/server-recovery.json]: ').strip()
                    if token: argv += ['--disk-token-file',token]
                    if peer: argv += ['--sender-public-key' if c['role']=='server' else '--server-public-key',peer]
                    if path: argv += ['--disk-path',path]
                subprocess.run([sys.executable,str(pathlib.Path(__file__).resolve())]+argv,input=payload,text=True,check=False)
        elif a.action=='run':
            if c['role']=='client':
                account=pwd.getpwnam(c.get('cookie_user','openflux')); os.setgid(account.pw_gid); os.setuid(account.pw_uid)
            os.execv(c['binary'],[c['binary']]+c['args'])
        elif a.action=='status':
            import openflux_auth
            result={'installed':identify(c),'role':c['role'],'channel':c['channel'],'authentication':openflux_auth.status()}
        elif a.action=='configure':
            if (a.args_file or a.transport or a.document_url) and not c.get('owns_runtime'):
                raise ValueError('This existing runtime is externally configured. Change its service/container arguments using its existing setup; the updater preserves them.')
            for k in ('channel','transport','document_url'):
                if getattr(a,k) is not None: c[k]=getattr(a,k)
            if a.args_file: c['args']=read(a.args_file)
            else:
                for flag,value in (('--transport',a.transport),('--url',a.document_url)):
                    if value is not None:
                        if flag in c['args']:
                            pos=c['args'].index(flag)
                            if pos+1>=len(c['args']):raise ValueError('Invalid existing arguments')
                            c['args'][pos+1]=value
                        else:c['args'] += [flag,value]
            validate(c); save(CONFIG,c)
            if c.get('owns_runtime') and c['backend']=='docker':runtime_script(c)
            result={'saved':str(CONFIG),'restart_required':bool(a.args_file or a.transport or a.document_url)}
        elif a.action in ('start','stop','restart'): runtime_action(c,a.action); result={'action':a.action}
        elif a.action in ('check','download','update','rollback') and c.get('router_updater'):
            mapped={'download':'prepare','update':'install'}.get(a.action,a.action)
            if a.action=='update': run(['/usr/local/sbin/openflux-client-release','download'],timeout=180)
            p=run(['/usr/local/sbin/openflux-client-release',mapped],timeout=180); print(p.stdout); return
        elif a.action=='check': result=candidate(c)
        elif a.action in ('download','update'):
            import fcntl
            with open('/run/openflux-update.lock','a') as lock:
                fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
                result=stage(c)
                if a.action=='update' and not result.get('current'): result=install_staged(c)
        elif a.action=='rollback':
            data=read(STATE/'transaction.json') or read(STATE/'rollback.json')
            if not data: raise ValueError('No rollback checkpoint')
            restore(data); result={'restored':data['installed']}
        elif a.action in ('cookies','recovery'):
            import openflux_auth
            if a.action=='recovery': result=openflux_auth.configure_recovery(vars(a))
            else:
                text=sys.stdin.read(131073) if a.file=='-' else pathlib.Path(a.file).read_text()
                result=openflux_auth.import_cookies(text) if a.operation=='import' else openflux_auth.package_cookies(text,upload=a.operation=='send')
                if a.output and 'package' in result: save(a.output,result['package']); result={'saved':a.output}
    print(json.dumps(result,indent=2))

if __name__=='__main__':
    try: main()
    except Exception as e: print(json.dumps({'ok':False,'error':str(e)}),file=sys.stderr); sys.exit(1)
