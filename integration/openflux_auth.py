#!/usr/bin/env python3
"""OpenFlux authentication status and safe browser-cookie refresh; never exports logs/cookies."""
import argparse, datetime, fcntl, hashlib, importlib.machinery, importlib.util, json, os, pathlib, pwd, re, shlex, subprocess, sys, tempfile, time

NODE_CONFIG = pathlib.Path('/etc/openflux/node.json')
RECOVERY_DIR = pathlib.Path('/etc/openflux-recovery')
try: NODE = json.loads(NODE_CONFIG.read_text())
except FileNotFoundError: NODE = {}
CLIENT = NODE.get('role') == 'client' if NODE else pathlib.Path('/etc/openflux-client/yandex.env').exists()
UNIT = NODE.get('service', 'openflux-yandex-client.service')
CONTAINER = NODE.get('container', 'openflux-yandex-exit')
NATIVE = NODE.get('backend') == 'systemd' if NODE else CLIENT
STATE = pathlib.Path('/var/lib/openflux-auth/status.json')
STORE = pathlib.Path(NODE.get('cookie_store') or ('/var/lib/openflux-client/yandex-cookies.json' if CLIENT else '/var/lib/openflux-yandex/yandex-cookies.json'))
HELPER = '/usr/local/sbin/openflux-yandex-cookie-import'
EVENTS = [
 ('CONNECTING: retry scheduled','connecting','Connection interrupted. Reconnecting…'),
 ('OnlyOffice authentication successful','connected','OnlyOffice authentication succeeded.'),
 ('AUTH_BLOCKED cleared','connecting','Fresh cookies loaded. Reconnecting…'),
 ('AUTH_BLOCKED','auth_blocked','Authentication blocked. Refresh browser cookies to reconnect.'),
 ('CAPTCHA_REQUIRED','auth_blocked','Yandex requires browser verification. Refresh cookies.'),
 ('AUTH_REJECTED:','auth_failed','Server rejected document authentication. Refresh cookies if this persists.'),
 ('AUTH_WAIT_TIMEOUT:','connecting','Authentication handshake timed out. Retrying; cookie expiry is not confirmed.'),
 ('AUTH_HANDSHAKE_INTERRUPTED:','connecting','Authentication handshake was interrupted. Retrying; cookie expiry is not confirmed.'),
 ('Document authentication not accepted','connecting','Authentication handshake did not complete. Retrying; cookie expiry is not confirmed.'),
 ('Authentication failed','auth_failed','Authentication failed. Refresh browser cookies if this persists.'),
 ('Auth response read failed','connecting','Authentication response was interrupted. Retrying; cookie expiry is not confirmed.'),
 ('Read error:','connecting','Connection interrupted. Reconnecting…'),
 ('WebSocket dial failed','connecting','Connection attempt failed. Retrying…'),
 ('fetchDocInfo failed','connecting','Yandex bootstrap failed. Retrying…'),
]

def command(args,timeout=12):
    return subprocess.run(args,capture_output=True,text=True,timeout=timeout)

def save(path,data):
    path.parent.mkdir(mode=0o700,parents=True,exist_ok=True)
    fd,tmp=tempfile.mkstemp(dir=path.parent,prefix='.status-')
    try:
        with os.fdopen(fd,'w') as f:json.dump(data,f);f.flush();os.fsync(f.fileno())
        os.chmod(tmp,0o600);os.replace(tmp,path)
    finally:
        if os.path.exists(tmp):os.unlink(tmp)

def classify(message):
    for needle,state,reason in EVENTS:
        if needle.lower() in message.lower():return state,reason
    return None

def runtime():
    if NATIVE:
        p=command(['systemctl','show',UNIT,'-p','ActiveState,InvocationID'])
        d=dict(s.split('=',1) for s in p.stdout.splitlines() if '=' in s)
        return d.get('ActiveState')=='active',d.get('InvocationID','')
    p=command(['docker','inspect',CONTAINER])
    if p.returncode:return False,''
    d=json.loads(p.stdout)[0]
    return d['State']['Running'],d['Id']+':'+d['State']['StartedAt']

def status():
    with open('/run/openflux-auth.lock','a') as lock:
        os.chmod('/run/openflux-auth.lock',0o600)
        fcntl.flock(lock,fcntl.LOCK_EX)
        return _status()

def snapshot():
    try:return status()
    except Exception:return {'state':'unknown','reason':'Authentication status unavailable. Check the status helper.'}

