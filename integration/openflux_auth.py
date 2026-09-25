#!/usr/bin/env python3
"""OpenFlux authentication status and safe browser-cookie refresh; never exports logs/cookies."""
import argparse, datetime, fcntl, hashlib, importlib.machinery, importlib.util, json, os, pathlib, pwd, re, shlex, subprocess, sys, tempfile, time

CLIENT = pathlib.Path('/etc/openflux-client/yandex.env').exists()
UNIT = 'openflux-yandex-client.service'
CONTAINER = 'openflux-yandex-exit'
STATE = pathlib.Path('/var/lib/openflux-auth/status.json')
STORE = pathlib.Path('/var/lib/openflux-client/yandex-cookies.json' if CLIENT else '/var/lib/openflux-yandex/yandex-cookies.json')
HELPER = '/usr/local/sbin/openflux-yandex-cookie-import'
EVENTS = [
 ('CONNECTING: retry scheduled','connecting','Connection interrupted. Reconnecting…'),
 ('OnlyOffice authentication successful','connected','OnlyOffice authentication succeeded.'),
 ('AUTH_BLOCKED cleared','connecting','Fresh cookies loaded. Reconnecting…'),
 ('AUTH_BLOCKED','auth_blocked','Authentication blocked. Refresh browser cookies to reconnect.'),
 ('CAPTCHA_REQUIRED','auth_blocked','Yandex requires browser verification. Refresh cookies.'),
 ('Document authentication not accepted','auth_failed','OnlyOffice authentication failed. Retrying; refresh cookies if this persists.'),
 ('Authentication failed','auth_failed','Authentication failed. Refresh browser cookies if this persists.'),
 ('Auth response read failed','auth_failed','Authentication response failed. Retrying…'),
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
    if CLIENT:
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
    active,invocation=runtime()
    same=previous.get('invocation')==invocation
    out=dict(previous) if same else {'last_failure':previous.get('last_failure')}
    out.update(active=active,invocation=invocation,checked_at=int(time.time()))
    if not active:
        out.update(state='stopped',reason='OpenFlux is stopped.')
    else:
        if not same:out.update(state='connecting',reason='Connecting to Yandex…')
        events=[]
        if CLIENT and re.fullmatch(r'[0-9a-f]{32}',invocation or ''):
            pattern='|'.join(re.escape(e[0]) for e in EVENTS)+'|persisted cookies|Detected OnlyOffice build'
            p=command(['journalctl','--no-pager','-o','json','-n','80','_SYSTEMD_INVOCATION_ID='+invocation,'--grep',pattern,'--case-sensitive=no'])
            for line in p.stdout.splitlines():
                try:
                    d=json.loads(line);events.append((int(d['__REALTIME_TIMESTAMP'])/1e6,str(d.get('MESSAGE',''))))
                except (ValueError,KeyError):continue
        elif not CLIENT:
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
    if not CLIENT:
        try:
            recovery=json.loads(pathlib.Path('/var/lib/openflux-recovery/state.json').read_text())
            out['recovery']={k:recovery[k] for k in ['checked_at','inbox','applied_at','needs_attention','next_check'] if k in recovery}
        except (OSError,ValueError):pass
    save(STATE,out)
    return {k:v for k,v in out.items() if k not in ('invocation','cursor')}

def document_url():
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
        account=pwd.getpwnam('openflux');os.chown(STORE.parent,account.pw_uid,account.pw_gid);os.chmod(STORE.parent,0o700)
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

def post(handler):
    if handler.path!='/api/openflux/cookies':return False
    if not handler.allowed() or not handler.authorized():handler.j({'ok':False,'error':'Request rejected'},403);return True
    try:
        length=int(handler.headers.get('Content-Length','0'))
        if not 1<=length<=140000:raise ValueError('Input is too large.')
        if 'application/json' not in handler.headers.get('Content-Type',''):raise ValueError('JSON input required.')
        body=json.loads(handler.rfile.read(length));result=import_cookies(body.get('curl'))
        handler.j(result)
    except (ValueError,TypeError,AttributeError):handler.j({'ok':False,'error':'Paste a valid Copy as cURL (bash) command containing browser cookies.'},400)
    except Exception:handler.j({'ok':False,'error':'Cookie import failed. Check the installed helper and store permissions.'},500)
    return True

if __name__=='__main__':
    parser=argparse.ArgumentParser();parser.add_argument('action',choices=['status','import']);parser.add_argument('--quiet',action='store_true');args=parser.parse_args()
    try:
        result=status() if args.action=='status' else import_cookies(sys.stdin.read(131073))
        if not args.quiet:print(json.dumps(result))
    except Exception:
        print(json.dumps({'ok':False,'error':'OpenFlux authentication operation failed. Check configuration and permissions.'}));sys.exit(1)
