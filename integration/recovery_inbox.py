#!/usr/bin/env python3
"""The OpenFlux server pulls a signed cookie-only envelope from a configured Disk path."""
import fcntl,hashlib,json,os,pathlib,sys,time,urllib.parse,urllib.request
from cryptography.hazmat.primitives import serialization
import recovery_crypto
import openflux_auth

CONFIG=pathlib.Path('/etc/openflux-recovery')
STATE=pathlib.Path('/var/lib/openflux-recovery/state.json')
# The timers still fire every minute; these pauses decide when a run touches Disk.
# A server checks its inbox every 2 min (one metadata request unless the file
# changed) and publishes its status on a change or every 10 min. A client
# fetches the server status every 5 min and treats one older than
# recovery_crypto.STATUS_STALE_AFTER as unknown. Before, every node called Disk
# every minute.
SERVER_POLL_EVERY=120
CLIENT_POLL_EVERY=300
STATUS_HEARTBEAT=600
class DiskCaptcha(Exception):pass

def status_signature(report,keys):
    """A digest of the fields a client shows; the timestamps are left out."""
    keep={k:report.get(k) for k in keys}
    return hashlib.sha256(json.dumps(keep,sort_keys=True,default=str).encode()).hexdigest()

def publish_due(state,signature,now):
    return state.get('status_signature')!=signature or now-int(state.get('status_published_at') or 0)>=STATUS_HEARTBEAT

def disk_md5(path,token):
    """One metadata request for the file's md5; a missing file raises HTTPError 404."""
    query=urllib.parse.urlencode({'path':path,'fields':'md5'})
    return fetch('https://cloud-api.yandex.net/v1/disk/resources?'+query,limit=4000,token=token).get('md5') or ''

LEGACY_STATUS_KEYS=('state','reason','needs_cookies','active','cookie_store_present','recovery')

def legacy_status_view(status):
    """What a client shows of the server status; the recovery part without its clocks."""
    view={k:status.get(k) for k in LEGACY_STATUS_KEYS}
    view['recovery']={k:(status.get('recovery') or {}).get(k) for k in ('inbox','applied_at','needs_attention')}
    return view

def upload(path,envelope,token):
    query=urllib.parse.urlencode({'path':path,'overwrite':'true'})
    link=fetch('https://cloud-api.yandex.net/v1/disk/resources/upload?'+query,token=token)
    url=link.get('href','');parsed=urllib.parse.urlparse(url)
    if parsed.scheme!='https' or not any((parsed.hostname or '').endswith(s) for s in ('.yandex.net','.yandex.ru','.yandex.com')):raise ValueError('Unexpected upload URL')
    class NoRedirect(urllib.request.HTTPRedirectHandler):
        def redirect_request(self,*args,**kwargs):raise ValueError('Unexpected upload redirect')
    data=json.dumps(envelope).encode()
    req=urllib.request.Request(url,data=data,method='PUT',headers={'Content-Type':'application/json'})
    with urllib.request.build_opener(urllib.request.ProxyHandler({}),NoRedirect()).open(req,timeout=30) as response:
        if response.status not in (200,201,202,204):raise ValueError('Disk did not accept the upload')

def fetch(url,limit=230000,token=None):
    parsed=urllib.parse.urlparse(url)
    if parsed.scheme!='https' or not (parsed.hostname=='cloud-api.yandex.net' or parsed.hostname.endswith('.yandex.net') or parsed.hostname.endswith('.yandex.ru') or parsed.hostname.endswith('.yandex.com')):raise ValueError('Unexpected Yandex download URL')
    class Redirect(urllib.request.HTTPRedirectHandler):
        def redirect_request(self,req,fp,code,msg,headers,newurl):
            host=urllib.parse.urlparse(newurl)
            if host.scheme!='https' or not any((host.hostname or '').endswith(x) for x in ('.yandex.net','.yandex.ru','.yandex.com')):raise ValueError('Unexpected redirect')
            redirected=super().redirect_request(req,fp,code,msg,headers,newurl)
            if redirected:redirected.remove_header('Authorization')
            return redirected
    opener=urllib.request.build_opener(urllib.request.ProxyHandler({}),Redirect())
    headers={'User-Agent':'OpenFluxRecovery/1','Accept':'application/json'}
    if token:
        if parsed.hostname!='cloud-api.yandex.net':raise ValueError('Refusing to send OAuth outside the API host')
        headers['Authorization']='OAuth '+token
    with opener.open(urllib.request.Request(url,headers=headers),timeout=20) as response:
        raw=response.read(limit+1)
        if len(raw)>limit:raise ValueError('Recovery download too large')
        try:return json.loads(raw)
        except ValueError:
            if b'captcha' in raw.lower():raise DiskCaptcha() from None
            raise