def _status():
    try:previous=json.loads(STATE.read_text())
    except (OSError,ValueError):previous={}
    failure=previous.get('last_failure') or {}
    if failure.get('reason') in ('OnlyOffice authentication failed. Retrying; refresh cookies if this persists.','Authentication response failed. Retrying…'):
        failure['reason']='Previous authentication handshake did not complete; this did not confirm expired cookies.'
    active,invocation=runtime()
    same=previous.get('invocation')==invocation
    out=dict(previous) if same else {'last_failure':previous.get('last_failure')}
    out.update(active=active,invocation=invocation,checked_at=int(time.time()))
    if not active:
        out.update(state='stopped',reason='OpenFlux is stopped.')
    else:
        if not same:out.update(state='connecting',reason='Connecting to Yandex…')
        events=[]
        if NATIVE and re.fullmatch(r'[0-9a-f]{32}',invocation or ''):
            pattern='|'.join(re.escape(e[0]) for e in EVENTS)+'|persisted cookies|Detected OnlyOffice build'
            p=command(['journalctl','--no-pager','-o','json','-n','80','_SYSTEMD_INVOCATION_ID='+invocation,'--grep',pattern,'--case-sensitive=no'])
            for line in p.stdout.splitlines():
                try:
                    d=json.loads(line);events.append((int(d['__REALTIME_TIMESTAMP'])/1e6,str(d.get('MESSAGE',''))))
                except (ValueError,KeyError):continue
        elif not NATIVE:
            # Read only new log records after the last completed poll. No raw log is returned or saved.
            since=str(max(0,float(previous.get('cursor',0))-1)) if same and previous.get('cursor') else invocation.split(':',1)[1]
            poll_started=time.time()
            p=command(['docker','logs','--timestamps','--since',since,CONTAINER],timeout=20)
            for line in (p.stdout+'\n'+p.stderr).splitlines():
                stamp,_,message=line.partition(' ')
                try:ts=datetime.datetime.fromisoformat(re.sub(r'(\.\d{6})\d+',r'\1',stamp).replace('Z','+00:00')).timestamp()
                except ValueError:continue
                events.append((ts,message))
            out['cursor']=poll_started
        for ts,message in sorted(events):
            if 'persisted cookies' in message:out['persisted_cookies_loaded']=True
            if 'Detected OnlyOffice build:' in message:out['onlyoffice_build_detected']=True
            event=classify(message)
            if not event:continue
            if ts<float(out.get('event_at',0)) and same:continue
            state,reason=event
            out.update(state=state,reason=reason,event_at=ts)
            if state in ('auth_failed','auth_blocked'):
                out['last_failure']={'at':ts,'state':state,'reason':reason}
        out.setdefault('state','connecting');out.setdefault('reason','Connecting to Yandex…')
    out['needs_cookies']=out['state']=='auth_blocked'
    out['cookie_store_present']=STORE.is_file()
    cfg = RECOVERY_DIR
    out['recovery_sender_ready'] = CLIENT and (cfg/'server-public.pem').is_file() and (cfg/'sender-private.pem').is_file()
    out['recovery_upload_ready'] = out['recovery_sender_ready'] and (cfg/'disk-token').is_file()
    if CLIENT:
        try:
            peer=json.loads(pathlib.Path('/var/lib/openflux-recovery/peer-status.json').read_text())
            peer['stale']=bool(peer.get('fetch_failed')) or int(time.time())-peer.get('reported_at',0)>180
            out['server_authentication']=peer
        except (OSError,ValueError):out['server_authentication']={'state':'unknown','stale':True,'reason':'Server status is not configured or has not arrived.'}
    if not CLIENT:
        try:
            recovery=json.loads(pathlib.Path('/var/lib/openflux-recovery/state.json').read_text())
            out['recovery']={k:recovery[k] for k in ['checked_at','inbox','applied_at','needs_attention','next_check','status_published_at','status_publish_error'] if k in recovery}
        except (OSError,ValueError):pass
    save(STATE,out)
    return {k:v for k,v in out.items() if k not in ('invocation','cursor')}

def document_url():
    if NODE.get('document_url'): return NODE['document_url']
    if CLIENT:
        for line in pathlib.Path('/etc/openflux-client/yandex.env').read_text().splitlines():
            if line.startswith('YANDEX_URL='):
                value=shlex.split(line.split('=',1)[1])
                if len(value)==1 and value[0].startswith('https://'):return value[0]
        raise ValueError('Yandex document URL is not configured.')
    data=json.loads(STORE.read_text())
    keys=[x for x in data if x.startswith('https://')]
    if len(keys)!=1:raise ValueError('The server cookie store must identify one Yandex document.')
    return keys[0]

def import_cookies(text):
    if not isinstance(text,str) or not 1<=len(text.encode())<=131072:raise ValueError('Paste a browser Copy as cURL command (maximum 128 KiB).')
    helper_path=HELPER if pathlib.Path(HELPER).exists() else str(pathlib.Path(__file__).with_name('cookie_import.py'))
    loader=importlib.machinery.SourceFileLoader('cookie_import_helper',helper_path)
    spec=importlib.util.spec_from_loader(loader.name,loader);helper=importlib.util.module_from_spec(spec);loader.exec_module(helper)
    try:cookies=helper.parse_cookie_map(helper.extract_cookie_header(text))
    except (SystemExit,ValueError):raise ValueError('No usable cookies found. Use Copy as cURL (bash) on the Yandex document request.') from None
    url=document_url()
    try:current=helper.load_store(str(STORE))
    except (SystemExit,ValueError):raise ValueError('Existing cookie store is invalid; repair it before importing.') from None
    current[url]=cookies
    STORE.parent.mkdir(mode=0o700,parents=True,exist_ok=True)
    if CLIENT:
        account=pwd.getpwnam(NODE.get('cookie_user','openflux'));os.chown(STORE.parent,account.pw_uid,account.pw_gid);os.chmod(STORE.parent,0o700)
        # Atomic replacement must already belong to the service user when visible.
        fd,tmp=tempfile.mkstemp(prefix='.cookie-import-',dir=STORE.parent)
        try:
            os.fchown(fd,account.pw_uid,account.pw_gid)
            with os.fdopen(fd,'w') as f:json.dump(current,f);f.flush();os.fsync(f.fileno())
            os.replace(tmp,STORE)
        finally:
            if os.path.exists(tmp):os.unlink(tmp)
    else:helper.atomic_save(str(STORE),current)
    return {'ok':True,'count':len(cookies),'message':'Cookies saved. A blocked connection will reconnect automatically; no restart is needed.','status':status()}

def configure_recovery(options):
    from cryptography.hazmat.primitives import serialization
    from cryptography.hazmat.primitives.asymmetric import ed25519, rsa
    cfg=RECOVERY_DIR;cfg.mkdir(parents=True,exist_ok=True,mode=0o700);os.chmod(cfg,0o700)
    def key_file(name,data):
        fd,temp=tempfile.mkstemp(dir=cfg)
        try:
            with os.fdopen(fd,'wb') as f:f.write(data)
            os.chmod(temp,0o600);os.replace(temp,cfg/name)
        finally:
            if os.path.exists(temp):os.unlink(temp)
    if options.get('disk_token_file'):
        token=pathlib.Path(options['disk_token_file']).read_text().strip()
        if not token or '\n' in token:raise ValueError('Invalid OAuth token')
        key_file('disk-token',token.encode())
    if CLIENT:
        private=cfg/'sender-private.pem'
        if not private.exists():
            signer=ed25519.Ed25519PrivateKey.generate()
            key_file(private.name,signer.private_bytes(serialization.Encoding.PEM,serialization.PrivateFormat.PKCS8,serialization.NoEncryption()))
        signer=serialization.load_pem_private_key(private.read_bytes(),password=None)
        if not isinstance(signer,ed25519.Ed25519PrivateKey):raise ValueError('Invalid signing key')
        key_file('sender-public.pem',signer.public_key().public_bytes(serialization.Encoding.PEM,serialization.PublicFormat.SubjectPublicKeyInfo))
        if options.get('server_public_key'):
            data=pathlib.Path(options['server_public_key']).read_bytes();key=serialization.load_pem_public_key(data)
            if not isinstance(key,rsa.RSAPublicKey) or key.key_size<3072:raise ValueError('Expected RSA3072+ server public key')
            key_file('server-public.pem',data)
    else:
        private=cfg/'server-private.pem'
        if not private.exists():
            key=rsa.generate_private_key(public_exponent=65537,key_size=3072)
            key_file(private.name,key.private_bytes(serialization.Encoding.PEM,serialization.PrivateFormat.PKCS8,serialization.NoEncryption()))
        key=serialization.load_pem_private_key(private.read_bytes(),password=None)
        key_file('server-public.pem',key.public_key().public_bytes(serialization.Encoding.PEM,serialization.PublicFormat.SubjectPublicKeyInfo))
        if options.get('sender_public_key'):
            data=pathlib.Path(options['sender_public_key']).read_bytes();key=serialization.load_pem_public_key(data)
            if not isinstance(key,ed25519.Ed25519PublicKey):raise ValueError('Expected client Ed25519 public key')
            trusted=cfg/'trusted-senders';trusted.mkdir(exist_ok=True,mode=0o700)
            name='trusted-senders/'+hashlib.sha256(data).hexdigest()+'.pem';key_file(name,data)
    try:legacy=json.loads((cfg/'config.json').read_text())
    except FileNotFoundError:legacy={}
    disk_path=options.get('disk_path') or NODE.get('recovery_path') or legacy.get('path') or 'disk:/OpenFlux Recovery/server-recovery.json'
    if disk_path and not disk_path.startswith(('disk:/','app:/')):raise ValueError('Use a disk:/ or app:/ path')
    if NODE:
        new=dict(NODE)
        if disk_path:new['recovery_path']=disk_path
        new['recovery_enabled']=True;save(NODE_CONFIG,new);NODE.update(new)
    else:
        try:new=json.loads((cfg/'config.json').read_text())
        except FileNotFoundError:new={}
        if disk_path:new['path']=disk_path
        save(cfg/'config.json',new)
    if (cfg/'disk-token').exists():
        library=str(pathlib.Path(__file__).parent/'recovery_inbox.py')
        pathlib.Path('/etc/systemd/system/openflux-recovery-inbox.service').write_text('[Unit]\nDescription=OpenFlux cookie recovery\nAfter=network-online.target\n[Service]\nType=oneshot\nUMask=0077\nTimeoutStartSec=150\nExecStart=/usr/bin/python3 '+library+'\n')
        pathlib.Path('/etc/systemd/system/openflux-recovery-inbox.timer').write_text('[Unit]\nDescription=OpenFlux recovery inbox\n[Timer]\nOnBootSec=30\nOnUnitActiveSec=60\n[Install]\nWantedBy=timers.target\n')
        command(['systemctl','daemon-reload']);p=command(['systemctl','enable','--now','openflux-recovery-inbox.timer'])
        if p.returncode:raise ValueError('Could not enable the recovery timer')
    return {'configured':True,'public_key_file':str(cfg/('sender-public.pem' if CLIENT else 'server-public.pem')),'disk_path':NODE.get('recovery_path') or disk_path,'sender_ready':CLIENT and (cfg/'server-public.pem').exists()}