def main():
    with open('/run/openflux-recovery.lock','a') as lock:
        os.chmod('/run/openflux-recovery.lock',0o600)
        try:fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
        except BlockingIOError:return
        config={'path':openflux_auth.NODE['recovery_path']} if openflux_auth.NODE.get('recovery_path') else json.loads((CONFIG/'config.json').read_text())
        if openflux_auth.CLIENT:
            path=config['path']+'.status.json';peer_path=STATE.with_name('peer-status.json')
            try:previous=json.loads(peer_path.read_text())
            except (OSError,ValueError):previous={}
            if int(time.time())-int(previous.get('checked_at') or 0)<CLIENT_POLL_EVERY:return
            try:
                token=(CONFIG/'disk-token').read_text().strip()
                link=fetch('https://cloud-api.yandex.net/v1/disk/resources/download?'+urllib.parse.urlencode({'path':path}),token=token)
                public=serialization.load_pem_public_key((CONFIG/'server-public.pem').read_bytes())
                peer=recovery_crypto.verify_status(fetch(link['href'],limit=16000),public)
                if peer['reported_at']<previous.get('reported_at',0):raise ValueError('Older server status')
                peer.update(checked_at=int(time.time()),fetch_failed=False);openflux_auth.save(peer_path,peer)
            except Exception:
                previous=previous or {'state':'unknown','reported_at':0}
                previous.update(checked_at=int(time.time()),stale=True,fetch_failed=True,reason='Server status could not be verified or downloaded. Check Disk access; this is not a client authentication failure.')
                openflux_auth.save(peer_path,previous)
            return
        try:state=json.loads(STATE.read_text())
        except (OSError,ValueError):state={'seen':[]}
        now=int(time.time())
        # Status publication is independent of cookie inbox retries.
        try:
            token=(CONFIG/'disk-token').read_text().strip()
            status=openflux_auth.status()
            signature=status_signature(legacy_status_view(status),LEGACY_STATUS_KEYS)
            if publish_due(state,signature,now):
                private=serialization.load_pem_private_key((CONFIG/'server-private.pem').read_bytes(),password=None)
                upload(config['path']+'.status.json',recovery_crypto.sign_status(status,private),token)
                state.update(status_published_at=now,status_publish_error=None,status_signature=signature)
        except urllib.error.HTTPError as e:
            state['status_publish_error']='Disk write access is required for server status' if e.code in (401,403) else 'Server status upload unavailable'
        except Exception:state['status_publish_error']='Server status upload unavailable'
        if now<state.get('next_check',0) or now-int(state.get('checked_at') or 0)<SERVER_POLL_EVERY:
            openflux_auth.save(STATE,state);return
        state['checked_at']=now
        attention=state.get('needs_attention',False)
        state['needs_attention']=False
        try:
            token_path=CONFIG/'disk-token'
            if token_path.exists():
                token=token_path.read_text().strip()
                md5=disk_md5(config['path'],token)
                if md5 and md5==state.get('inbox_md5'):
                    envelope=None
                else:
                    query=urllib.parse.urlencode({'path':config['path']})
                    link=fetch('https://cloud-api.yandex.net/v1/disk/resources/download?'+query,token=token)
                    envelope=fetch(link['href']);state['inbox_md5']=md5
            else:
                query=urllib.parse.urlencode({'public_key':config['folder'],'path':'/'+config.get('path','aws-recovery.json').rsplit('/',1)[-1]})
                link=fetch('https://cloud-api.yandex.net/v1/disk/public/resources/download?'+query)
                envelope=fetch(link['href'])
            if envelope is None:
                # The file has not changed since it was last downloaded: that outcome stands.
                state['needs_attention']=attention
            elif envelope.get('id') in state.get('seen',[]):
                state.update(inbox='Already applied',failures=0);openflux_auth.save(STATE,state);return
            else:
                private=serialization.load_pem_private_key((CONFIG/'server-private.pem').read_bytes(),password=None)
                trusted=list((CONFIG/'trusted-senders').glob('*.pem'))
                if (CONFIG/'sender-public.pem').exists():trusted.append(CONFIG/'sender-public.pem')
                payload=None
                for key in trusted:
                    public=serialization.load_pem_public_key(key.read_bytes())
                    try:payload=recovery_crypto.unseal(envelope,private,public,state.get('seen',[]));break
                    except Exception:continue
                if payload is None:raise ValueError('No trusted sender accepted this package')
                curl,ident=payload
                result=openflux_auth.import_cookies(curl)
                state['seen']=(state.get('seen',[])+[ident])[-256:]
                state.update(inbox='Applied',applied_at=int(time.time()),package_id=ident,cookie_count=result['count'])
                state['failures']=0
        except urllib.error.HTTPError as e:
            state['inbox']='Waiting for recovery file' if e.code==404 else 'DISK_AUTH_REQUIRED' if e.code in (401,403) else 'Yandex Disk temporarily unavailable'
            if e.code in (401,403):state['needs_attention']=True
        except DiskCaptcha:state.update(inbox='DISK_CAPTCHA_REQUIRED',needs_attention=True)
        except Exception:state.update(inbox='Package rejected or download unavailable; check file, signature and expiry',needs_attention=True)
        if state.get('needs_attention'):
            state['failures']=state.get('failures',0)+1;state['next_check']=int(time.time())+min(900,60*2**min(state['failures'],4))
        else:state['next_check']=0
        openflux_auth.save(STATE,state)

if __name__=='__main__':main()