def package_cookies(text,upload=False):
    from cryptography.hazmat.primitives import serialization
    import cookie_import,recovery_crypto
    if not CLIENT:raise ValueError('Create the recovery packet on the client, or import cookies directly on the server')
    if not isinstance(text,str) or not 1<=len(text.encode())<=131072:raise ValueError('Paste Copy as cURL (bash), maximum 128 KiB')
    try:cookie_import.parse_cookie_map(cookie_import.extract_cookie_header(text))
    except (SystemExit,ValueError):raise ValueError('No usable cookies in the supplied cURL') from None
    cfg=RECOVERY_DIR
    try:
        public=serialization.load_pem_public_key((cfg/'server-public.pem').read_bytes())
        signer=serialization.load_pem_private_key((cfg/'sender-private.pem').read_bytes(),password=None)
    except FileNotFoundError:raise ValueError('Pair this client with the server using openfluxctl recovery first') from None
    envelope=recovery_crypto.seal(text,public,signer)
    disk_path=NODE.get('recovery_path') or 'disk:/OpenFlux Recovery/server-recovery.json'
    if upload:
        import recovery_inbox
        token=(cfg/'disk-token').read_text().strip()
        recovery_inbox.upload(disk_path,envelope,token)
        return {'ok':True,'message':'Encrypted cookies uploaded. The server will check the inbox within about one minute.','package_id':envelope['id']}
    return {'ok':True,'message':'Encrypted file ready. Upload it to the configured Disk folder and replace the previous file.','filename':disk_path.rsplit('/',1)[-1],'package':envelope}

def post(handler):
    if handler.path not in ('/api/openflux/cookies','/api/openflux/recovery'):return False
    if not handler.allowed() or not handler.authorized():handler.j({'ok':False,'error':'Request rejected'},403);return True
    try:
        length=int(handler.headers.get('Content-Length','0'))
        if not 1<=length<=140000:raise ValueError('Input is too large.')
        if 'application/json' not in handler.headers.get('Content-Type',''):raise ValueError('JSON input required.')
        body=json.loads(handler.rfile.read(length))
        result=import_cookies(body.get('curl')) if handler.path=='/api/openflux/cookies' else package_cookies(body.get('curl'),upload=body.get('upload') is True)
        handler.j(result)
    except (ValueError,TypeError,AttributeError):handler.j({'ok':False,'error':'Check the browser cURL and client/server recovery pairing. The Disk token needs write access for automatic upload.'},400)
    except Exception:handler.j({'ok':False,'error':'Cookie import failed. Check the installed helper and store permissions.'},500)
    return True

if __name__=='__main__':
    parser=argparse.ArgumentParser();parser.add_argument('action',choices=['status','import']);parser.add_argument('--quiet',action='store_true');args=parser.parse_args()
    try:
        result=status() if args.action=='status' else import_cookies(sys.stdin.read(131073))
        if not args.quiet:print(json.dumps(result))
    except Exception:
        print(json.dumps({'ok':False,'error':'OpenFlux authentication operation failed. Check configuration and permissions.'}));sys.exit(1)
