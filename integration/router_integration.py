"""Idempotent hooks for existing Router Panel updater deployments."""

COOKIE_HTML='''
  <div class="small" id="openfluxLastFailure" style="margin-top:8px"></div>
  <details id="openfluxCookies" style="margin-top:14px">
   <summary style="cursor:pointer">Update Yandex cookies</summary>
   <p class="small">Open your public Yandex document in Chrome/Edge and complete any verification. Press F12 → Network, reload the document, right-click its request → Copy → Copy as cURL (bash). Paste below. Cookies stay on this Mini-PC.</p>
   <textarea id="openfluxCurl" rows="5" autocomplete="off" spellcheck="false" placeholder="Paste Copy as cURL (bash)…" style="width:100%;box-sizing:border-box"></textarea>
   <button id="openfluxCookieImport" style="margin-top:10px">Save cookies and reconnect</button>
   <p class="small" id="openfluxCookieResult" role="status" aria-live="polite">No restart or reinstall required.</p>
  </details>'''

COOKIE_JS='''
$('#openfluxCookieImport').onclick=async()=>{
 const button=$('#openfluxCookieImport'), field=$('#openfluxCurl'), result=$('#openfluxCookieResult');
 button.disabled=true;result.textContent='Saving cookies…';
 try{const d=await api('/api/openflux/cookies',{method:'POST',headers:{'Content-Type':'application/json','X-Router-Panel':'1'},body:JSON.stringify({curl:field.value})});field.value='';result.textContent=d.message;refresh();}
 catch(e){result.textContent=e.message;}finally{button.disabled=false;}
};
'''

RECOVERY_HTML='''
  <div class="small" id="openfluxServerState" style="margin-top:12px">Server authentication: checking…</div>
  <div class="small" id="openfluxServerFailure"></div>
  <details id="openfluxServerRecovery" style="margin-top:14px">
   <summary style="cursor:pointer">Server cookies via Yandex Disk</summary>
   <p class="small">Paste Copy as cURL above. Create an encrypted recovery file and upload it to your configured Disk folder, replacing the previous file. This runs on the client; no Windows application is needed. Pairing and the Disk path are configured with openfluxctl recovery.</p>
   <button id="openfluxRecoveryDownload">Download encrypted file</button>
   <button id="openfluxRecoverySend">Upload through Disk API</button>
   <p class="small" id="openfluxRecoveryResult" role="status" aria-live="polite"></p>
  </details>'''

RECOVERY_JS='''
async function openfluxRecovery(upload){
 const result=$('#openfluxRecoveryResult');result.textContent='Preparing server cookies…';
 try{
  const d=await api('/api/openflux/recovery',{method:'POST',headers:{'Content-Type':'application/json','X-Router-Panel':'1'},body:JSON.stringify({curl:$('#openfluxCurl').value,upload})});
  if(d.package){const blob=new Blob([JSON.stringify(d.package)],{type:'application/json'}),url=URL.createObjectURL(blob),a=document.createElement('a');a.href=url;a.download=d.filename;a.click();setTimeout(()=>URL.revokeObjectURL(url),1000);}
  $('#openfluxCurl').value='';result.textContent=d.message;
 }catch(e){result.textContent=e.message;}
}
$('#openfluxRecoveryDownload').onclick=()=>openfluxRecovery(false);
$('#openfluxRecoverySend').onclick=()=>openfluxRecovery(true);
'''

def patch_recovery(text):
    if 'id="openfluxServerRecovery"' in text:return text
    text=replace(text,COOKIE_HTML,COOKIE_HTML+RECOVERY_HTML)
    text=replace(text,COOKIE_JS,COOKIE_JS+RECOVERY_JS)
    marker="$('#openfluxLastFailure').textContent=of.last_failure?'Last authentication issue: '+new Date(of.last_failure.at*1000).toLocaleString()+' — '+of.last_failure.reason:'';"
    text=replace(text,marker,marker+"\n  $('#openfluxRecoveryDownload').disabled=!of.recovery_sender_ready;\n  $('#openfluxRecoverySend').disabled=!of.recovery_upload_ready;\n  const peer=of.server_authentication||{}, ps=peer.status||{};\n  $('#openfluxServerState').textContent='Server authentication: '+(peer.stale?'Unknown — no fresh verified response':({connected:'Connected',connecting:'Connecting',auth_blocked:'AUTH_BLOCKED — update server cookies',auth_failed:'Authentication failed on server',stopped:'Stopped'})[peer.state]||'Unknown')+(peer.reported_at?' · Last response: '+new Date(peer.reported_at*1000).toLocaleString():'');\n  $('#openfluxServerFailure').textContent=ps.last_failure?'Last server authentication issue: '+new Date(ps.last_failure.at*1000).toLocaleString()+' — '+ps.last_failure.reason:'';")
    text=text.replace("'Last authentication issue: '","'Last client authentication issue: '")
    return text

def replace(text,old,new):
    if text.count(old)!=1:raise ValueError('Patch context count '+str(text.count(old))+': '+old[:80])
    return text.replace(old,new)

def patch_core(text):
    if 'import openflux_release' in text:return text.replace("candidate['same_version'] = current['sha256'] == candidate['asset_sha256']","candidate['same_version'] = openflux_release.is_current(current, candidate) if name == 'openflux' else current['sha256'] == candidate['asset_sha256']")
    text=replace(text,'from transport import Transport','from transport import Transport\nimport openflux_release')
    text=replace(text,"candidate['same_version'] = current['sha256'] == candidate['asset_sha256']","candidate['same_version'] = openflux_release.is_current(current, candidate) if name == 'openflux' else current['sha256'] == candidate['asset_sha256']")
    text=replace(text,'def release(name, transport):\n','def release(name, transport):\n    if name == \'openflux\': return openflux_release.release(sys.modules[__name__], transport)\n')
    text=replace(text,'def _prepare_binary(name, route):\n','def _prepare_binary(name, route):\n    if name == \'openflux\': return openflux_release.prepare(sys.modules[__name__], route)\n')
    text=replace(text,'def replace_component(name, source, metadata):\n','def replace_component(name, source, metadata):\n    if name == \'openflux\': return openflux_release.replace(sys.modules[__name__], source, metadata)\n')
    return text

def patch_panel(text):
    if 'import openflux_auth' in text:return patch_volga_panel(patch_config_panel(patch_recovery(text)))
    text=replace(text,'import panel_updates as updater_ui','import panel_updates as updater_ui\nimport openflux_auth')
    text=replace(text,'"openflux":{"client_active":','"openflux":{**openflux_auth.snapshot(), "client_active":')
    text=replace(text,'    def do_POST(self):\n','    def do_POST(self):\n        if openflux_auth.post(self):return\n')
    health='<div class="small" id="openfluxHealth" style="margin-top:8px"></div>'
    text=replace(text,health,health+COOKIE_HTML)
    old="$('#openfluxState').textContent=ofReady?'Ready':(of.client_active||of.bridge_active||s.interfaces?.oflux0?'Partial':'Stopped');"
    text=replace(text,old,"$('#openfluxState').textContent=({connected:'Connected',connecting:'Connecting',auth_blocked:'AUTH_BLOCKED — update cookies',auth_failed:'Authentication failed',stopped:'Stopped',unknown:'Unknown'})[of.state]||'Checking';")
    old="dot('#openfluxDot',s.mode==='openflux'?(s.health?.level||'checking'):(ofReady?'warn':'bad'));"
    text=replace(text,old,"dot('#openfluxDot',of.state==='connected'?'ok':of.state==='connecting'?'warn':'bad');")
    text=replace(text,"$('#openfluxHealth').textContent=s.mode==='openflux'?(s.health?.summary||''):'';", "$('#openfluxHealth').textContent=(of.reason||'')+(s.mode==='openflux'?' · '+(s.health?.summary||''):'');\n  $('#openfluxLastFailure').textContent=of.last_failure?'Last authentication issue: '+new Date(of.last_failure.at*1000).toLocaleString()+' — '+of.last_failure.reason:'';")
    text=replace(text,'refreshNetwork();setInterval(refreshNetwork,5000);',COOKIE_JS+'\nrefreshNetwork();setInterval(refreshNetwork,5000);')
    return patch_volga_panel(patch_config_panel(patch_recovery(text)))


def patch_config_panel(text):
    # Existing panels provide the authenticated download/upload API. Extend its
    # registry instead of installing a second configuration UI or API.
    if 'import panel_extra' not in text:return text
    hook='from router_integration import extend_config_store\nimport config_store\nextend_config_store(config_store)'
    if hook not in text:text=replace(text,'import panel_extra','import panel_extra\n'+hook)
    old='<div class="small" style="overflow-wrap:anywhere">${esc(c.path)}</div>'
    description='<div class="small">${esc(c.description||\'\')}</div>'
    if description not in text:text=replace(text,old,old+description)
    text=text.replace('accept=".json,.yaml,.yml,.conf,.env,.txt"','accept=".json,.yaml,.yml,.conf,.env,.txt,.pem"')
    return text


class OpenFluxConfigs:
    """Validated files for the existing panel; upload never restarts services."""
    def __init__(self, core, root=None):
        from pathlib import Path
        self.core=core;self.root=Path(root or '/')
        self.original_registry=core.registry;self.original_upload=core.upload

    def path(self, name):return self.root/name.lstrip('/')

    def registry(self):
        rows=self.original_registry()
        descriptions={
            'openflux-bridge':'TUN/SOCKS bridge and DNS routing. Restart OpenFlux after changing.',
            'openflux-url':'Yandex document URL. Also updates the OpenFlux profile. Restart OpenFlux after changing.',
            'openflux-runtime':'Transport, compression, buffers and batching. Restart OpenFlux after changing.'}
        extra={
            'openflux-volga-config':('/etc/openflux-volga/config.json','Volga configuration','Volga document links and shared key. Separate from Legacy; the measured performance profile is fixed. Restart Volga after changing.'),
            'openflux-mailru':(MAILRU_ENV,'Mail.ru document','Public Mail.ru Cloud document link, MAILRU_DOCUMENT=https://cloud.mail.ru/public/…; no cookies. The server’s Mail.ru exit must use the same link. Apply Mail.ru again (or restart OPENFLUX) after changing.'),
            'openflux-node':('/etc/openflux/node.json','Node and Disk settings','Document URL, Disk recovery path and release channel. Managed service paths stay unchanged.'),
            'openflux-updater':('/etc/router/updater/settings.json','Update settings','Allow prereleases and expected exit IP. Settings of other components stay unchanged.'),
            'openflux-disk-token':('/etc/openflux-recovery/disk-token','Disk OAuth token','Secret API token for cookies and status. Applied on the next Disk request; no restart.'),
            'openflux-server-key':('/etc/openflux-recovery/server-public.pem','Server public key','Encrypts server cookies and verifies server status. Replace when pairing with a different server.'),
            'openflux-client-key':('/etc/openflux-recovery/sender-private.pem','Client private signing key','Secret client identity. Import derives the matching public key; the server must trust that public key.'),
            'openflux-client-public':('/etc/openflux-recovery/sender-public.pem','Client public signing key','Give this key to the server. An uploaded public key must match this client’s private key.'),
            'openflux-cookies':('/var/lib/openflux-client/yandex-cookies.json','Client cookies','Secret saved browser cookies for this client. AUTH_BLOCKED reconnects after replacement; not sent to the server.')}
        for key,(name,label,description) in extra.items():
            path=self.path(name)
            if path.is_file():rows[key]={'path':str(path),'label':'OpenFlux · '+label,'kind':key,'file':'disk-token.txt' if key=='openflux-disk-token' else path.name,'description':description}
        for key,description in descriptions.items():
            if key in rows:rows[key]['description']=description
        return rows

    def plan(self, target, data, read):
        import json,re,shlex,ipaddress
        from urllib.parse import urlsplit
        from cryptography.hazmat.primitives import serialization
        from cryptography.hazmat.primitives.asymmetric import ed25519,rsa
        rows=self.registry();path=__import__('pathlib').Path(rows[target]['path'])
        text=self.core.parse(data);writes={path:data}
        node_path=self.path('/etc/openflux/node.json')
        settings_path=self.path('/etc/router/updater/settings.json')
        def encoded(obj):return (json.dumps(obj,indent=2)+'\n').encode()
        def url(value):
            if not isinstance(value,str) or any(c.isspace() for c in value):raise ValueError('Invalid Yandex document URL')
            parsed=urlsplit(value)
            if parsed.scheme!='https' or parsed.hostname not in ('disk.yandex.ru','disk.yandex.com','telemost.yandex.ru') or parsed.username or parsed.password:raise ValueError('Use an HTTPS Yandex document URL')
            return value
        def disk(value):
            if not isinstance(value,str) or not value.startswith(('disk:/','app:/')) or value.endswith('/') or any(ord(c)<32 for c in value) or '..' in value.split('/'):raise ValueError('Use a disk:/ or app:/ path including the recovery filename')
        def change_url(node,value):
            node['document_url']=url(value)
            args=node.get('args',[])[:]
            if '--url' in args:
                i=args.index('--url')
                if i+1>=len(args):raise ValueError('Invalid existing URL arguments')
                args[i+1]=value;node['args']=args
            writes[self.path('/etc/openflux-client/yandex.env')]=('YANDEX_URL='+shlex.quote(value)+'\n').encode()
        if target=='openflux-volga-config':
            node=json.loads(read(node_path));validate_volga_config(json.loads(text),node['role'])
        elif target=='openflux-mailru':
            writes[path]=('MAILRU_DOCUMENT='+mailru_env(text)+'\n').encode()
        elif target=='openflux-node':
            old=json.loads(read(node_path));node=json.loads(text)
            if not isinstance(node,dict) or set(node)!=set(old):raise ValueError('Keep all existing profile fields')
            editable={'document_url','recovery_path','channel'}
            if any(node[k]!=old[k] for k in old if k not in editable):raise ValueError('Only document_url, recovery_path and channel are editable; keep managed fields unchanged')
            disk(node['recovery_path'])
            if node['channel'] not in ('stable','prerelease'):raise ValueError('Channel must be stable or prerelease')
            change_url(node,node['document_url']);writes[node_path]=encoded(node)
            settings=json.loads(read(settings_path));settings['openflux_prereleases']=node['channel']=='prerelease';writes[settings_path]=encoded(settings)
        elif target=='openflux-url':
            values=self.core.env_values(text)
            if set(values)!={'YANDEX_URL'}:raise ValueError('Expected only YANDEX_URL')
            node=json.loads(read(node_path));change_url(node,values['YANDEX_URL']);writes[node_path]=encoded(node)
        elif target=='openflux-updater':
            old=json.loads(read(settings_path));settings=json.loads(text)
            if not isinstance(settings,dict) or set(settings)!=set(old):raise ValueError('Keep all existing updater fields')
            allowed={'openflux_prereleases','openflux_expected_exit_ip'}
            if any(settings[k]!=old[k] for k in old if k not in allowed):raise ValueError('Only OpenFlux updater settings may change here')
            if type(settings.get('openflux_prereleases')) is not bool:raise ValueError('openflux_prereleases must be true or false')
            if settings.get('openflux_expected_exit_ip'):ipaddress.ip_address(settings['openflux_expected_exit_ip'])
            node=json.loads(read(node_path));node['channel']='prerelease' if settings['openflux_prereleases'] else 'stable';writes[node_path]=encoded(node)
        elif target=='openflux-disk-token':
            token=text.strip()
            if not re.fullmatch(r'[A-Za-z0-9._~-]{16,16384}',token):raise ValueError('Upload a file containing only the OAuth token, not cURL or JSON')
            writes[path]=(token+'\n').encode()
        elif target in ('openflux-server-key','openflux-client-key','openflux-client-public'):
            try:
                if target=='openflux-client-key':
                    key=serialization.load_pem_private_key(data,password=None)
                    if not isinstance(key,ed25519.Ed25519PrivateKey):raise ValueError()
                    public=key.public_key().public_bytes(serialization.Encoding.PEM,serialization.PublicFormat.SubjectPublicKeyInfo)
                    writes[self.path('/etc/openflux-recovery/sender-public.pem')]=public
                else:
                    key=serialization.load_pem_public_key(data)
                    if target=='openflux-server-key':
                        if not isinstance(key,rsa.RSAPublicKey) or key.key_size<3072:raise ValueError()
                    else:
                        private=serialization.load_pem_private_key(read(self.path('/etc/openflux-recovery/sender-private.pem')),password=None)
                        if not isinstance(key,ed25519.Ed25519PublicKey) or key.public_bytes(serialization.Encoding.Raw,serialization.PublicFormat.Raw)!=private.public_key().public_bytes(serialization.Encoding.Raw,serialization.PublicFormat.Raw):raise ValueError()
            except (ValueError,TypeError,AttributeError):raise ValueError('Invalid key type, or client public key does not match its private key') from None
        elif target=='openflux-cookies':
            cookies=json.loads(text)
            if not isinstance(cookies,dict) or not cookies:raise ValueError('Expected the downloaded cookie-store JSON format')
            for document,values in cookies.items():
                url(document)
                if not isinstance(values,dict) or not values:raise ValueError('Each document needs a non-empty cookie object')
                for name,value in values.items():
                    if not re.fullmatch(r'[!#$%&\x27*+.^_`|~0-9A-Za-z-]+',name) or not isinstance(value,str) or any(c in value for c in '\r\n\0'):raise ValueError('Invalid cookie entry')
            current=json.loads(read(node_path))['document_url']
            if current not in cookies:raise ValueError('Cookie file must include the configured document URL')
        else:raise ValueError('Unknown OpenFlux config')
        return writes

    def upload(self, body):
        import base64,json,os,tempfile,stat,time,uuid,fcntl,sys
        from pathlib import Path
        target=body.get('target');rows=self.registry()
        custom={'openflux-url','openflux-node','openflux-updater','openflux-disk-token','openflux-server-key','openflux-client-key','openflux-client-public','openflux-cookies','openflux-volga-config','openflux-mailru'}
        if target not in custom:return self.original_upload(body)
        if target not in rows or body.get('confirm') is not True:raise ValueError('Unknown config or missing replacement confirmation')
        data=base64.b64decode(body.get('data',''),validate=True)
        if not 0<len(data)<=self.core.MAX_FILE:raise ValueError('File must be at most 1 MiB')
        self.core.ROOT.mkdir(mode=0o700,parents=True,exist_ok=True)
        with open(self.core.ROOT/'openflux.lock','a') as lock:
            fcntl.flock(lock,fcntl.LOCK_EX)
            originals={}
            def read(path):
                path=Path(path)
                if path not in originals:originals[path]=self.core.contents(path)
                return originals[path]
            primary=Path(rows[target]['path']);original=read(primary)
            if body.get('revision')!=self.core.digest(original):raise ValueError('Config changed since loaded; refresh and retry')
            writes=self.plan(target,data,read)
            for path in writes:read(path)
            for path,previous in originals.items():
                if self.core.contents(path)!=previous:raise ValueError('Config changed during validation; retry')
            changes={p:b for p,b in writes.items() if b!=originals[p]}
            if not changes:return {'ok':True,'changed':False,'message':'File and related settings are already identical.'}
            backup=self.core.BACKUPS/(time.strftime('%Y%m%d-%H%M%S')+'-'+uuid.uuid4().hex[:8]);backup.mkdir(mode=0o700,parents=True)
            manifest=[]
            for i,path in enumerate(changes):
                saved=backup/str(i);saved.write_bytes(originals[path]);saved.chmod(0o600)
                manifest.append({'path':str(path),'copy':str(i)})
            (backup/'manifest.json').write_text(json.dumps(manifest,indent=2))
            def replace_file(path,content):
                info=path.stat();fd,tmp=tempfile.mkstemp(dir=path.parent,prefix='.'+path.name+'-')
                try:
                    with os.fdopen(fd,'wb') as file:
                        file.write(content);file.flush();os.fsync(file.fileno());os.fchown(file.fileno(),info.st_uid,info.st_gid);os.fchmod(file.fileno(),0o600)
                    os.replace(tmp,path)
                finally:
                    if os.path.exists(tmp):os.unlink(tmp)
            completed=[]
            try:
                for path,content in changes.items():replace_file(path,content);completed.append(path)
            except BaseException:
                for path in reversed(completed):replace_file(path,originals[path])
                raise
            node_path=self.path('/etc/openflux/node.json')
            auth=sys.modules.get('openflux_auth')
            if auth is not None and node_path in changes:
                auth.NODE.clear();auth.NODE.update(json.loads(changes[node_path]))
            message='Saved with backup. No services restarted.'
            if self.path('/etc/openflux-client/yandex.env') in changes:message+=' Restart OpenFlux to use the new document; profile URL is synchronized.'
            elif target=='openflux-client-key':message+=' Matching public key updated; register it on the server if identity changed.'
            elif target=='openflux-cookies':message+=' A blocked client will reload changed cookies automatically.'
            elif target=='openflux-mailru':message+=' Apply Mail.ru again (or restart OPENFLUX) to use the new document.'
            else:message+=' Used by the next recovery/update operation.'
            result={'ok':True,'changed':True,'target':target,'message':message,'backup':str(backup),'saved_at':int(time.time())}
            from xray_control import atomic
            atomic(self.core.ROOT/'last-upload.json',result)
            return result


def extend_config_store(core):
    if getattr(core,'_openflux_extended',False):return
    extension=OpenFluxConfigs(core)
    core.registry=extension.registry;core.upload=extension.upload;core._openflux_extended=True


# Keep the Volga adapter in an existing bundle member: v4.0.x updaters reject
# archives containing unknown filenames. Nothing below runs on module import.
VOLGA_PROFILE = {'posts_per_second':480,'record_window_bytes':1048576,
                 'record_chunk_bytes':5600,'flush_millis':1,'send_workers':64,'per_lane_budget':False}
VOLGA_CONFIG = '/etc/openflux-volga/config.json'
VOLGA_DATA = '/var/lib/openflux-volga'
VOLGA_BINARY = '/opt/openflux-volga/openflux-volga'
VOLGA_UNIT = 'openflux-volga-client.service'
VOLGA_CONTAINER = 'openflux-volga'
VOLGA_IMAGE_REPOSITORY = 'ghcr.io/levl-max/openflux-mod-volga'
CONFIG_BACKUPS = 3

# Mail.ru: one public Mail.ru Cloud document opened as an anonymous visitor and
# carried by the Legacy binary (--transport mailru). No cookies, no Disk
# recovery. A client runs it as its own unit on the router SOCKS port; a server
# runs its exit as a systemd-managed container beside the Legacy exit.
MAILRU_ENV = '/etc/openflux-mailru/mailru.env'
MAILRU_DATA = '/var/lib/openflux-mailru'
MAILRU_UNIT = 'openflux-mailru-client.service'
MAILRU_EXIT_UNIT = 'openflux-mailru-exit.service'
MAILRU_CONTAINER = 'openflux-mailru'
# The Legacy profile it was measured with (2026-10-11, PC2 through the AWS exit):
# fixed, like Volga's.
MAILRU_PROFILE = ('--compression=true','--yandex-queue-size=1024','--tcp-buffer-default=1048576',
                  '--tcp-buffer-max=4194304','--batch-packets=4','--batch-delay-us=1000')
# The exit forwards with raw sockets; its own kernel must not reset those flows.
# The rule lives in the container's network namespace only.
MAILRU_EXIT_SCRIPT = ('#!/bin/sh\nset -eu\niptables -C OUTPUT -p tcp --tcp-flags RST RST -j DROP 2>/dev/null || '
                      'iptables -A OUTPUT -p tcp --tcp-flags RST RST -j DROP\nexec /usr/local/bin/openflux "$@"\n')

def mailru_document(value):
    import re
    from urllib.parse import urlsplit
    if not isinstance(value,str) or len(value)>512 or any(c.isspace() for c in value):raise ValueError('Invalid Mail.ru document link')
    u=urlsplit(value)
    if (u.scheme!='https' or u.hostname!='cloud.mail.ru' or u.username or u.password or u.port not in (None,443) or u.query or u.fragment
            or not re.fullmatch(r'/public/[A-Za-z0-9_-]{2,64}/[A-Za-z0-9_-]{2,64}/?',u.path)):
        raise ValueError('Expected a public https://cloud.mail.ru/public/… document link')
    return value.rstrip('/')

def mailru_env(text):
    """The document link in mailru.env: exactly one MAILRU_DOCUMENT line."""
    values={}
    for line in text.splitlines():
        line=line.strip()
        if not line or line.startswith('#'):continue
        key,sep,value=line.partition('=')
        if not sep or key in values:raise ValueError('Expected one MAILRU_DOCUMENT=<public link> line')
        values[key]=value
    if set(values)!={'MAILRU_DOCUMENT'}:raise ValueError('Expected one MAILRU_DOCUMENT=<public link> line')
    return mailru_document(values['MAILRU_DOCUMENT'])

def mailru_exit_settings(value):
    """Container image and Docker network of a server's Mail.ru exit."""
    import re
    if not isinstance(value,dict) or set(value)!={'image','network'}:raise ValueError('The Mail.ru exit needs an image and a network')
    if not isinstance(value['image'],str) or not re.fullmatch(r'[a-z0-9][a-z0-9._/-]{0,199}(?::[A-Za-z0-9._-]{1,128})?(?:@sha256:[0-9a-f]{64})?',value['image']):
        raise ValueError('Invalid Mail.ru exit image')
    # Host networking would put the exit's RST rule on the host itself.
    if not isinstance(value['network'],str) or not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.-]{0,63}',value['network']) or value['network'] in ('host','none'):
        raise ValueError('The Mail.ru exit needs a bridge network of its own, not host or none')
    return value

def validate_volga_manifest(value, version, digest):
    import re
    if not isinstance(value,dict) or value.get('schema')!=1 or value.get('version')!=version or value.get('default')!='yandex':raise ValueError('Invalid protocol manifest')
    rows=[r for r in value.get('protocols',[]) if r.get('id')=='volga']
    if len(rows)!=1:raise ValueError('Missing or duplicate Volga protocol')
    row=rows[0]
    if row.get('binary')!={'name':'openflux-volga-linux-amd64','sha256':digest}:raise ValueError('Volga manifest checksum mismatch')
    if row.get('performance_profile')!=VOLGA_PROFILE or row.get('config_protocol')!='volga-stream-v1' or row.get('max_streams')!=64:raise ValueError('Unsupported Volga profile')
    if not re.fullmatch(r'ghcr.io/levl-max/openflux-mod-volga@sha256:[0-9a-f]{64}',row.get('image','')):raise ValueError('Unpinned Volga container image')
    if not re.fullmatch(r'sha256:[0-9a-f]{64}',row.get('image_id','')):raise ValueError('Missing immutable Volga image ID')
    return row

def volga_document(value, editor=False):
    from urllib.parse import urlsplit
    hosts=('docs.yandex.ru','docs.yandex.com') if editor else ('disk.yandex.ru','disk.yandex.com')
    if not isinstance(value,str) or len(value)>4096 or any(c.isspace() for c in value):raise ValueError('Invalid document URL')
    u=urlsplit(value)
    if u.scheme!='https' or u.hostname not in hosts or u.username or u.password or u.port not in (None,443) or u.fragment:raise ValueError('Expected a Yandex HTTPS document URL')
    if editor and not u.path.startswith('/edit/d/'):raise ValueError('Expected a direct editor URL')
    if not editor and not u.path.startswith(('/i/','/d/')):raise ValueError('Expected a public document link')
    return value

def validate_volga_config(value, role):
    import ipaddress,re
    allowed=set(VOLGA_PROFILE)|{'protocol','role','documents','shared_key','listen','cookie_store','browser_profile','idle_seconds','max_streams','egress_policy','allowed_targets','denied_cidrs'}
    if not isinstance(value,dict) or set(value)-allowed:raise ValueError('Unknown Volga configuration field')
    if value.get('protocol')!='volga-stream-v1' or value.get('role')!=role:raise ValueError('Wrong Volga protocol or node role')
    if any(type(value.get(k)) is not type(v) or value[k]!=v for k,v in VOLGA_PROFILE.items()):raise ValueError('Keep the frozen Volga performance profile')
    docs=value.get('documents')
    if not isinstance(docs,list) or len(docs)!=4 or len(set(docs))!=2:raise ValueError('Use four lanes over two separate documents')
    for doc in docs:volga_document(doc)
    if not re.fullmatch(r'[0-9a-fA-F]{64}',value.get('shared_key','')) or int(value['shared_key'],16)==0:raise ValueError('A random 32-byte shared key is required')
    if value.get('cookie_store')!=VOLGA_DATA+'/cookies.json' or value.get('browser_profile')!=VOLGA_DATA+'/browser.json':raise ValueError('Keep the dedicated Volga credential paths')
    if value.get('listen')!='127.0.0.1:11080':raise ValueError('Keep the router SOCKS listener on 127.0.0.1:11080')
    if type(value.get('max_streams')) is not int or not 1<=value['max_streams']<=64:raise ValueError('max_streams must be 1–64')
    if type(value.get('idle_seconds')) is not int or not 5<=value['idle_seconds']<=60:raise ValueError('idle_seconds must be 5–60')
    if value.get('egress_policy')!=('public' if role=='server' else 'allowlist'):raise ValueError('Server requires public-only egress')
    if value.get('allowed_targets')!=[]:raise ValueError('Production configuration must not contain private origin exceptions')
    if not isinstance(value.get('denied_cidrs'),list) or len(value['denied_cidrs'])>128:raise ValueError('Invalid denied_cidrs')
    for prefix in value['denied_cidrs']:ipaddress.ip_network(prefix,strict=False)
    return value

def openflux_runtime_unit():
    import json,pathlib
    try:node=json.loads(pathlib.Path('/etc/openflux/node.json').read_text())
    except (FileNotFoundError,ValueError):return 'openflux-yandex-client.service'
    selected=node.get('active_transport','yandex')
    if selected=='volga':return VOLGA_UNIT
    if selected=='mailru':return MAILRU_UNIT
    return node.get('service','openflux-yandex-client.service')

class VolgaRuntime:
    """Opt-in runtime; Legacy paths and state are never used for Volga updates."""
    def __init__(self, root='/'):
        import pathlib
        self.root=pathlib.Path(root)
        self.config_path=self.path(VOLGA_CONFIG)
        self.data=self.path(VOLGA_DATA)
        self.state=self.path('/var/lib/openflux-volga-updater')
        self.node_path=self.path('/etc/openflux/node.json')
        self.binary=self.path(VOLGA_BINARY)

    def path(self,name):return self.root/name.lstrip('/')

    @staticmethod
    def read(path,default=None):
        import json
        try:return json.loads(path.read_text())
        except FileNotFoundError:return default

    @staticmethod
    def save(path,value):
        import json,os,tempfile
        path.parent.mkdir(parents=True,exist_ok=True,mode=0o700)
        fd,tmp=tempfile.mkstemp(prefix='.volga-',dir=path.parent)
        try:
            with os.fdopen(fd,'w') as f:json.dump(value,f);f.flush();os.fsync(f.fileno())
            os.chmod(tmp,0o600)
            if path.exists():
                st=path.stat();os.chown(tmp,st.st_uid,st.st_gid)
            os.replace(tmp,path)
        finally:
            if os.path.exists(tmp):os.unlink(tmp)

    @staticmethod
    def run(argv,check=True,timeout=30):
        import subprocess
        p=subprocess.run(argv,capture_output=True,text=True,timeout=timeout)
        if check and p.returncode:raise RuntimeError('Volga operation failed: '+argv[0]+'; inspect the service log')
        return p

    def node(self):
        node=self.read(self.node_path,{})
        if node.get('role') not in ('client','server'):raise ValueError('Adopt the existing OpenFlux node first')
        return node

    def configuration(self):return validate_volga_config(self.read(self.config_path),self.node()['role'])

    def template(self):
        role=self.node()['role']
        return dict(VOLGA_PROFILE,protocol='volga-stream-v1',role=role,
            documents=['DOCUMENT_A','DOCUMENT_B','DOCUMENT_A','DOCUMENT_B'],shared_key='REPLACE_WITH_64_RANDOM_HEX_CHARACTERS',
            cookie_store=VOLGA_DATA+'/cookies.json',browser_profile=VOLGA_DATA+'/browser.json',listen='127.0.0.1:11080',
            max_streams=64,idle_seconds=30,egress_policy='public' if role=='server' else 'allowlist',allowed_targets=[],denied_cidrs=[])

    def configure(self,value):
        import os,pwd,time
        validate_volga_config(value,self.node()['role'])
        if self.config_path.exists():
            self.save(self.state/('config-backup-'+str(time.time_ns())+'.json'),self.read(self.config_path))
            # Backups hold the shared key: keep only the newest few.
            for old in sorted(self.state.glob('config-backup-*.json'))[:-CONFIG_BACKUPS]:old.unlink(missing_ok=True)
        self.save(self.config_path,value)
        self.data.mkdir(parents=True,exist_ok=True,mode=0o700)
        for name,default in [('cookies.json',{}),('browser.json',{'headers':{},'editors':{}})]:
            if not (self.data/name).exists():self.save(self.data/name,default)
        if self.node()['role']=='client':
            account=pwd.getpwnam(self.node().get('cookie_user','openflux'))
            for path in [self.config_path.parent,self.config_path,self.data,*self.data.glob('*.json')]:
                os.chown(path,account.pw_uid,account.pw_gid);os.chmod(path,0o700 if path.is_dir() else 0o600)
        if not self.binary.is_file():
            how='Router Updater: OpenFlux, Download, then Install.' if self.node().get('router_updater') else 'sudo openfluxctl update'
            return {'ok':True,'message':'Volga configuration saved. Volga is installed with the next OpenFlux update: '+how}
        return {'ok':True,'message':'Volga configuration saved with backup. Apply it by selecting or restarting Volga.'}

    def action(self,action):
        if action not in ('start','stop','restart'):raise ValueError('Invalid runtime action')
        node=self.node()
        if node['role']=='server':self.owned_container(VOLGA_CONTAINER,required=True)
        return self.run(['systemctl',action,VOLGA_UNIT] if node['role']=='client' else ['docker',action,VOLGA_CONTAINER])

    def owned_container(self,name,required=False):
        import json
        result=self.run(['docker','inspect',name],check=False)
        if result.returncode:
            if required:raise ValueError('Managed Volga container is missing')
            return False
        info=json.loads(result.stdout)[0]
        if info.get('Config',{}).get('Labels',{}).get('io.openflux.managed')!='volga':raise ValueError('Refusing to change a container not owned by this Volga adapter')
        return True

    def active(self):
        if self.node()['role']=='client':return self.run(['systemctl','is-active','--quiet',VOLGA_UNIT],check=False).returncode==0
        p=self.run(['docker','inspect','--format={{.State.Running}}',VOLGA_CONTAINER],check=False)
        return p.returncode==0 and p.stdout.strip()=='true'

    def status(self):
        import json,time
        node=self.node();out={'state':'stopped','active':False,'reason':'Volga is stopped.','needs_cookies':False,'configured':self.config_path.is_file(),'installed':self.binary.is_file(),'transport':'volga'}
        if out['configured']:
            try:out['documents']=list(dict.fromkeys(self.configuration()['documents']))
            except ValueError:out.update(configured=False,reason='Volga configuration needs attention.')
        # The server's verified report matters most while this client is stopped.
        peer=self.read(self.state/'peer-status.json')
        if peer:
            import openflux_auth
            peer['stale']=bool(peer.get('fetch_failed')) or time.time()-peer.get('reported_at',0)>openflux_auth.STATUS_STALE_AFTER
            out['server_authentication']=peer
        client=node['role']=='client'
        if not out['installed'] or not self.active():
            if client and out['installed']:out['client_check']=self.client_check(None)
            return out
        out.update(active=True,state='connecting',reason='Volga is connecting.')
        checks=[]
        if node['role']=='client':
            p=self.run(['systemctl','show',VOLGA_UNIT,'-p','InvocationID','--value'],check=False)
            invocation=p.stdout.strip()
            if not __import__('re').fullmatch('[0-9a-f]{32}',invocation):return out
            p=self.run(['journalctl','--no-pager','-o','cat','-n','80','_SYSTEMD_INVOCATION_ID='+invocation],check=False)
        else:
            p=self.run(['docker','inspect','--format={{.State.StartedAt}}',VOLGA_CONTAINER],check=False)
            p=self.run(['docker','logs','--since',p.stdout.strip(),'--tail','80',VOLGA_CONTAINER],check=False)
        for line in (p.stdout+'\n'+p.stderr).splitlines():
            try:e=json.loads(line)
            except ValueError:continue
            if not isinstance(e,dict):continue
            kind=e.get('event');data=e.get('data')
            check=self.check_event(e)
            if check:checks.append(check)
            if kind=='carrier_started':out['carrier_ready']=True
            if kind=='auth_blocked' or kind=='status' and isinstance(data,dict) and data.get('auth_blocked'):
                out.update(state='auth_blocked',needs_cookies=True,reason='Refresh browser cookies for Volga.')
            elif kind=='session_ready' or kind=='status' and isinstance(data,dict) and data.get('stream',{}).get('ready'):
                out.update(state='connected',needs_cookies=False,reason='Volga session is ready.')
            elif kind in ('session_closed','handshake_failed','connecting','stopped'):
                out.update(state='connecting',reason='Volga is reconnecting.')
        if client:out['client_check']=self.client_check(checks)
        out['checked_at']=int(time.time())
        return out

    # Log lines that settle whether Yandex accepted this mini-PC's Volga cookies.
    CHECK_PATTERN='"event":"(auth_blocked|session_ready|carrier_started)"|"auth_blocked":true'

    @staticmethod
    def check_event(e):
        """(time, 'refused' or 'ok') for a Volga event about its cookies, else None."""
        import datetime,re
        kind=e.get('event');data=e.get('data')
        if kind=='auth_blocked' or kind=='status' and isinstance(data,dict) and data.get('auth_blocked'):verdict='refused'
        elif kind in ('session_ready','carrier_started'):verdict='ok'
        else:return None
        try:ts=datetime.datetime.fromisoformat(re.sub(r'(\.\d{6})\d+',r'\1',str(e.get('time'))).replace('Z','+00:00')).timestamp()
        except ValueError:return None
        return ts,verdict

    def client_check(self,checks):
        """This mini-PC's last verdict on its Volga cookies (openflux_auth.client_check).
        checks are the running client's (time, verdict) pairs; for a stopped client
        (None) the runs nobody polled are read back from the journal."""
        import fcntl,json,time
        import openflux_auth as auth
        path=self.state/'client-check.json'
        self.state.mkdir(parents=True,exist_ok=True,mode=0o700)
        with open(self.state/'.client-check.lock','a') as lock:
            fcntl.flock(lock,fcntl.LOCK_EX)
            rec=self.read(path,{}) or {};before=json.dumps(rec,sort_keys=True)
            if checks is None:
                checks=[];last=float(rec.get('scanned_at',0));started=time.time()
                if started-last>=auth.SCAN_EVERY:
                    p=self.run(['journalctl','--no-pager','-o','cat','-u',VOLGA_UNIT,'--since','@%d'%int(max(last-1,started-auth.SCAN_BACK)),'--grep',self.CHECK_PATTERN],check=False,timeout=20)
                    for line in p.stdout.splitlines():
                        try:e=json.loads(line)
                        except ValueError:continue
                        check=self.check_event(e) if isinstance(e,dict) else None
                        if check:checks.append(check)
                    rec['scanned_at']=started
            for ts,verdict in checks:
                if verdict=='refused':auth.record_check(rec,ts,'auth_blocked','Yandex asked for a captcha or login.')
                else:auth.record_check(rec,ts,'connected','')
            if json.dumps(rec,sort_keys=True)!=before:self.save(path,rec)
        return auth.client_check(rec,[self.data/'cookies.json',self.data/'browser.json'])

    def health(self,seconds=90,require_session=True):
        import time
        deadline=time.monotonic()+seconds;passed=None
        while time.monotonic()<deadline:
            s=self.status()
            if not require_session and self.node()['role']=='server' and s.get('carrier_ready') and not s.get('needs_cookies'):return
            if not s['active']:raise RuntimeError('Volga stopped during health check')
            if s['state']=='auth_blocked':raise RuntimeError('Volga requires fresh browser cookies')
            if s['state']!='connected':passed=None
            elif self.node()['role']=='server':return
            elif self.exit_ok():
                passed=passed or time.monotonic()
                # A new session can still be replaced in its first seconds: pass
                # only on a second probe of a settled session.
                if time.monotonic()-passed>=3:return
            else:passed=None
            time.sleep(1)
        raise RuntimeError('Volga did not pass session and exit-IP checks')

    def exit_ok(self):
        import ipaddress
        expected=self.node().get('expected_exit_ip') or self.read(self.path('/etc/router/updater/settings.json'),{}).get('openflux_expected_exit_ip')
        for url in ('https://api.ipify.org','https://checkip.amazonaws.com'):
            p=self.run(['curl','-q','--fail','--silent','--noproxy','','--proxy','socks5h://127.0.0.1:11080','--max-time','12',url],check=False,timeout=15)
            try:address=str(ipaddress.ip_address(p.stdout.strip()))
            except ValueError:continue
            if p.returncode==0:return not expected or address==expected
        return False

    def parse_browser(self,document,text):
        import re,shlex
        import cookie_import
        if document not in self.configuration()['documents']:raise ValueError('Choose a configured Volga document')
        if not isinstance(text,str) or not 1<=len(text.encode())<=120000:raise ValueError('Paste Copy as cURL, at most 120 KiB')
        words=shlex.split(text.replace('\\\r\n',' ').replace('\\\n',' '))
        if not words or words[0]!='curl':raise ValueError('Expected Copy as cURL (bash)')
        urls=[];headers={};i=1
        allowed={'user-agent':'User-Agent','accept':'Accept','accept-language':'Accept-Language','sec-ch-ua':'Sec-CH-UA','sec-ch-ua-mobile':'Sec-CH-UA-Mobile','sec-ch-ua-platform':'Sec-CH-UA-Platform'}
        while i<len(words):
            word=words[i]
            if word in ('-H','--header','--url','-b','--cookie','-A','--user-agent'):
                if i+1>=len(words):raise ValueError('Incomplete cURL option')
                value=words[i+1];i+=2
                if word=='--url':urls.append(value)
                elif word in ('-H','--header'):
                    name,sep,content=value.partition(':')
                    if sep and name.lower() in allowed:headers[allowed[name.lower()]]=content.strip()
                elif word in ('-A','--user-agent'):headers['User-Agent']=value
                continue
            if word.startswith('https://'):urls.append(word)
            i+=1
        if len(urls)!=1:raise ValueError('Paste one document request at a time')
        editor=None
        if urls[0]!=document:editor=volga_document(urls[0],editor=True)
        try:cookies=cookie_import.parse_cookie_map(cookie_import.extract_cookie_header(text))
        except SystemExit:raise ValueError('No usable browser cookies') from None
        for name,value in cookies.items():
            if not re.fullmatch(r'[!#$%&\x27*+.^_`|~0-9A-Za-z-]+',name) or any(c in value for c in '\r\n\0'):raise ValueError('Invalid cookie entry')
        if any(any(c in value for c in '\r\n\0') or len(value)>8192 for value in headers.values()):raise ValueError('Invalid browser header')
        return cookies,headers,editor

    def import_cookies(self,document,text):
        cookies,headers,editor=self.parse_browser(document,text)
        profile=self.read(self.data/'browser.json',{'headers':{},'editors':{}})
        profile['headers'].update(headers)
        if editor:profile['editors'][document]=editor
        store=self.read(self.data/'cookies.json',{});store[document]=cookies
        # The cookie change is the reconnect signal; publish the profile first.
        self.save(self.data/'browser.json',profile);self.save(self.data/'cookies.json',store)
        return {'ok':True,'count':len(cookies),'message':'Volga cookies saved. Authorization will retry automatically; Legacy cookies are unchanged.'}

    def disk_path(self,document=None):
        node=self.node()
        base=node.get('recovery_path','disk:/OpenFlux Recovery/server-recovery.json')
        path=node.get('volga_recovery_path') or (base[:-5] if base.endswith('.json') else base)+'-volga.json'
        if not isinstance(path,str) or not path.startswith(('disk:/','app:/')) or '..' in path.split('/') or any(ord(c)<32 for c in path):raise ValueError('Invalid Volga recovery path')
        if path==base:raise ValueError('Volga and Legacy recovery files must differ')
        if document is not None:
            if document not in self.configuration()['documents']:raise ValueError('Unknown Volga recovery document')
            suffix=__import__('hashlib').sha256(document.encode()).hexdigest()[:16]
            path=(path[:-5] if path.endswith('.json') else path)+'-'+suffix+'.json'
        return path

    def package_cookies(self,document,text,upload=False):
        import json
        from cryptography.hazmat.primitives import serialization
        import recovery_crypto,recovery_inbox
        if self.node()['role']!='client':raise ValueError('Send recovery cookies from a client')
        self.parse_browser(document,text)
        keys=self.path('/etc/openflux-recovery')
        public=serialization.load_pem_public_key((keys/'server-public.pem').read_bytes())
        signer=serialization.load_pem_private_key((keys/'sender-private.pem').read_bytes(),password=None)
        payload=json.dumps({'protocol':'volga-stream-v1','document':document,'curl':text})
        packet=recovery_crypto.seal(payload,public,signer,protocol='volga')
        if upload:
            recovery_inbox.upload(self.disk_path(document),packet,(keys/'disk-token').read_text().strip())
            return {'ok':True,'message':'Encrypted Volga cookies uploaded. The server checks its separate inbox once a minute.'}
        return {'ok':True,'package':packet,'filename':self.disk_path(document).rsplit('/',1)[-1],'message':'Encrypted Volga recovery file ready.'}

    def recovery_poll(self):
        import json,time,urllib.error
        from cryptography.hazmat.primitives import serialization
        import openflux_auth,recovery_crypto,recovery_inbox
        keys=self.path('/etc/openflux-recovery');token=(keys/'disk-token').read_text().strip()
        if openflux_auth.disk_token_rejected(token):return  # Disk refused it: wait for a new token
        path=self.disk_path();state=self.read(self.state/'recovery.json',{'seen':[]})
        now=int(time.time())
        def refused(error):
            if recovery_inbox.refused(error):
                openflux_auth.reject_disk_token(token);return True
            return False
        if self.node()['role']=='client':
            previous=self.read(self.state/'peer-status.json',{})
            if not recovery_inbox.client_poll_due(previous.get('checked_at'),now):return
            try:
                public=serialization.load_pem_public_key((keys/'server-public.pem').read_bytes())
                peer=recovery_crypto.verify_status(recovery_inbox.download(path+'.status.json',token,16000),public,protocol='volga')
                if peer['reported_at']<previous.get('reported_at',0):raise ValueError('Older Volga status')
                peer.update(checked_at=int(time.time()),fetch_failed=False)
            except Exception as e:
                refused(e)
                peer=previous or {'state':'unknown','reported_at':0}
                peer.update(checked_at=int(time.time()),fetch_failed=True,stale=True)
            self.save(self.state/'peer-status.json',peer);return
        private=serialization.load_pem_private_key((keys/'server-private.pem').read_bytes(),password=None)
        status={}
        try:
            status=self.status()
            view={k:status.get(k) for k in ('state','reason','needs_cookies','active','carrier_ready')}
            signature=recovery_inbox.status_signature(view,tuple(view))
            if recovery_inbox.publish_due(state,signature,now):
                recovery_inbox.upload(path+'.status.json',recovery_crypto.sign_status(status,private,protocol='volga'),token)
                state.update(status_published_at=now,status_signature=signature)
                state.pop('status_publish_error',None)
        except Exception as e:
            state['status_publish_error']='Volga status upload unavailable'
            if refused(e):self.save(self.state/'recovery.json',state);return
        # A server that waits for cookies checks its inboxes often; one that works rarely.
        waiting=status.get('needs_cookies') or status.get('state')=='auth_blocked'
        every=recovery_inbox.SERVER_POLL_WAITING if waiting else recovery_inbox.SERVER_POLL_EVERY
        # Separate document files prevent A being overwritten by B before the
        # next poll. Failure/missing cookies for A never delay processing B.
        for document in dict.fromkeys(self.configuration()['documents']):
            inbox=self.disk_path(document);entry=state.setdefault('inboxes',{}).setdefault(inbox,{})
            if now<entry.get('next_check',0) or now-int(entry.get('checked_at') or 0)<every:continue
            entry['checked_at']=now
            try:
                md5=recovery_inbox.disk_md5(inbox,token)
                if md5 and md5==entry.get('md5'):continue  # unchanged since it was last downloaded
                packet=recovery_inbox.download(inbox,token);entry['md5']=md5
                if packet.get('id') not in state['seen']:
                    trusted=list((keys/'trusted-senders').glob('*.pem'))
                    if (keys/'sender-public.pem').exists():trusted.append(keys/'sender-public.pem')
                    decoded=None
                    for path_key in trusted:
                        try:
                            public=serialization.load_pem_public_key(path_key.read_bytes())
                            decoded=recovery_crypto.unseal(packet,private,public,state['seen'],protocol='volga');break
                        except Exception:continue
                    if decoded is None:raise ValueError('No trusted Volga sender')
                    payload,ident=decoded;payload=json.loads(payload)
                    if set(payload)!={'protocol','document','curl'} or payload['protocol']!='volga-stream-v1' or payload['document']!=document:raise ValueError('Wrong cookie payload')
                    self.import_cookies(document,payload['curl'])
                    state['seen']=(state['seen']+[ident])[-256:];entry['applied_at']=int(time.time())
                entry.update(failures=0,next_check=0,inbox='Applied or already seen')
            except Exception as e:
                auth=refused(e)
                missing=isinstance(e,urllib.error.HTTPError) and e.code==404
                disk=isinstance(e,(OSError,recovery_inbox.DiskCaptcha)) and not missing
                # Only a downloaded package has an outcome that stands while the
                # file is unchanged. After a Disk failure it is downloaded again.
                if disk or missing:entry.pop('md5',None)
                failures=entry.get('failures',0)+1
                entry.update(failures=failures,next_check=int(time.time())+min(900,60*2**min(failures,4)),
                    inbox='DISK_AUTH_REQUIRED' if auth else 'Waiting for a Volga recovery file' if missing else 'Yandex Disk temporarily unavailable' if disk else 'Waiting for a valid Volga recovery file')
                if auth:break
        self.save(self.state/'recovery.json',state)

    def limits(self):
        value=self.node().get('volga_resources',{'memory_mib':256,'cpu_percent':50})
        if not isinstance(value,dict) or set(value)!={'memory_mib','cpu_percent'}:raise ValueError('Invalid Volga resource limits')
        if type(value['memory_mib']) is not int or not 192<=value['memory_mib']<=2048:raise ValueError('Volga memory limit must be 192–2048 MiB')
        if type(value['cpu_percent']) is not int or not 10<=value['cpu_percent']<=200:raise ValueError('Volga CPU limit must be 10–200 percent of one core')
        return value

    def unit_text(self):
        import re
        node=self.node();limits=self.limits();account=node.get('cookie_user','openflux');legacy=node.get('service','openflux-yandex-client.service')
        if not re.fullmatch(r'[a-z_][a-z0-9_-]*',account) or not re.fullmatch(r'[A-Za-z0-9_.@-]+',legacy):raise ValueError('Invalid service identity')
        return ('[Unit]\nDescription=OpenFlux Volga client\nAfter=network-online.target\nWants=network-online.target\n'
            'Conflicts='+legacy+'\nPartOf=sing-box-openflux.service\n[Service]\nType=simple\nUser='+account+'\nGroup='+account+'\n'
            'ExecStart='+VOLGA_BINARY+' -config '+VOLGA_CONFIG+'\nRestart=on-failure\nRestartSec=5\nUMask=0077\n'
            'NoNewPrivileges=yes\nProtectSystem=strict\nProtectHome=yes\nPrivateTmp=yes\nPrivateDevices=yes\n'
            'CapabilityBoundingSet=\nProtectKernelTunables=yes\nProtectKernelModules=yes\nProtectControlGroups=yes\n'
            'RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK\nReadWritePaths='+VOLGA_DATA+'\n'
            'MemoryMax='+str(limits['memory_mib'])+'M\nMemorySwapMax=0\nCPUQuota='+str(limits['cpu_percent'])+'%\nTasksMax=128\n'
            'Environment=GOMAXPROCS=1\nEnvironment=GOMEMLIMIT='+str(limits['memory_mib']*5//8)+'MiB\n[Install]\nWantedBy=multi-user.target\n')

    def docker_command(self,image,name=VOLGA_CONTAINER):
        import re
        if not re.fullmatch(r'(?:ghcr.io/levl-max/openflux-mod-volga@)?sha256:[0-9a-f]{64}',image):raise ValueError('Use the verified immutable image')
        limits=self.limits()
        return ['docker','create','--name',name,'--label','io.openflux.managed=volga','--restart=unless-stopped',
            '--cap-drop=ALL','--security-opt=no-new-privileges','--read-only','--network=bridge','--pids-limit=128',
            '--memory='+str(limits['memory_mib'])+'m','--memory-swap='+str(limits['memory_mib'])+'m','--cpus='+str(limits['cpu_percent']/100),
            '--env','GOMAXPROCS=1','--env','GOMEMLIMIT='+str(limits['memory_mib']*5//8)+'MiB',
            '--mount','type=bind,src=/opt/openflux-volga,dst=/opt/openflux-volga,readonly',
            '--mount','type=bind,src=/etc/openflux-volga,dst=/etc/openflux-volga,readonly',
            '--mount','type=bind,src='+VOLGA_DATA+',dst='+VOLGA_DATA,
            '--entrypoint',VOLGA_BINARY,image,'-config',VOLGA_CONFIG]

    def install_recovery_timer(self):
        library=self.node()['library']
        if any(c in library for c in '\r\n\0'):raise ValueError('Invalid integration path')
        units=self.path('/etc/systemd/system')
        (units/'openflux-volga-recovery.service').write_text('[Unit]\nDescription=Volga cookie recovery\nAfter=network-online.target\nConditionPathExists=/etc/openflux-recovery/disk-token\n[Service]\nType=oneshot\nUMask=0077\nTimeoutStartSec=150\nExecStart=/usr/bin/python3 '+library+'/openflux_node.py poll-volga-recovery\n')
        (units/'openflux-volga-recovery.timer').write_text('[Unit]\nDescription=Volga recovery inbox\n[Timer]\nOnBootSec=30\nOnUnitActiveSec=60\n[Install]\nWantedBy=timers.target\n')
        self.run(['systemctl','daemon-reload']);self.run(['systemctl','enable','--now','openflux-volga-recovery.timer'])

    def legacy_active(self):
        return self.run(['systemctl','is-active','--quiet',self.node().get('service','openflux-yandex-client.service')],check=False).returncode==0

    def refresh_panel(self):
        if not self.node().get('router_updater'):return
        # A panel request can be the updater itself. Restart only after its
        # transaction and HTTP response have completed, never during file copy.
        self.run(['systemd-run','--quiet','--collect','--unit=openflux-volga-panel-reload-'+str(__import__('time').time_ns()),
            '--on-active=2s','/usr/bin/systemctl','try-restart','router-panel.service'])

    def begin_transaction(self,kind):
        import time,shutil
        import openflux_node as node
        if (self.state/'transaction.json').exists():raise ValueError('Recover the interrupted Volga operation first')
        if self.node()['role']=='server':
            existing=self.run(['docker','inspect',VOLGA_CONTAINER],check=False)
            if existing.returncode==0:
                info=__import__('json').loads(existing.stdout)[0]
                if info.get('Config',{}).get('Labels',{}).get('io.openflux.managed')!='volga':raise ValueError('Existing Volga-named container is not owned by this adapter')
        ident=str(time.time_ns());backup=self.state/('backup-'+ident);backup.mkdir(parents=True,mode=0o700)
        record={'kind':kind,'node':self.node(),'installed':self.read(self.state/'installed.json',{}),
                'active':self.active(),'legacy_active':self.legacy_active() if self.node()['role']=='client' else False,
                'binary_present':self.binary.is_file(),'backup':str(backup),'watchdog':'openflux-volga-return-'+ident}
        if self.node()['role']=='client':
            record['bridge_active']=self.run(['systemctl','is-active','--quiet','sing-box-openflux.service'],check=False).returncode==0
            record['mailru_active']=self.run(['systemctl','is-active','--quiet',MAILRU_UNIT],check=False).returncode==0
        if record['binary_present']:
            shutil.copy2(self.binary,backup/'binary');record['binary_sha256']=node.sha(backup/'binary')
        unit=self.path('/etc/systemd/system')/VOLGA_UNIT
        record['unit']=unit.read_text() if unit.exists() else None
        if kind=='update':
            record['configuration']=self.read(self.config_path)
            record['recovery_units']={name:(self.path('/etc/systemd/system')/name).read_text() if (self.path('/etc/systemd/system')/name).exists() else None for name in ('openflux-volga-recovery.service','openflux-volga-recovery.timer')}
            record['recovery_enabled']=self.run(['systemctl','is-enabled','openflux-volga-recovery.timer'],check=False).stdout.strip()=='enabled'
            record['recovery_active']=self.run(['systemctl','is-active','--quiet','openflux-volga-recovery.timer'],check=False).returncode==0
        if kind=='update' and not record['installed'] and (record['binary_present'] or record['unit'] is not None):raise ValueError('Refusing to overwrite an untracked Volga installation')
        # Recovery must use the old, complete support code even if replacement
        # of the installed modules is interrupted halfway through.
        guard=self.path(self.node()['library'])/'openflux_node.py'
        if kind=='update':
            import openflux_release
            recovery=backup/'recovery';recovery.mkdir(mode=0o700)
            for name in (*openflux_release.MODULES,'openflux_node.py'):
                shutil.copy2(self.path(self.node()['library'])/name,recovery/name)
            guard=recovery/'openflux_node.py'
        self.save(self.state/'transaction.json',record)
        self.run(['systemd-run','--quiet','--unit='+record['watchdog'],'--on-active=180s','--property=Restart=on-failure','--property=RestartSec=5s','/usr/bin/python3',str(guard),'recover-volga'])
        return record

    def restore_transaction(self,record=None):
        import pathlib
        import openflux_node as node
        record=record or self.read(self.state/'transaction.json') or self.read(self.state/'rollback.json')
        if not record:raise ValueError('No Volga rollback checkpoint')
        self.action('stop') if self.active() else None
        if record['node']['role']=='client':
            self.run(['systemctl','stop',record['node'].get('service','openflux-yandex-client.service')],check=False)
            self.run(['systemctl','stop',MAILRU_UNIT],check=False)
        if record['kind']=='update':
            # Validate every backup before restoring any support file.
            for item in record.get('support_files',[]):
                if item['present'] and node.sha(item['copy'])!=item['sha256']:raise ValueError('Volga support rollback checksum mismatch')
            for item in record.get('support_files',[]):
                if item['present']:node.atomic_file(item['copy'],item['path'],item['mode'])
                else:pathlib.Path(item['path']).unlink(missing_ok=True)
            if record['binary_present']:
                source=pathlib.Path(record['backup'])/'binary'
                if node.sha(source)!=record['binary_sha256']:raise ValueError('Volga rollback checksum mismatch')
                node.atomic_file(source,self.binary)
            else:self.binary.unlink(missing_ok=True)
            if record.get('old_container'):
                # The checkpoint is written BEFORE rename. A failed rename must
                # never cause deletion of the still-original container.
                if self.owned_container(record['old_container']):
                    if self.owned_container(VOLGA_CONTAINER):self.run(['docker','rm','-f',VOLGA_CONTAINER])
                    self.run(['docker','rename',record['old_container'],VOLGA_CONTAINER])
            elif record.get('created_container') and self.owned_container(VOLGA_CONTAINER):self.run(['docker','rm','-f',VOLGA_CONTAINER])
            unit=self.path('/etc/systemd/system')/VOLGA_UNIT
            if record['unit'] is not None:unit.write_text(record['unit'])
            else:unit.unlink(missing_ok=True)
            if record.get('configuration') is not None:self.save(self.config_path,record['configuration'])
            if 'recovery_units' in record:
                self.run(['systemctl','disable','--now','openflux-volga-recovery.timer'],check=False)
                for name,content in record['recovery_units'].items():
                    path=self.path('/etc/systemd/system')/name
                    if content is None:path.unlink(missing_ok=True)
                    else:path.write_text(content)
            self.run(['systemctl','daemon-reload'])
            if record.get('recovery_enabled'):self.run(['systemctl','enable','openflux-volga-recovery.timer'])
            if record.get('recovery_active'):self.run(['systemctl','start','openflux-volga-recovery.timer'])
            self.save(self.state/'installed.json',record['installed'])
            for item in record.get('router_hooks',[]):
                source=self.state/'router-restore.tmp';source.write_text(item['content'])
                node.atomic_file(source,item['path'],item['mode']);source.unlink()
        self.save(self.node_path,record['node'])
        if record['active']:self.action('start')
        if record['legacy_active']:self.run(['systemctl','start',record['node'].get('service','openflux-yandex-client.service')])
        if record.get('mailru_active'):self.run(['systemctl','start',MAILRU_UNIT])
        if record.get('bridge_active'):self.run(['systemctl','start','sing-box-openflux.service'])
        self.run(['systemctl','stop',record['watchdog']+'.timer'],check=False)
        (self.state/'transaction.json').unlink(missing_ok=True)
        if record.get('support_files'):self.refresh_panel()
        return {'ok':True,'message':'Previous Volga version and protocol selection restored.'}

    def replace_server_container(self,image,record,save):
        # The OpenFlux server updater owns the transaction; save() persists the
        # record before the rename, so a crash never loses the original container.
        import json,ipaddress,time
        cfg=self.configuration()
        # Host addresses are not visible from a bridge container. Pin its current
        # connected subnets as additional explicit denies.
        addresses=json.loads(self.run(['ip','-j','address','show']).stdout)
        prefixes={str(ipaddress.ip_network(str(a['local'])+'/'+str(a['prefixlen']),strict=False)) for i in addresses for a in i.get('addr_info',[]) if a.get('family') in ('inet','inet6')}
        cfg['denied_cidrs']=sorted(set(cfg['denied_cidrs'])|prefixes);validate_volga_config(cfg,'server');self.save(self.config_path,cfg)
        if self.owned_container(VOLGA_CONTAINER):
            record['old_container']='openflux-volga-backup-'+str(time.time_ns());save(record)
            # One server per document pair: a running old server stops before the
            # new one exists. Rollback starts it again if it was running.
            self.run(['docker','stop',VOLGA_CONTAINER])
            self.run(['docker','rename',VOLGA_CONTAINER,record['old_container']])
        record['created_container']=True;save(record)
        self.run(self.docker_command(image))

    def restore_server_container(self,record):
        # A failed rename must never cause deletion of the still-original container.
        if record.get('old_container') and self.owned_container(record['old_container']):
            if self.owned_container(VOLGA_CONTAINER):self.run(['docker','rm','-f',VOLGA_CONTAINER])
            self.run(['docker','rename',record['old_container'],VOLGA_CONTAINER])
        elif record.get('created_container') and self.owned_container(VOLGA_CONTAINER):self.run(['docker','rm','-f',VOLGA_CONTAINER])

    def prune_server_containers(self,keep):
        # One previous container is enough for a rollback; older ones only use disk.
        managed=lambda:self.run(['docker','ps','-a','--filter','label=io.openflux.managed=volga','--format','{{.Names}}'],check=False).stdout.split()
        for name in managed():
            if name.startswith('openflux-volga-backup-') and name!=keep:self.run(['docker','rm','-f',name],check=False)
        # Every release loads a new image; keep only those the remaining containers
        # use. Without -f Docker refuses to remove an image that is still in use.
        used={self.run(['docker','inspect','--format={{.Image}}',name],check=False).stdout.strip() for name in managed()}
        for line in self.run(['docker','images','--no-trunc','--format','{{.ID}} {{.Repository}}'],check=False).stdout.splitlines():
            image,_,repository=line.partition(' ')
            if repository==VOLGA_IMAGE_REPOSITORY and image not in used:self.run(['docker','image','rm',image],check=False)

    def select(self,transport):
        if transport not in ('yandex','volga','mailru'):raise ValueError('This protocol is not installed or supported')
        profile=self.node()
        if profile['role']!='client':raise ValueError('Server transports run independently; there is no server protocol switch')
        if transport=='yandex' and profile.get('active_transport','yandex')=='yandex' and not self.binary.is_file():return {'ok':True,'transport':'yandex','message':'Legacy is already selected.'}
        mailru=MailruRuntime(self.root)
        if transport=='volga':
            self.configuration()
            if not self.read(self.state/'installed.json'):raise ValueError('Install the verified Volga release first')
        if transport=='mailru':
            if not mailru.document() or not mailru.installed():raise ValueError('Set up Mail.ru first')
            # The router helpers must accept the Mail.ru unit (idempotent).
            self.install_router_hooks()
        previous=self.begin_transaction('switch')
        try:
            self.run(['systemctl','stop',profile.get('service','openflux-yandex-client.service')])
            if self.active():self.action('stop')
            if previous.get('mailru_active'):self.run(['systemctl','stop',MAILRU_UNIT])
            changed=dict(profile,active_transport=transport);self.save(self.node_path,changed)
            if transport=='volga':self.action('start');self.health()
            elif transport=='mailru':mailru.action('start');mailru.health()
            else:
                self.run(['systemctl','start',profile.get('service','openflux-yandex-client.service')])
                import openflux_node
                openflux_node.health(profile)
            if previous.get('bridge_active'):self.run(['systemctl','start','sing-box-openflux.service'])
            # Selecting a protocol does not change the router's traffic mode.
            mode=self.path('/etc/router/mode')
            if mode.exists() and mode.read_text().strip()!='openflux' and not previous['active'] and not previous['legacy_active'] and not previous.get('mailru_active'):
                if transport=='volga':self.action('stop')
                elif transport=='mailru':mailru.action('stop')
                else:self.run(['systemctl','stop',profile.get('service','openflux-yandex-client.service')])
            self.run(['systemctl','stop',previous['watchdog']+'.timer'])
            (self.state/'transaction.json').unlink()
            return {'ok':True,'transport':transport,'message':'Protocol selected; SOCKS and exit-IP checks passed.'}
        except BaseException:
            self.restore_transaction(previous);raise

    def install_router_hooks(self):
        # Router helpers follow the selected protocol's unit. The OpenFlux updater
        # checkpoints these files, so one rollback restores them with the release.
        import os
        import openflux_node as node
        if not self.node().get('router_updater'):return []
        self.state.mkdir(parents=True,exist_ok=True,mode=0o700)
        files=['/usr/local/sbin/openflux-routerctl','/usr/local/sbin/openflux-clientctl','/usr/local/sbin/router-restore-runtime']
        optional=['/usr/local/sbin/router-runtime-ensure','/usr/local/lib/router-wan/openflux_health.py','/usr/local/lib/router-xray/openflux_health.py']
        for raw in optional:
            if self.path(raw).is_file():files.append(raw)
        if not any(x.endswith('openflux_health.py') for x in files):raise ValueError('Unknown router health layout')
        plans=[]
        for raw in files:
            path=self.path(raw);old=path.read_text()
            # Some routers use this optional helper only to dispatch to the
            # controllers above. With no fixed client unit there is no patch.
            if raw=='/usr/local/sbin/router-runtime-ensure' and 'openflux-yandex-client' not in old:continue
            new=patch_runtime_selector(old,python=path.suffix=='.py')
            if new==old:continue
            if path.suffix=='.py':compile(new,str(path),'exec')
            else:
                import tempfile
                fd,tmp=tempfile.mkstemp(dir=self.state);os.close(fd)
                try:
                    __import__('pathlib').Path(tmp).write_text(new);self.run(['bash','-n',tmp])
                finally:os.unlink(tmp)
            plans.append((path,old,new,path.stat().st_mode&0o777))
        for path,old,new,mode in plans:
            if path.read_text()!=old:raise ValueError('Router helper changed during preparation')
            source=self.state/'router-hook.tmp';source.write_text(new)
            node.atomic_file(source,path,mode);source.unlink()
        return [str(path) for path,old,new,mode in plans]

class MailruRuntime:
    """Mail.ru beside Legacy and Volga: the Legacy binary with --transport mailru,
    in its own client unit or, on a server, in an exit container. No cookies."""
    # Status lines of transport/mailru; the newest one decides. Fixed texts only.
    EVENTS=(
        ('connected to the document','connected','Connected to the Mail.ru document.'),
        ('connection to the document dropped','connecting','The connection to the Mail.ru document dropped; reconnecting.'),
        ('cannot open the document','connecting','Mail.ru did not open the document; retrying. If this persists, check the link and that the document can be edited by link.'),
        ('document server refused the connection','connecting','Mail.ru refused the document connection; retrying.'),
        ('cannot reach the document server','connecting','Mail.ru is unreachable; retrying.'),
        ('did not finish the handshake','connecting','Mail.ru did not finish the document handshake; retrying.'))

    def __init__(self, root='/'):
        import pathlib
        self.root=pathlib.Path(root)
        self.env_path=self.path(MAILRU_ENV)
        self.data=self.path(MAILRU_DATA)
        self.node_path=self.path('/etc/openflux/node.json')
        self.units=self.path('/etc/systemd/system')

    def path(self,name):return self.root/name.lstrip('/')
    read=staticmethod(VolgaRuntime.read)
    save=staticmethod(VolgaRuntime.save)

    @staticmethod
    def run(argv,check=True,timeout=30):
        import subprocess
        p=subprocess.run(argv,capture_output=True,text=True,timeout=timeout)
        if check and p.returncode:raise RuntimeError('Mail.ru operation failed: '+argv[0]+'; inspect the service log')
        return p

    def node(self):
        node=self.read(self.node_path,{})
        if node.get('role') not in ('client','server'):raise ValueError('Adopt the existing OpenFlux node first')
        return node

    def unit(self):return MAILRU_UNIT if self.node()['role']=='client' else MAILRU_EXIT_UNIT

    def role(self):
        try:return self.node()['role']
        except ValueError:return None

    def disk_path(self):
        """The Yandex Disk file of the server's Mail.ru status, beside Legacy's and Volga's."""
        base=self.node().get('recovery_path','disk:/OpenFlux Recovery/server-recovery.json')
        path=(base[:-5] if base.endswith('.json') else base)+'-mailru.json'
        if not isinstance(path,str) or not path.startswith(('disk:/','app:/')) or '..' in path.split('/') or any(ord(c)<32 for c in path):raise ValueError('Invalid Mail.ru status path')
        return path

    # A server that reconnected within this long still counts as connected, so a
    # routine reconnect neither alarms the clients nor costs a Disk upload.
    RECONNECT_GRACE=120

    def status_exchange(self):
        """Mail.ru's server status on Yandex Disk, signed like Legacy's and Volga's:
        a server publishes its exit's state on a change and at least hourly, a
        client fetches it while someone looks at its panel. Mail.ru has no cookies,
        so there is no inbox. Runs from the Volga recovery timer."""
        import time
        from cryptography.hazmat.primitives import serialization
        import openflux_auth,recovery_crypto,recovery_inbox
        try:
            if not self.installed() or not self.document():return
        except ValueError:return
        keys=self.path('/etc/openflux-recovery')
        try:token=(keys/'disk-token').read_text().strip()
        except FileNotFoundError:return
        if not token or openflux_auth.disk_token_rejected(token):return
        path=self.disk_path()+'.status.json';now=int(time.time())
        def refused(error):
            if recovery_inbox.refused(error):openflux_auth.reject_disk_token(token)
        if self.role()=='client':
            previous=self.read(self.data/'peer-status.json',{}) or {}
            if not recovery_inbox.client_poll_due(previous.get('checked_at'),now):return
            try:
                public=serialization.load_pem_public_key((keys/'server-public.pem').read_bytes())
                peer=recovery_crypto.verify_status(recovery_inbox.download(path,token,16000),public,protocol='mailru')
                if peer['reported_at']<previous.get('reported_at',0):raise ValueError('Older Mail.ru status')
                peer.update(checked_at=now,fetch_failed=False)
            except Exception as e:
                refused(e)
                peer=previous or {'state':'unknown','reported_at':0}
                peer.update(checked_at=now,fetch_failed=True,stale=True)
            self.save(self.data/'peer-status.json',peer);return
        state=self.read(self.data/'recovery.json',{}) or {}
        try:
            private=serialization.load_pem_private_key((keys/'server-private.pem').read_bytes(),password=None)
            status=self.status()
            if status.get('state')=='connecting' and now-int(status.get('connected_at') or 0)<self.RECONNECT_GRACE:
                status=dict(status,state='connected',reason='Connected to the Mail.ru document.')
            view={k:status.get(k) for k in ('state','reason','active')}
            signature=recovery_inbox.status_signature(view,tuple(view))
            if recovery_inbox.publish_due(state,signature,now):
                recovery_inbox.upload(path,recovery_crypto.sign_status(status,private,protocol='mailru'),token)
                state.update(status_published_at=now,status_signature=signature);state.pop('status_publish_error',None)
        except Exception as e:
            state['status_publish_error']='Mail.ru status upload unavailable';refused(e)
        self.save(self.data/'recovery.json',state)

    def document(self):
        """The configured document link; None before Mail.ru is set up."""
        try:return mailru_env(self.env_path.read_text())
        except FileNotFoundError:return None

    def installed(self):return (self.units/self.unit()).is_file()

    def active(self):return self.run(['systemctl','is-active','--quiet',self.unit()],check=False).returncode==0

    def binary(self):
        import re
        binary=self.node().get('binary','')
        if not isinstance(binary,str) or not re.fullmatch(r'/[A-Za-z0-9_./-]+',binary) or '..' in binary.split('/'):raise ValueError('Invalid OpenFlux binary path')
        return binary

    def unit_text(self):
        import re
        node=self.node();account=node.get('cookie_user','openflux');legacy=node.get('service','openflux-yandex-client.service')
        if not re.fullmatch(r'[a-z_][a-z0-9_-]*',account) or not re.fullmatch(r'[A-Za-z0-9_.@-]+',legacy):raise ValueError('Invalid service identity')
        # The hardening of the Legacy client unit, which runs the same binary.
        return ('[Unit]\nDescription=OpenFlux Mail.ru client\nAfter=network-online.target\nWants=network-online.target\n'
            'Conflicts='+legacy+' '+VOLGA_UNIT+'\nPartOf=sing-box-openflux.service\n[Service]\nType=simple\nUser='+account+'\nGroup='+account+'\n'
            'EnvironmentFile='+MAILRU_ENV+'\nExecStart='+self.binary()+' --client --transport mailru --url ${MAILRU_DOCUMENT} --socks5 127.0.0.1:11080 '+' '.join(MAILRU_PROFILE)+'\n'
            'Restart=on-failure\nRestartSec=5\nUMask=0077\nNoNewPrivileges=yes\nPrivateTmp=yes\nPrivateDevices=yes\nProtectSystem=strict\nProtectHome=yes\n'
            'ProtectKernelTunables=yes\nProtectKernelModules=yes\nProtectControlGroups=yes\nRestrictSUIDSGID=yes\nLockPersonality=yes\n'
            'CapabilityBoundingSet=\nAmbientCapabilities=\n[Install]\nWantedBy=multi-user.target\n')

    def exit_unit_text(self):
        settings=mailru_exit_settings(self.node().get('mailru_exit'))
        # Like the Legacy exit: the release binary in a container with raw-socket
        # rights only, its output in this unit's journal. Images come from the
        # host only, and the container log is bounded.
        return ('[Unit]\nDescription=OpenFlux Mail.ru exit\nAfter=docker.service network-online.target\nRequires=docker.service\nWants=network-online.target\n'
            '[Service]\nType=simple\nEnvironmentFile='+MAILRU_ENV+'\nExecStartPre=-/usr/bin/docker rm -f '+MAILRU_CONTAINER+'\n'
            'ExecStart=/usr/bin/docker run --rm --pull=never --name '+MAILRU_CONTAINER+' --label io.openflux.managed=mailru --network='+settings['network']+' '
            '--cap-drop=ALL --cap-add=NET_RAW --cap-add=NET_ADMIN --security-opt=no-new-privileges --pids-limit=256 --memory=512m --memory-swap=512m '
            '--log-driver=json-file --log-opt=max-size=10m --log-opt=max-file=2 '
            '--mount=type=bind,src='+self.binary()+',dst=/usr/local/bin/openflux,readonly --mount=type=bind,src='+MAILRU_DATA+'/run.sh,dst=/run/openflux-mailru/run.sh,readonly '
            '--entrypoint=/bin/sh '+settings['image']+' /run/openflux-mailru/run.sh --exit-node --transport mailru --url ${MAILRU_DOCUMENT} '+' '.join(MAILRU_PROFILE)+'\n'
            'ExecStop=/usr/bin/docker stop --time 10 '+MAILRU_CONTAINER+'\n'
            # A Go program running as PID 1 exits with status 2 on SIGTERM.
            'SuccessExitStatus=2 143\nRestart=always\nRestartSec=5\nTimeoutStartSec=120\nTimeoutStopSec=30\n[Install]\nWantedBy=multi-user.target\n')

    def legacy_exit_settings(self):
        """The Legacy exit container's image and network: the Mail.ru exit's default."""
        import json
        p=self.run(['docker','inspect',self.node().get('container','')],check=False)
        if p.returncode:return None
        try:
            info=json.loads(p.stdout)[0];networks=list((info.get('NetworkSettings') or {}).get('Networks') or {})
            value={'image':(info.get('Config') or {}).get('Image'),'network':networks[0] if len(networks)==1 else None}
            return mailru_exit_settings(value)
        except (ValueError,IndexError,KeyError,TypeError,AttributeError):return None

    @staticmethod
    def write(path,data,mode):
        import os,tempfile
        path.parent.mkdir(parents=True,exist_ok=True,mode=0o700)
        fd,tmp=tempfile.mkstemp(prefix='.mailru-',dir=path.parent)
        try:
            with os.fdopen(fd,'wb') as f:f.write(data);f.flush();os.fsync(f.fileno())
            os.chmod(tmp,mode);os.replace(tmp,path)
        finally:
            if os.path.exists(tmp):os.unlink(tmp)

    def write_units(self):
        """Write this node's Mail.ru unit (and a server's exit script) for the
        current release; True when anything changed. Callers reload systemd."""
        server=self.node()['role']=='server'
        files=[(self.units/self.unit(),(self.exit_unit_text() if server else self.unit_text()).encode(),0o644)]
        if server:files.append((self.data/'run.sh',MAILRU_EXIT_SCRIPT.encode(),0o755))
        changed=False
        for path,data,mode in files:
            if not path.exists() or path.read_bytes()!=data:self.write(path,data,mode);changed=True
        return changed

    def configure(self,document,exit_settings=None):
        """Save the document link and write the unit. A server's exit then starts
        and must connect; any failure restores the previous files and state."""
        document=mailru_document(document);node=self.node();server=node['role']=='server'
        if server:exit_settings=mailru_exit_settings(exit_settings or node.get('mailru_exit') or self.legacy_exit_settings())
        usage=self.run([self.binary(),'--help'],check=False)
        if 'mailru-editor-activity' not in usage.stdout+usage.stderr:raise ValueError('The installed OpenFlux binary has no Mail.ru transport; update OpenFlux first')
        unit=self.units/self.unit()
        paths=[self.env_path,unit,self.node_path]+([self.data/'run.sh'] if server else [])
        before={p:(p.read_bytes(),p.stat().st_mode&0o777) if p.exists() else None for p in paths}
        existed=before[unit] is not None;was_active=existed and self.active()
        env=('MAILRU_DOCUMENT='+document+'\n').encode();changed=before[self.env_path] is None or before[self.env_path][0]!=env
        try:
            self.write(self.env_path,env,0o600)
            if server and node.get('mailru_exit')!=exit_settings:self.save(self.node_path,dict(node,mailru_exit=exit_settings))
            units_changed=self.write_units()
            self.run(['systemctl','daemon-reload'])
            if not server:
                # The router helpers must accept the Mail.ru unit before it is selected.
                VolgaRuntime(self.root).install_router_hooks()
                if was_active and changed:return {'ok':True,'changed':True,'message':'Mail.ru link saved. Apply the Mail.ru protocol again (or restart OPENFLUX) to use the new document.'}
                return {'ok':True,'changed':changed,'message':'Mail.ru link saved. Select Mail.ru as the protocol to use it; the server’s Mail.ru exit must use the same document.'}
            self.run(['systemctl','enable',MAILRU_EXIT_UNIT])
            if changed or units_changed or not was_active:self.run(['systemctl','restart',MAILRU_EXIT_UNIT],timeout=150)
            self.health()
            return {'ok':True,'changed':changed or units_changed,'message':'Mail.ru exit is running and connected to the document.'}
        except BaseException:
            if server:
                self.run(['systemctl','stop',MAILRU_EXIT_UNIT],check=False,timeout=60)
                if not existed:self.run(['systemctl','disable',MAILRU_EXIT_UNIT],check=False)
            for path,saved in before.items():
                if saved is None:path.unlink(missing_ok=True)
                elif not path.exists() or path.read_bytes()!=saved[0]:self.write(path,*saved)
            self.run(['systemctl','daemon-reload'],check=False)
            if server and was_active:self.run(['systemctl','start',MAILRU_EXIT_UNIT],check=False,timeout=150)
            raise

    def action(self,action):
        if action not in ('start','stop','restart'):raise ValueError('Invalid runtime action')
        if not self.installed():raise ValueError('Set up Mail.ru first')
        return self.run(['systemctl',action,self.unit()],timeout=150)

    @classmethod
    def classify(cls,message):
        import re
        for needle,state,reason in cls.EVENTS:
            if needle in message:
                code=re.search(r'(?:status|http) ([1-5]\d\d)\b',message)
                if code and state!='connected':reason=reason.replace(';',' (HTTP '+code[1]+');',1)
                return state,reason
        return None

    def status(self):
        import fcntl,json,re,time
        unit=self.unit()
        out={'transport':'mailru','state':'stopped','active':False,'installed':(self.units/unit).is_file(),'configured':False,'reason':'Mail.ru is stopped.'}
        try:
            document=self.document()
            if document:out.update(configured=True,documents=[document])
        except ValueError:out['reason']='Mail.ru configuration needs attention.'
        # The server's signed status, fetched from Yandex Disk (status_exchange).
        peer=self.read(self.data/'peer-status.json') if self.role()=='client' else None
        if peer:
            import openflux_auth
            peer['stale']=bool(peer.get('fetch_failed')) or time.time()-peer.get('reported_at',0)>openflux_auth.STATUS_STALE_AFTER
            out['server_authentication']=peer
        p=self.run(['systemctl','show',unit,'-p','ActiveState','-p','InvocationID'],check=False)
        d=dict(line.split('=',1) for line in p.stdout.splitlines() if '=' in line)
        if d.get('ActiveState')!='active':return out
        out.update(active=True,state='connecting',reason='Mail.ru is connecting.')
        invocation=d.get('InvocationID','')
        if not re.fullmatch('[0-9a-f]{32}',invocation):return out
        # The client logs a stats line every 5 s: read only what is new since the
        # last look, never the whole run again.
        self.data.mkdir(parents=True,exist_ok=True,mode=0o700)
        with open(self.data/'.status.lock','a') as lock:
            fcntl.flock(lock,fcntl.LOCK_EX)
            saved=self.read(self.data/'status.json',{}) or {}
            if saved.get('invocation')!=invocation:saved={'invocation':invocation}
            argv=['journalctl','--no-pager','-o','json','_SYSTEMD_INVOCATION_ID='+invocation,'--grep','|'.join(re.escape(e[0]) for e in self.EVENTS)]
            if saved.get('scanned_at'):argv+=['--since','@%d'%int(saved['scanned_at']-2)]
            started=time.time();p=self.run(argv,check=False,timeout=20)
            for line in p.stdout.splitlines():
                try:e=json.loads(line);ts=int(e['__REALTIME_TIMESTAMP'])/1e6;message=e.get('MESSAGE')
                except (ValueError,KeyError,TypeError):continue
                if not isinstance(message,str) or ts<=saved.get('event_at',0):continue
                event=self.classify(message)
                if not event:continue
                saved.update(state=event[0],reason=event[1],event_at=ts)
                if event[0]=='connected':saved['connected_at']=ts
            if p.returncode in (0,1):saved['scanned_at']=started
            self.save(self.data/'status.json',saved)
        if saved.get('state'):out.update(state=saved['state'],reason=saved['reason'])
        if saved.get('connected_at'):out['connected_at']=int(saved['connected_at'])
        out['checked_at']=int(time.time())
        return out

    def health(self,seconds=90):
        import time
        deadline=time.monotonic()+seconds;client=self.node()['role']=='client'
        while time.monotonic()<deadline:
            s=self.status()
            if not s['active']:raise RuntimeError('Mail.ru stopped during health check')
            # A server is ready once it holds the document; a client also proves
            # the exit through its SOCKS port.
            if s['state']=='connected' and (not client or VolgaRuntime(self.root).exit_ok()):return
            time.sleep(1)
        raise RuntimeError('Mail.ru did not pass connection and exit-IP checks')

def run_mailru_cli(args):
    import contextlib,fcntl,pathlib
    runtime=MailruRuntime();action=args.action
    if action in ('check','download','update','rollback'):raise ValueError('Mail.ru is updated together with OpenFlux: run this without --transport')
    with contextlib.ExitStack() as stack:
        locks=['/run/openflux-volga.lock']
        if action=='setup-mailru':locks=['/run/router-updater.lock','/run/lock/router-mode.lock','/run/openflux-update.lock']+locks
        for name in locks:
            pathlib.Path(name).parent.mkdir(parents=True,exist_ok=True)
            lock=stack.enter_context(open(name,'a'))
            try:fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
            except BlockingIOError:raise ValueError('Another router/Volga operation is running; retry shortly') from None
        if action=='status':return runtime.status()
        if action=='setup-mailru':
            settings=None
            if runtime.node()['role']=='server' and (getattr(args,'image',None) or getattr(args,'network',None)):
                current=runtime.node().get('mailru_exit') or runtime.legacy_exit_settings() or {}
                settings={'image':args.image or current.get('image'),'network':args.network or current.get('network')}
            return runtime.configure(args.document_url,settings)
        if action in ('start','stop','restart'):
            node=runtime.node()
            if node['role']=='client' and action in ('start','restart') and node.get('active_transport','yandex')!='mailru':raise ValueError('Use select-protocol mailru to switch clients safely')
            runtime.action(action);return {'ok':True,'action':action}
        if action=='health':runtime.health(max(1,min(args.seconds,150)));return {'ok':True,'health':'passed'}
        raise ValueError('Unsupported Mail.ru action')

# The router helpers accept only the units of known protocols (v4.2.0: Mail.ru).
SELECTOR_CASE_V1='case "$OPENFLUX_SELECTED_UNIT" in openflux-yandex-client.service|openflux-volga-client.service) ;; *) echo "Invalid OpenFlux protocol unit" >&2; exit 1;; esac\n'
SELECTOR_CASE=SELECTOR_CASE_V1.replace('|openflux-volga-client.service)','|openflux-volga-client.service|'+MAILRU_UNIT+')')
# router-core's health helper names the selected protocol in its messages.
HEALTH_NAMES_V1="def protocol():\n    return 'Volga' if UNIT=='openflux-volga-client.service' else 'Legacy'\n"
HEALTH_NAMES="def protocol():\n    return {'openflux-volga-client.service':'Volga','"+MAILRU_UNIT+"':'Mail.ru'}.get(UNIT,'Legacy')\n"

def patch_runtime_selector(text,python=False):
    import re
    marker='# openflux-protocol-selector-v1'
    if marker in text:
        # Helpers patched before v4.2.0 learn the Mail.ru unit; nothing else changes.
        old,new=(HEALTH_NAMES_V1,HEALTH_NAMES) if python else (SELECTOR_CASE_V1,SELECTOR_CASE)
        return text.replace(old,new) if text.count(old)==1 else text
    if python:
        old="UNIT='openflux-yandex-client.service'"
        if text.count(old)!=1:raise ValueError('Unknown router health unit declaration')
        new=marker+"\nimport sys\nsys.path.insert(0, '/usr/local/lib/router-updater')\ntry:\n    from router_integration import openflux_runtime_unit\n    UNIT=openflux_runtime_unit()\nexcept ImportError:\n    UNIT='openflux-yandex-client.service'"
        text=text.replace(old,new)
        return text.replace(HEALTH_NAMES_V1,HEALTH_NAMES) if text.count(HEALTH_NAMES_V1)==1 else text
    if not text.startswith('#!/usr/bin/env bash') or 'set -Eeuo pipefail' not in text:raise ValueError('Unknown router shell layout')
    refs=[line for line in text.splitlines() if 'openflux-yandex-client.service' in line]
    if not refs or any('systemctl' not in line and not re.fullmatch(r'(UNIT|CLIENT_UNIT|UNIT_CLIENT)="openflux-yandex-client.service"',line) for line in refs):raise ValueError('Unknown router unit reference')
    text=re.sub(r'(UNIT|CLIENT_UNIT|UNIT_CLIENT)="openflux-yandex-client.service"',r'\1="$OPENFLUX_SELECTED_UNIT"',text)
    text=text.replace('openflux-yandex-client.service','"$OPENFLUX_SELECTED_UNIT"')
    hook=marker+'\nOPENFLUX_SELECTED_UNIT="$(/usr/local/sbin/openfluxctl runtime-unit 2>/dev/null || echo openflux-yandex-client.service)"\n'+SELECTOR_CASE
    return text.replace('set -Eeuo pipefail\n','set -Eeuo pipefail\n'+hook,1)

def run_volga_cli(args):
    import contextlib,fcntl,json,pathlib,sys
    runtime=VolgaRuntime();action=args.action
    # One release, one updater and one channel: refuse before touching state.
    if action in ('check','download','update','rollback'):raise ValueError('Volga is updated together with OpenFlux: run this without --transport')
    if action in ('configure','setup-volga') and getattr(args,'channel',None):raise ValueError('Volga follows the OpenFlux release channel; set it without --transport')
    runtime.state.mkdir(parents=True,exist_ok=True,mode=0o700)
    with contextlib.ExitStack() as stack:
        locks=['/run/openflux-volga.lock']
        if action in ('setup-volga','select-protocol','recover-volga'):
            locks=['/run/router-updater.lock','/run/lock/router-mode.lock','/run/openflux-update.lock']+locks
        for name in locks:
            pathlib.Path(name).parent.mkdir(parents=True,exist_ok=True)
            lock=stack.enter_context(open(name,'a'))
            try:fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
            except BlockingIOError:raise ValueError('Another router/Volga operation is running; retry shortly') from None
        if action=='status':return runtime.status()
        if action=='configure':
            if not args.config_file:raise ValueError('Volga configure requires --config-file')
            return runtime.configure(runtime.read(pathlib.Path(args.config_file)))
        if action=='setup-volga':
            profile=runtime.node();profile['volga_resources']={'memory_mib':args.memory_mib,'cpu_percent':args.cpu_percent}
            # Validate limits before changing the node; preserve all Legacy fields.
            old=runtime.node();old_config=runtime.read(runtime.config_path);runtime.save(runtime.node_path,profile)
            try:
                runtime.limits();return runtime.configure(runtime.read(pathlib.Path(args.config_file)))
            except BaseException:
                runtime.save(runtime.node_path,old)
                if old_config is None:runtime.config_path.unlink(missing_ok=True)
                else:runtime.save(runtime.config_path,old_config)
                raise
        if action=='recover-volga':
            record=runtime.read(runtime.state/'transaction.json')
            return runtime.restore_transaction(record) if record else {'ok':True,'message':'No interrupted Volga operation.'}
        if action=='select-protocol':return runtime.select(args.protocol)
        if action=='poll-volga-recovery':
            # The same timer carries Mail.ru's status: Mail.ru has no inbox of its own.
            try:MailruRuntime().status_exchange()
            except Exception as e:print('Mail.ru status exchange failed: '+type(e).__name__,file=sys.stderr)
            runtime.recovery_poll();return {'ok':True}
        if action in ('start','stop','restart'):
            if runtime.node()['role']=='client' and action in ('start','restart') and runtime.node().get('active_transport','yandex')!='volga':raise ValueError('Use select-protocol volga to switch clients safely')
            runtime.action(action);return {'ok':True,'action':action}
        if action=='health':runtime.health(max(1,min(args.seconds,150)));return {'ok':True,'health':'passed'}
        if action=='cookies':
            text=sys.stdin.read(120001) if args.file=='-' else pathlib.Path(args.file).read_text()
            result=runtime.import_cookies(args.document_url,text) if args.operation=='import' else runtime.package_cookies(args.document_url,text,upload=args.operation=='send')
            if args.output and 'package' in result:runtime.save(pathlib.Path(args.output),result['package']);return {'saved':args.output}
            return result
        if action=='recovery':
            import openflux_auth
            options=vars(args).copy();options['disk_path']=None
            result=openflux_auth.configure_recovery(options)
            if args.disk_path:
                old=runtime.node();changed=dict(old,volga_recovery_path=args.disk_path);runtime.save(runtime.node_path,changed)
                try:runtime.disk_path()
                except BaseException:runtime.save(runtime.node_path,old);raise
            runtime.install_recovery_timer();result['disk_path']=runtime.disk_path();return result
        raise ValueError('Unsupported Volga action')

# rc1/rc2 Volga blocks, kept verbatim so those panels can be migrated.
VOLGA_HTML_V1='''
  <div id="openfluxProtocolControls" class="row" style="margin-top:12px">
   <label>Protocol <select id="openfluxProtocol"><option value="yandex">Yandex Legacy</option><option value="volga">Volga</option></select></label>
   <button id="openfluxProtocolApply">Apply protocol</button>
   <span class="small" id="openfluxProtocolResult" role="status"></span>
  </div>
  <details id="volgaSettings" style="margin-top:12px">
   <summary>Volga setup, updates and browser cookies</summary>
   <p class="small">Volga uses two Yandex documents and a matching shared key on client/server. One mini-PC may use this document pair at a time: stop OpenFlux/Volga on the other mini-PC before connecting. Existing recovery keys are reused.</p>
   <div class="row"><button id="volgaConfigDownload">Download Volga config</button><label>Upload Volga config <input id="volgaConfigUpload" type="file" accept=".json"></label></div>
   <p class="small">Configuration downloads include the shared key. Keep them private. Performance parameters are fixed.</p>
   <div class="row"><button data-volga-update="check">Check Volga release</button><button data-volga-update="download">Download update</button><button data-volga-update="update">Install / update Volga</button><button data-volga-update="rollback">Roll back Volga</button></div>
   <p class="small" id="volgaUpdateResult" role="status"></p>
   <label>Document <select id="volgaDocument"></select></label>
   <p class="small">Open this document in the browser, complete any verification, then paste its Copy as cURL (bash) request. Repeat for the second document.</p>
   <textarea id="volgaCurl" rows="4" autocomplete="off" spellcheck="false" style="width:100%;box-sizing:border-box" placeholder="Copy as cURL (bash)"></textarea>
   <div class="row"><button id="volgaCookies">Save client cookies</button><button id="volgaRecoveryDownload">Download encrypted server cookies</button><button id="volgaRecoverySend">Send server cookies via Disk</button></div>
   <p class="small" id="volgaCookieResult" role="status"></p>
  </details>'''

VOLGA_JS_V1='''
async function volgaPost(path,body){return api('/api/openflux/'+path,{method:'POST',headers:{'Content-Type':'application/json','X-Router-Panel':'1'},body:JSON.stringify(body)});}
function volgaDownload(data,name){const u=URL.createObjectURL(new Blob([JSON.stringify(data,null,2)],{type:'application/json'})),a=document.createElement('a');a.href=u;a.download=name;a.click();setTimeout(()=>URL.revokeObjectURL(u),1000);}
$('#openfluxProtocolApply').onclick=async()=>{const b=$('#openfluxProtocolApply'),r=$('#openfluxProtocolResult');b.disabled=true;r.textContent='Checking connection; previous protocol will return if it fails…';try{const d=await volgaPost('protocol',{protocol:$('#openfluxProtocol').value});r.textContent=d.message;refresh();}catch(e){r.textContent=e.message;}finally{b.disabled=false;}};
$('#volgaConfigDownload').onclick=async()=>{try{const d=await volgaPost('volga/config',{operation:'download'});volgaDownload(d.config,'openflux-volga-config.json');}catch(e){$('#volgaUpdateResult').textContent=e.message;}};
$('#volgaConfigUpload').onchange=async(e)=>{const f=e.target.files[0];if(!f)return;try{if(f.size>65536)throw Error('Config must be at most 64 KiB');const d=await volgaPost('volga/config',{operation:'upload',config:JSON.parse(await f.text())});$('#volgaUpdateResult').textContent=d.message;refresh();}catch(error){$('#volgaUpdateResult').textContent=error.message;}finally{e.target.value='';}};
document.querySelectorAll('[data-volga-update]').forEach(b=>b.onclick=async()=>{const r=$('#volgaUpdateResult');b.disabled=true;r.textContent='Working…';try{const d=await volgaPost('volga/update',{action:b.dataset.volgaUpdate});r.textContent=d.message||('Volga release: '+(d.version||'unknown'));refresh();}catch(e){r.textContent=e.message;}finally{b.disabled=false;}});
async function volgaCookies(operation){const r=$('#volgaCookieResult');r.textContent='Saving Volga cookies…';try{const d=await volgaPost(operation==='import'?'volga/cookies':'volga/recovery',{document:$('#volgaDocument').value,curl:$('#volgaCurl').value,upload:operation==='send'});if(d.package)volgaDownload(d.package,d.filename);$('#volgaCurl').value='';r.textContent=d.message;refresh();}catch(e){r.textContent=e.message;}}
$('#volgaCookies').onclick=()=>volgaCookies('import');$('#volgaRecoveryDownload').onclick=()=>volgaCookies('package');$('#volgaRecoverySend').onclick=()=>volgaCookies('send');
'''

VOLGA_HOOK_V1="\n  const vp=of.protocols?.volga||{}, selected=of.active_transport||'yandex';\n  if(document.activeElement!==$('#openfluxProtocol'))$('#openfluxProtocol').value=selected;\n  $('#openfluxCookies').hidden=selected==='volga';$('#openfluxServerRecovery').hidden=selected==='volga';\n  const vd=$('#volgaDocument'), oldDocument=vd.value, docs=vp.documents||[];\n  if(JSON.stringify([...vd.options].map(o=>o.value))!==JSON.stringify(docs)){vd.replaceChildren(...docs.map((u,i)=>{const o=document.createElement('option');o.value=u;o.textContent='Document '+(i+1)+' · '+u;return o;}));if(docs.includes(oldDocument))vd.value=oldDocument;}\n"

# rc3 Volga blocks, kept verbatim so those panels can be migrated.
VOLGA_HTML_V2='''
  <div id="openfluxProtocolControls" class="row" style="margin-top:12px" data-volga-panel="2">
   <label>Protocol <select id="openfluxProtocol"><option value="yandex">Yandex Legacy</option><option value="volga">Volga</option></select></label>
   <button id="openfluxProtocolApply">Apply protocol</button>
   <span class="small" id="openfluxProtocolResult" role="status"></span>
  </div>
  <p class="small" id="volgaOneDevice" role="note" style="margin:8px 0 0;padding:6px 10px;border-left:4px solid #d97706;background:rgba(217,119,6,.14)"><b>Volga: one mini-PC at a time.</b> Stop OpenFlux/Volga on the other mini-PC before selecting Volga here. A second mini-PC ends the first one&#39;s session.</p>
  <details id="volgaSettings" style="margin-top:12px">
   <summary>Volga browser cookies</summary>
   <p class="small">Volga uses two Yandex documents and a matching shared key on client/server. Volga is updated with OpenFlux in Router Updater; its configuration file is under Configuration.</p>
   <div class="row" id="volgaSetup" hidden><button id="volgaConfigDownload">Download Volga config template</button><label>Upload Volga config <input id="volgaConfigUpload" type="file" accept=".json"></label></div>
   <p class="small" id="volgaUpdateResult" role="status"></p>
   <label>Document <select id="volgaDocument"></select></label>
   <p class="small">Open this document in the browser, complete any verification, then paste its Copy as cURL (bash) request. Repeat for the second document.</p>
   <textarea id="volgaCurl" rows="4" autocomplete="off" spellcheck="false" style="width:100%;box-sizing:border-box" placeholder="Copy as cURL (bash)"></textarea>
   <div class="row"><button id="volgaCookies">Save client cookies</button><button id="volgaRecoveryDownload">Download encrypted server cookies</button><button id="volgaRecoverySend">Send server cookies via Disk</button></div>
   <p class="small" id="volgaCookieResult" role="status"></p>
  </details>'''

VOLGA_JS_V2='''
async function volgaPost(path,body){return api('/api/openflux/'+path,{method:'POST',headers:{'Content-Type':'application/json','X-Router-Panel':'1'},body:JSON.stringify(body)});}
function volgaDownload(data,name){const u=URL.createObjectURL(new Blob([JSON.stringify(data,null,2)],{type:'application/json'})),a=document.createElement('a');a.href=u;a.download=name;a.click();setTimeout(()=>URL.revokeObjectURL(u),1000);}
$('#openfluxProtocolApply').onclick=async()=>{if($('#openfluxProtocol').value==='volga'&&!confirm('Volga works on one mini-PC at a time. Is OpenFlux/Volga stopped on the other mini-PC?'))return;const b=$('#openfluxProtocolApply'),r=$('#openfluxProtocolResult');b.disabled=true;r.textContent='Checking connection; previous protocol will return if it fails…';try{const d=await volgaPost('protocol',{protocol:$('#openfluxProtocol').value});r.textContent=d.message;refresh();}catch(e){r.textContent=e.message;}finally{b.disabled=false;}};
$('#volgaConfigDownload').onclick=async()=>{try{const d=await volgaPost('volga/config',{operation:'download'});volgaDownload(d.config,'openflux-volga-config.json');}catch(e){$('#volgaUpdateResult').textContent=e.message;}};
$('#volgaConfigUpload').onchange=async(e)=>{const f=e.target.files[0];if(!f)return;try{if(f.size>65536)throw Error('Config must be at most 64 KiB');const d=await volgaPost('volga/config',{operation:'upload',config:JSON.parse(await f.text())});$('#volgaUpdateResult').textContent=d.message;refresh();}catch(error){$('#volgaUpdateResult').textContent=error.message;}finally{e.target.value='';}};
async function volgaCookies(operation){const r=$('#volgaCookieResult');r.textContent='Saving Volga cookies…';try{const d=await volgaPost(operation==='import'?'volga/cookies':'volga/recovery',{document:$('#volgaDocument').value,curl:$('#volgaCurl').value,upload:operation==='send'});if(d.package)volgaDownload(d.package,d.filename);$('#volgaCurl').value='';r.textContent=d.message;refresh();}catch(e){r.textContent=e.message;}}
$('#volgaCookies').onclick=()=>volgaCookies('import');$('#volgaRecoveryDownload').onclick=()=>volgaCookies('package');$('#volgaRecoverySend').onclick=()=>volgaCookies('send');
'''

VOLGA_HOOK_V2=VOLGA_HOOK_V1+"  $('#volgaSetup').hidden=!!vp.configured;\n"

# One "browser cookies" section per protocol: this mini-PC's and the server's
# state, the document links and every cookie action. Both stay visible because
# the server needs fresh cookies for either protocol, whichever one is selected.
VOLGA_HTML='''
  <div id="openfluxProtocolControls" class="row" style="margin-top:12px" data-volga-panel="3">
   <label>Protocol <select id="openfluxProtocol"><option value="yandex">Yandex Legacy</option><option value="volga">Volga</option></select></label>
   <button id="openfluxProtocolApply">Apply protocol</button>
   <span class="small" id="openfluxProtocolResult" role="status"></span>
  </div>
  <p class="small" id="volgaOneDevice" role="note" style="margin:8px 0 0;padding:6px 10px;border-left:4px solid #d97706;background:rgba(217,119,6,.14)"><b>Volga: one mini-PC at a time.</b> Stop OpenFlux/Volga on the other mini-PC before selecting Volga here. A second mini-PC ends the first one&#39;s session.</p>
  <details id="openfluxCookies" style="margin-top:12px">
   <summary style="cursor:pointer">Legacy browser cookies<span class="small" id="openfluxCookieBadge"></span></summary>
   <div id="openfluxDocLinks" style="margin-top:8px"></div>
   <div class="small" id="openfluxLastFailure" style="margin-top:4px"></div>
   <div class="small" id="openfluxServerState" style="margin-top:4px">Server authentication: checking…</div>
   <div class="small" id="openfluxServerFailure"></div>
   <p class="small">Open the document in Chrome/Edge and complete any verification. Press F12 → Network, reload the document, right-click its request → Copy → Copy as cURL (bash) and paste it below.</p>
   <textarea id="openfluxCurl" rows="5" autocomplete="off" spellcheck="false" placeholder="Paste Copy as cURL (bash)…" style="width:100%;box-sizing:border-box"></textarea>
   <div class="row" id="openfluxServerRecovery" style="margin-top:10px"><button id="openfluxCookieBoth">Save here and send to server</button><button id="openfluxCookieImport">Save on this mini-PC only</button><button id="openfluxRecoverySend">Send to server only</button><button id="openfluxRecoveryDownload">Download encrypted server file</button></div>
   <p class="small" id="openfluxCookieResult" role="status" aria-live="polite">No restart or reinstall required.</p>
   <p class="small" id="openfluxRecoveryResult" role="status" aria-live="polite"></p>
   <p class="small">The server copy is encrypted for the paired server and goes through your Yandex Disk folder; the server checks it once a minute. Pairing and the Disk path are configured with openfluxctl recovery.</p>
  </details>
  <details id="volgaSettings" style="margin-top:12px">
   <summary style="cursor:pointer">Volga browser cookies<span class="small" id="volgaCookieBadge"></span></summary>
   <div id="volgaDocLinks" style="margin-top:8px"></div>
   <div class="small" id="volgaClientState" style="margin-top:4px"></div>
   <div class="small" id="volgaServerState" style="margin-top:4px">Volga server: checking…</div>
   <p class="small">Volga keeps its own cookies, separate from Legacy, for each of its two documents. Choose a document, open it in the browser, complete any verification and paste its Copy as cURL (bash); then repeat for the other document.</p>
   <div class="row" id="volgaSetup" hidden><button id="volgaConfigDownload">Download Volga config template</button><label>Upload Volga config <input id="volgaConfigUpload" type="file" accept=".json"></label></div>
   <p class="small" id="volgaUpdateResult" role="status"></p>
   <label>Document <select id="volgaDocument"></select></label>
   <textarea id="volgaCurl" rows="4" autocomplete="off" spellcheck="false" style="width:100%;box-sizing:border-box" placeholder="Copy as cURL (bash)"></textarea>
   <div class="row"><button id="volgaCookieBoth">Save here and send to server</button><button id="volgaCookies">Save on this mini-PC only</button><button id="volgaRecoverySend">Send to server only</button><button id="volgaRecoveryDownload">Download encrypted server file</button></div>
   <p class="small" id="volgaCookieResult" role="status"></p>
   <p class="small">Volga is updated with OpenFlux in Router Updater; its configuration file (documents and shared key) is under Configuration.</p>
  </details>'''
VOLGA_HTML_V3=VOLGA_HTML
VOLGA_HTML=VOLGA_HTML_V3.replace('data-volga-panel="3"','data-volga-panel="4"')

# No backslashes or template literals: the panel embeds this in a Python string.
VOLGA_JS=VOLGA_JS_V2+'''async function cookiesBoth(button,field,result,save,send){
 const text=field.value;let saved=null;button.disabled=true;result.textContent='Saving on this mini-PC…';
 try{saved=await save(text);result.textContent=saved.message+' Sending to the server…';const sent=await send(text);field.value='';result.textContent=saved.message+' '+sent.message;refresh();}
 catch(e){result.textContent=(saved?'Saved on this mini-PC; sending to the server failed: ':'')+e.message;}finally{button.disabled=false;}
}
function cookieLinks(box,urls){
 const key=JSON.stringify(urls);if(box.dataset.urls===key)return;box.dataset.urls=key;
 box.replaceChildren(...urls.map((u,i)=>{const row=document.createElement('div');row.className='small';row.append((urls.length>1?'Document '+(i+1):'Document')+': ');
  if(typeof u==='string'&&u.startsWith('https://')&&!u.includes(' ')){const a=document.createElement('a');a.href=u;a.target='_blank';a.rel='noopener noreferrer';a.textContent=u;row.append(a);}else row.append(String(u));
  return row;}));
}
$('#openfluxCookieBoth').onclick=()=>cookiesBoth($('#openfluxCookieBoth'),$('#openfluxCurl'),$('#openfluxCookieResult'),t=>volgaPost('cookies',{curl:t}),t=>volgaPost('recovery',{curl:t,upload:true}));
$('#volgaCookieBoth').onclick=()=>cookiesBoth($('#volgaCookieBoth'),$('#volgaCurl'),$('#volgaCookieResult'),t=>volgaPost('volga/cookies',{document:$('#volgaDocument').value,curl:t}),t=>volgaPost('volga/recovery',{document:$('#volgaDocument').value,curl:t,upload:true}));
'''

VOLGA_HOOK_V3=VOLGA_HOOK_V1.replace("  $('#openfluxCookies').hidden=selected==='volga';$('#openfluxServerRecovery').hidden=selected==='volga';\n",'')+'''  $('#volgaSetup').hidden=!!vp.configured;
  const legacyAuth=of.protocols?.yandex||of, volgaServer=vp.server_authentication||{};
  const cookieServer=p=>!p.state||p.stale?'unknown':(({connected:'connected',connecting:'connecting',auth_blocked:'AUTH_BLOCKED',auth_failed:'authentication failed',stopped:'stopped'})[p.state]||p.state);
  const cookieBadge=(el,active,client,server)=>{const warn=client==='auth_blocked'||(server.state==='auth_blocked'||server.state==='auth_failed')&&!server.stale;el.textContent=(active?' · selected':'')+' · '+(client==='auth_blocked'?'this mini-PC: AUTH_BLOCKED':'server: '+cookieServer(server));el.style.color=warn?'#d97706':'';el.style.fontWeight=warn?'600':'';};
  cookieBadge($('#openfluxCookieBadge'),selected==='yandex',legacyAuth.state,legacyAuth.server_authentication||{});cookieBadge($('#volgaCookieBadge'),selected==='volga',vp.state,volgaServer);
  cookieLinks($('#openfluxDocLinks'),legacyAuth.document_url?[legacyAuth.document_url]:[]);cookieLinks($('#volgaDocLinks'),docs);
  $('#volgaClientState').textContent='This mini-PC: '+(!vp.configured?'Volga is not configured yet':({connected:'Connected',connecting:'Connecting',auth_blocked:'AUTH_BLOCKED — save fresh Volga cookies here',stopped:'Stopped'})[vp.state]||'Unknown');
  $('#volgaServerState').textContent='Volga server: '+(!volgaServer.state||volgaServer.stale?'Unknown — no fresh verified response':({connected:'Running · client session active',connecting:'Running · waiting for a client',auth_blocked:'AUTH_BLOCKED — send fresh server cookies',stopped:'Stopped'})[volgaServer.state]||'Unknown')+(volgaServer.reported_at?' · Last response: '+new Date(volgaServer.reported_at*1000).toLocaleString():'');
  $('#openfluxCookieBoth').disabled=$('#volgaCookieBoth').disabled=$('#volgaRecoverySend').disabled=!legacyAuth.recovery_upload_ready;$('#volgaRecoveryDownload').disabled=!legacyAuth.recovery_sender_ready;
'''

# v4: an idle Volga server reports "connecting" all the time; its heading says
# "waiting for a client" and warns only when it is blocked or stopped.
VOLGA_HOOK=VOLGA_HOOK_V3.replace('''  const cookieServer=p=>!p.state||p.stale?'unknown':(({connected:'connected',connecting:'connecting',auth_blocked:'AUTH_BLOCKED',auth_failed:'authentication failed',stopped:'stopped'})[p.state]||p.state);
  const cookieBadge=(el,active,client,server)=>{const warn=client==='auth_blocked'||(server.state==='auth_blocked'||server.state==='auth_failed')&&!server.stale;el.textContent=(active?' · selected':'')+' · '+(client==='auth_blocked'?'this mini-PC: AUTH_BLOCKED':'server: '+cookieServer(server));el.style.color=warn?'#d97706':'';el.style.fontWeight=warn?'600':'';};
  cookieBadge($('#openfluxCookieBadge'),selected==='yandex',legacyAuth.state,legacyAuth.server_authentication||{});cookieBadge($('#volgaCookieBadge'),selected==='volga',vp.state,volgaServer);
''','''  const cookieServer=(p,names)=>!p.state||p.stale?'unknown':(names[p.state]||p.state);
  const cookieBadge=(el,active,client,server,names,alarms)=>{const warn=client==='auth_blocked'||alarms.includes(server.state)&&!server.stale;el.textContent=(active?' · selected':'')+' · '+(client==='auth_blocked'?'this mini-PC: AUTH_BLOCKED':'server: '+cookieServer(server,names));el.style.color=warn?'#d97706':'';el.style.fontWeight=warn?'600':'';};
  cookieBadge($('#openfluxCookieBadge'),selected==='yandex',legacyAuth.state,legacyAuth.server_authentication||{},{connected:'connected',connecting:'connecting',auth_blocked:'AUTH_BLOCKED',auth_failed:'authentication failed',stopped:'stopped'},['auth_blocked','auth_failed']);
  cookieBadge($('#volgaCookieBadge'),selected==='volga',vp.state,volgaServer,{connected:'client connected',connecting:'waiting for a client',auth_blocked:'AUTH_BLOCKED',stopped:'stopped'},['auth_blocked','stopped']);
''')

# v5: one browser login per place. The combined "save here and send to
# server" button put one session on the server and on this mini-PC at once;
# Yandex treats such a session as stolen. The sections say when the server
# picks cookies up and show a Disk token that Yandex refused.
VOLGA_HTML_V4,VOLGA_JS_V4,VOLGA_HOOK_V4=VOLGA_HTML,VOLGA_JS,VOLGA_HOOK
SEPARATE_LOGINS='<p class="small">Use a separate browser login for the server and for each mini-PC, so that no session is used in two places. If you can, copy the server&#39;s cURL from a browser that goes online through the server.</p>'
VOLGA_HTML=(VOLGA_HTML_V4.replace('data-volga-panel="4"','data-volga-panel="5"')
    .replace('<button id="openfluxCookieBoth">Save here and send to server</button>','')
    .replace('<button id="volgaCookieBoth">Save here and send to server</button>','')
    .replace('<p class="small">The server copy is encrypted for the paired server and goes through your Yandex Disk folder; the server checks it once a minute. Pairing and the Disk path are configured with openfluxctl recovery.</p>',
             SEPARATE_LOGINS+'<p class="small">The server copy is encrypted for the paired server and goes through your Yandex Disk folder. The server picks it up within 2 minutes while it waits for cookies, otherwise within 15 minutes. Pairing and the Disk path are configured with openfluxctl recovery.</p><p class="small" id="openfluxDiskToken" role="status"></p>')
    .replace('<p class="small" id="volgaCookieResult" role="status"></p>','<p class="small" id="volgaCookieResult" role="status"></p>'+SEPARATE_LOGINS+'<p class="small" id="volgaDiskToken" role="status"></p>'))
VOLGA_JS=VOLGA_JS_V4[:VOLGA_JS_V4.index('async function cookiesBoth(')]+VOLGA_JS_V4[VOLGA_JS_V4.index('function cookieLinks('):VOLGA_JS_V4.index("$('#openfluxCookieBoth').onclick")]
VOLGA_HOOK=VOLGA_HOOK_V4.replace("$('#openfluxCookieBoth').disabled=$('#volgaCookieBoth').disabled=$('#volgaRecoverySend').disabled=!legacyAuth.recovery_upload_ready;",
    "$('#volgaRecoverySend').disabled=!legacyAuth.recovery_upload_ready;\n  for(const el of [$('#openfluxDiskToken'),$('#volgaDiskToken')]){el.textContent=legacyAuth.disk_token_rejected?'Yandex Disk refused the Disk OAuth token. Upload a new one under Configuration (OpenFlux · Disk OAuth token); the server cookies and status wait until then.':'';el.style.color='#d97706';}\n  ")

# v6: the router reports its last OPENFLUX start, restart or automatic return
# to the previous mode (router-core /var/lib/router-core/openflux-last.json)
# right under the protocol selector, for a day.
VOLGA_HTML_V5,VOLGA_JS_V5,VOLGA_HOOK_V5=VOLGA_HTML,VOLGA_JS,VOLGA_HOOK
VOLGA_HTML=VOLGA_HTML_V5.replace('data-volga-panel="5"','data-volga-panel="6"').replace(
    '<span class="small" id="openfluxProtocolResult" role="status"></span>\n  </div>',
    '<span class="small" id="openfluxProtocolResult" role="status"></span>\n  </div>\n  <p class="small" id="openfluxRouterEvent" role="status" style="margin:8px 0 0"></p>')
VOLGA_JS=VOLGA_JS_V5
VOLGA_HOOK=VOLGA_HOOK_V5+'''  const rev=of.router_event||null, revEl=$('#openfluxRouterEvent');
  if(rev&&rev.at&&Date.now()/1000-rev.at<86400){const warn=!rev.ok||rev.kind==='watchdog'||rev.kind==='fallback';revEl.textContent='Router · '+new Date(rev.at*1000).toLocaleString()+': '+rev.message;revEl.style.color=warn?'#d97706':'';revEl.style.fontWeight=warn?'600':'';}else revEl.textContent='';
'''

# v7: this mini-PC's last check of its own cookies stays on the panel after the
# client stops (it stops at once when Yandex refuses them). A refusal warns in
# the section, on its badge and on the OPENFLUX card until fresh cookies are
# saved or a later check passes; Legacy's old "last issue" line gives way to it.
VOLGA_HTML_V6,VOLGA_JS_V6,VOLGA_HOOK_V6=VOLGA_HTML,VOLGA_JS,VOLGA_HOOK
VOLGA_HTML=VOLGA_HTML_V6.replace('data-volga-panel="6"','data-volga-panel="7"')
VOLGA_JS=VOLGA_JS_V6
CLIENT_CHECK_JS='''  const checkText=(c,name)=>!c?'':c.result==='refused'?(c.cookies_saved_after?'fresh '+name+' cookies saved after Yandex refused the previous ones on '+new Date(c.at*1000).toLocaleString()+'; they are checked at the next start':new Date(c.at*1000).toLocaleString()+': Yandex did not accept these cookies (captcha or login) — save fresh '+name+' cookies on this mini-PC'):'last check '+new Date(c.at*1000).toLocaleString()+': cookies accepted';
  const refused=c=>!!c&&c.result==='refused'&&!c.cookies_saved_after;
  const clientBadge=p=>p.state==='stopped'&&refused(p.client_check)?'refused':p.state;
'''
VOLGA_HOOK=(VOLGA_HOOK_V6
    .replace("  const cookieServer=(p,names)=>","  "+CLIENT_CHECK_JS.strip()+"\n  const cookieServer=(p,names)=>")
    .replace("const warn=client==='auth_blocked'||alarms.includes(server.state)&&!server.stale;el.textContent=(active?' · selected':'')+' · '+(client==='auth_blocked'?'this mini-PC: AUTH_BLOCKED':'server: '+cookieServer(server,names));",
             "const warn=client==='auth_blocked'||client==='refused'||alarms.includes(server.state)&&!server.stale;el.textContent=(active?' · selected':'')+' · '+(client==='auth_blocked'?'this mini-PC: AUTH_BLOCKED':client==='refused'?'this mini-PC: cookies refused':'server: '+cookieServer(server,names));")
    .replace("cookieBadge($('#openfluxCookieBadge'),selected==='yandex',legacyAuth.state,","cookieBadge($('#openfluxCookieBadge'),selected==='yandex',clientBadge(legacyAuth),")
    .replace("cookieBadge($('#volgaCookieBadge'),selected==='volga',vp.state,","cookieBadge($('#volgaCookieBadge'),selected==='volga',clientBadge(vp),")
    .replace("stopped:'Stopped'})[vp.state]||'Unknown');\n",
             "stopped:'Stopped'})[vp.state]||'Unknown')+(vp.state==='stopped'&&vp.client_check?' · '+checkText(vp.client_check,'Volga'):'');\n"
             "  {const w=vp.state==='auth_blocked'||vp.state==='stopped'&&refused(vp.client_check);$('#volgaClientState').style.color=w?'#d97706':'';$('#volgaClientState').style.fontWeight=w?'600':'';}\n"
             "  if(of.state==='stopped'&&refused((selected==='volga'?vp:legacyAuth).client_check))$('#openfluxState').textContent='Stopped · Yandex refused this mini-PC’s '+(selected==='volga'?'Volga':'Legacy')+' cookies — save fresh ones below';\n"))
for _old in ("  const cookieServer=(p,names)=>","client==='refused'","clientBadge(legacyAuth),","clientBadge(vp),","checkText(vp.client_check,'Volga')"):
    assert _old in VOLGA_HOOK,_old

# v8: Mail.ru is the third protocol. It has an option in the selector and a
# section of its own: its document link, this mini-PC's state and a field to set
# the link. The one-mini-PC note covers it, and the OPENFLUX card's cookie
# warning belongs to the Yandex protocols only.
VOLGA_HTML_V7,VOLGA_JS_V7,VOLGA_HOOK_V7=VOLGA_HTML,VOLGA_JS,VOLGA_HOOK
MAILRU_HTML='''
  <details id="mailruSettings" style="margin-top:12px">
   <summary style="cursor:pointer">Mail.ru document<span class="small" id="mailruBadge"></span></summary>
   <div id="mailruDocLinks" style="margin-top:8px"></div>
   <div class="small" id="mailruClientState" style="margin-top:4px"></div>
   <p class="small">Mail.ru opens one public Mail.ru Cloud document as an anonymous visitor: no cookies and no login. The server&#39;s Mail.ru exit must use the same document. Paste the document&#39;s public link, or upload a Mail.ru config file, to set Mail.ru up or to change the document.</p>
   <div class="row"><input id="mailruDocument" type="url" autocomplete="off" spellcheck="false" placeholder="https://cloud.mail.ru/public/…" style="flex:1;min-width:16em"><button id="mailruSave">Save link</button></div>
   <div class="row" style="margin-top:8px"><button id="mailruConfigDownload">Download Mail.ru config</button><label>Upload Mail.ru config <input id="mailruConfigUpload" type="file" accept=".env,.txt"></label></div>
   <p class="small" id="mailruResult" role="status"></p>
   <p class="small">The config file (mailru.env, one line MAILRU_DOCUMENT=…) is also under Configuration as OpenFlux · Mail.ru document.</p>
  </details>'''
MAILRU_JS='''$('#mailruSave').onclick=async()=>{const b=$('#mailruSave'),r=$('#mailruResult');b.disabled=true;r.textContent='Saving the Mail.ru link…';try{const d=await volgaPost('mailru/config',{document:$('#mailruDocument').value.trim()});$('#mailruDocument').value='';r.textContent=d.message;refresh();}catch(e){r.textContent=e.message;}finally{b.disabled=false;}};
$('#mailruConfigDownload').onclick=async()=>{const r=$('#mailruResult');try{const d=await volgaPost('mailru/config',{operation:'download'});const u=URL.createObjectURL(new Blob([d.config],{type:'text/plain'})),a=document.createElement('a');a.href=u;a.download=d.filename;a.click();setTimeout(()=>URL.revokeObjectURL(u),1000);}catch(e){r.textContent=e.message;}};
$('#mailruConfigUpload').onchange=async(e)=>{const f=e.target.files[0],r=$('#mailruResult');if(!f)return;r.textContent='Saving the Mail.ru config…';try{if(f.size>4096)throw Error('The Mail.ru config must be at most 4 KiB');const d=await volgaPost('mailru/config',{operation:'upload',config:await f.text()});r.textContent=d.message;refresh();}catch(error){r.textContent=error.message;}finally{e.target.value='';}};
'''
MAILRU_HOOK='''  const mp=of.protocols?.mailru||{};
  $('#mailruConfigDownload').disabled=!mp.configured;
  cookieLinks($('#mailruDocLinks'),mp.documents||[]);
  $('#mailruBadge').textContent=(selected==='mailru'?' · selected':'')+' · '+(!mp.configured?'not set up':({connected:'connected',connecting:'connecting',stopped:'stopped'})[mp.state]||'unknown');
  $('#mailruClientState').textContent='This mini-PC: '+(!mp.configured?'Mail.ru is not set up yet':({connected:'Connected',connecting:'Connecting',stopped:'Stopped'})[mp.state]||'Unknown')+(mp.active&&mp.state!=='connected'&&mp.reason?' · '+mp.reason:'');
'''
VOLGA_HTML=(VOLGA_HTML_V7.replace('data-volga-panel="7"','data-volga-panel="8"')
    .replace('<option value="volga">Volga</option></select>','<option value="volga">Volga</option><option value="mailru">Mail.ru</option></select>')
    .replace('<b>Volga: one mini-PC at a time.</b> Stop OpenFlux/Volga on the other mini-PC before selecting Volga here.',
             '<b>Volga and Mail.ru: one mini-PC at a time.</b> Stop OpenFlux on the other mini-PC before selecting Volga or Mail.ru here.')
    +MAILRU_HTML)
VOLGA_JS=VOLGA_JS_V7.replace("if($('#openfluxProtocol').value==='volga'&&!confirm('Volga works on one mini-PC at a time. Is OpenFlux/Volga stopped on the other mini-PC?'))return;",
    "if(['volga','mailru'].includes($('#openfluxProtocol').value)&&!confirm('Volga and Mail.ru work on one mini-PC at a time. Is OpenFlux stopped on the other mini-PC?'))return;")+MAILRU_JS
VOLGA_HOOK=VOLGA_HOOK_V7.replace("  if(of.state==='stopped'&&refused((selected==='volga'?vp:legacyAuth).client_check))",
    "  if(of.state==='stopped'&&selected!=='mailru'&&refused((selected==='volga'?vp:legacyAuth).client_check))")+MAILRU_HOOK
for _new in ('data-volga-panel="8"','<option value="mailru">Mail.ru</option>','<b>Volga and Mail.ru: one mini-PC at a time.</b>'):
    assert VOLGA_HTML.count(_new)==1,_new
assert "['volga','mailru'].includes(" in VOLGA_JS and "selected!=='mailru'&&refused(" in VOLGA_HOOK

# v9: the Mail.ru section shows the server's Mail.ru exit from its signed
# status on Yandex Disk, like the Volga server, and its badge warns while the
# exit is not connected to the document.
VOLGA_HTML_V8,VOLGA_JS_V8,VOLGA_HOOK_V8=VOLGA_HTML,VOLGA_JS,VOLGA_HOOK
VOLGA_HTML=(VOLGA_HTML_V8.replace('data-volga-panel="8"','data-volga-panel="9"')
    .replace('<div class="small" id="mailruClientState" style="margin-top:4px"></div>',
             '<div class="small" id="mailruClientState" style="margin-top:4px"></div>\n   <div class="small" id="mailruServerState" style="margin-top:4px">Mail.ru server: checking…</div>'))
VOLGA_JS=VOLGA_JS_V8
MAILRU_SERVER_HOOK='''  const ms=mp.server_authentication||{}, msWarn=!!mp.configured&&!!ms.state&&!ms.stale&&ms.state!=='connected';
  $('#mailruServerState').textContent='Mail.ru server: '+(!mp.configured?'shown once Mail.ru is set up on this mini-PC':!ms.state||ms.stale?'Unknown — no fresh verified response':({connected:'Connected to the document',connecting:'Reconnecting'+(ms.status&&ms.status.reason?' — '+ms.status.reason:''),stopped:'Stopped'})[ms.state]||'Unknown')+(mp.configured&&ms.reported_at?' · Last response: '+new Date(ms.reported_at*1000).toLocaleString():'');
  $('#mailruServerState').style.color=msWarn?'#d97706':'';$('#mailruServerState').style.fontWeight=msWarn?'600':'';
  if(msWarn)$('#mailruBadge').textContent+=' · server: '+(({connecting:'reconnecting',stopped:'stopped'})[ms.state]||ms.state);
  $('#mailruBadge').style.color=msWarn?'#d97706':'';$('#mailruBadge').style.fontWeight=msWarn?'600':'';
'''
VOLGA_HOOK=VOLGA_HOOK_V8+MAILRU_SERVER_HOOK
assert VOLGA_HTML.count('id="mailruServerState"')==1 and VOLGA_HTML.count('data-volga-panel="9"')==1

# The mode buttons show why a mode did not start: the last line of the mode
# controller, e.g. "OPENFLUX was not started: AUTH_BLOCKED… AWG kept serving
# the LAN." Both router panel layouts get the same answer and timeout.
MODE_API=(
    ('ok,out,rc=run(["/usr/bin/env","ROUTER_UPDATER_LOCK_HELD=1",*cmd],400 if target=="openflux" else 180);actual=current_mode()',
     'ok,out,rc=run(["/usr/bin/env","ROUTER_UPDATER_LOCK_HELD=1",*cmd],600 if target=="openflux" else 180);actual=current_mode()'),
    ('ok,out,rc=run(["env","ROUTER_UPDATER_LOCK_HELD=1"]+cmd,340);actual=current_mode()',
     'ok,out,rc=run(["/usr/bin/env","ROUTER_UPDATER_LOCK_HELD=1",*cmd],600 if target=="openflux" else 180);actual=current_mode()'),
    ('return self.j({"ok":False,"error":f"{actual.upper()} remains selected; startup failed. Use Restart/Rotate or select another mode.","output":out[-5000:]},500)',
     r'''return self.j({"ok":False,"error":(f"{target.upper()} was not started; {actual.upper()} is active." if actual!=target else f"{actual.upper()} remains selected; startup failed. Use Restart/Rotate or select another mode.")+"\n"+(out.strip().splitlines() or [""])[-1][-600:],"output":out[-5000:]},500)'''),
    ('return self.j({"ok":False,"error":f"{actual.upper()} remains selected. Use Restart/Rotate to retry.\\n{out[-2000:]}","output":out[-5000:]},500)',
     r'''return self.j({"ok":False,"error":(f"{target.upper()} was not started; {actual.upper()} is active." if actual!=target else f"{actual.upper()} remains selected; startup failed. Use Restart/Rotate or select another mode.")+"\n"+(out.strip().splitlines() or [""])[-1][-600:],"output":out[-5000:]},500)'''))

def patch_mode_api(text):
    # Optional: a layout without these lines keeps its own handler.
    for old,new in MODE_API:
        if text.count(old)==1:text=text.replace(old,new)
    return text

# Older status lines read the selected protocol's status. Both sections are
# visible now, so the Legacy lines read Legacy's own status.
LEGACY_STATUS_V3=(
    ("$('#openfluxLastFailure').textContent=of.last_failure?'Last client authentication issue: '+new Date(of.last_failure.at*1000).toLocaleString()+' — '+of.last_failure.reason:'';",
     "$('#openfluxLastFailure').textContent=legacyAuth.last_failure?'Last client authentication issue: '+new Date(legacyAuth.last_failure.at*1000).toLocaleString()+' — '+legacyAuth.last_failure.reason:'';"),
    ("$('#openfluxRecoveryDownload').disabled=!of.recovery_sender_ready;","$('#openfluxRecoveryDownload').disabled=!legacyAuth.recovery_sender_ready;"),
    ("$('#openfluxRecoverySend').disabled=!of.recovery_upload_ready;","$('#openfluxRecoverySend').disabled=!legacyAuth.recovery_upload_ready;"),
    ("const peer=of.server_authentication||{}, ps=peer.status||{};","const peer=legacyAuth.server_authentication||{}, ps=peer.status||{};"))

# v7: the Legacy section's "last client authentication issue" stayed forever,
# even after later successes; it shows the last check's verdict instead.
LEGACY_CHECK_V7=(LEGACY_STATUS_V3[0][1],
    "{const lc=legacyAuth.client_check||null,el=$('#openfluxLastFailure');el.textContent=lc?'This mini-PC: '+checkText(lc,'Legacy'):'';el.style.color=refused(lc)?'#d97706':'';el.style.fontWeight=refused(lc)?'600':'';}")

def patch_legacy_check(text):
    old,new=LEGACY_CHECK_V7
    return text if new in text else replace(text,old,new)

def patch_volga_panel(text):
    if 'const CFG_GROUPS=' in text:
        # Grouped configuration cards: the Volga and Mail.ru files join OpenFlux.
        new="'openflux-updater','openflux-volga-config','openflux-mailru'],modes:['openflux']"
        if new not in text:
            old="'openflux-updater','openflux-volga-config'],modes:['openflux']"
            text=replace(text,old if old in text else "'openflux-updater'],modes:['openflux']",new)
        new="const CFG_SECRET=new Set(['openflux-disk-token','openflux-client-key','openflux-cookies','openflux-volga-config','openflux-mailru']);"
        if new not in text:
            old="const CFG_SECRET=new Set(['openflux-disk-token','openflux-client-key','openflux-cookies','openflux-volga-config']);"
            text=replace(text,old if old in text else "const CFG_SECRET=new Set(['openflux-disk-token','openflux-client-key','openflux-cookies']);",new)
    if 'data-volga-panel="9"' in text:return text
    if 'data-volga-panel="8"' in text:
        # A v4.2.0/v4.2.1 panel: the Mail.ru section shows the server's exit.
        text=replace(replace(replace(text,VOLGA_HTML_V8,VOLGA_HTML),VOLGA_JS_V8,VOLGA_JS),VOLGA_HOOK_V8,VOLGA_HOOK)
    elif 'data-volga-panel="7"' in text:
        # A v4.1.10/v4.1.11 panel: Mail.ru joins the selector and gets its section.
        text=replace(replace(replace(text,VOLGA_HTML_V7,VOLGA_HTML),VOLGA_JS_V7,VOLGA_JS),VOLGA_HOOK_V7,VOLGA_HOOK)
    elif 'data-volga-panel="6"' in text:
        # A v4.1.8/v4.1.9 panel: the last cookie check of this mini-PC stays visible.
        text=replace(replace(replace(text,VOLGA_HTML_V6,VOLGA_HTML),VOLGA_JS_V6,VOLGA_JS),VOLGA_HOOK_V6,VOLGA_HOOK)
    elif 'data-volga-panel="5"' in text:
        # A v4.1.6/v4.1.7 panel: the router's OPENFLUX events line appears.
        text=replace(replace(replace(text,VOLGA_HTML_V5,VOLGA_HTML),VOLGA_JS_V5,VOLGA_JS),VOLGA_HOOK_V5,VOLGA_HOOK)
    elif 'data-volga-panel="4"' in text:
        # A v4.1.2-v4.1.5 panel: the combined send button goes, the notes change.
        text=replace(replace(replace(text,VOLGA_HTML_V4,VOLGA_HTML),VOLGA_JS_V4,VOLGA_JS),VOLGA_HOOK_V4,VOLGA_HOOK)
    elif 'data-volga-panel="3"' in text:
        # A v4.1.0/v4.1.1 panel: it had the v4 script.
        text=replace(replace(replace(text,VOLGA_HTML_V3,VOLGA_HTML),VOLGA_JS_V4,VOLGA_JS),VOLGA_HOOK_V3,VOLGA_HOOK)
    else:
        # The Legacy cookie sections sit right after the Volga block. Exact matches
        # or no change at all: an unexpected layout makes the install roll back.
        legacy=COOKIE_HTML+RECOVERY_HTML
        if 'data-volga-panel="2"' in text:
            text=replace(replace(replace(text,VOLGA_HTML_V2+legacy,VOLGA_HTML),VOLGA_JS_V2,VOLGA_JS),VOLGA_HOOK_V2,VOLGA_HOOK)
        elif 'id="openfluxProtocolControls"' in text:
            # An rc1/rc2 panel.
            text=replace(replace(replace(text,VOLGA_HTML_V1+legacy,VOLGA_HTML),VOLGA_JS_V1,VOLGA_JS),VOLGA_HOOK_V1,VOLGA_HOOK)
        else:
            text=replace(text,'import openflux_auth','import openflux_auth\nfrom router_integration import openflux_runtime_unit')
            # Preserve card anchors and the existing bridge. Status queries follow the
            # selected unit, including older panels using names without .service.
            for literal in ('"openflux-yandex-client.service"',"'openflux-yandex-client.service'",'"openflux-yandex-client"',"'openflux-yandex-client'"):
                text=text.replace(literal,'openflux_runtime_unit()')
            marker='<div class="small" id="openfluxHealth" style="margin-top:8px"></div>'
            text=replace(text,marker+legacy,marker+VOLGA_HTML)
            marker="$('#openfluxState').textContent="
            start=text.index(marker);end=text.index('\n',start)
            text=text[:end]+VOLGA_HOOK+text[end:]
            text=replace(text,'refreshNetwork();setInterval(refreshNetwork,5000);',VOLGA_JS+'\nrefreshNetwork();setInterval(refreshNetwork,5000);')
        for old,new in LEGACY_STATUS_V3:text=replace(text,old,new)
    return patch_mode_api(patch_legacy_check(text))

# What a failed protocol switch tells the browser. Only these fixed texts are
# shown, never the exception itself: no browser text, credential, raw
# subprocess output or key path. Every switch restores the previous protocol.
PROTOCOL_ERRORS=(
    ('AUTH_BLOCKED','Legacy: Yandex asks for fresh browser cookies (captcha or login). Save fresh Legacy cookies on this mini-PC, then apply the protocol again.'),
    ('Volga requires fresh browser cookies','Volga: Yandex asks for fresh browser cookies (captcha or login). Save fresh Volga cookies on this mini-PC for both documents, then apply the protocol again.'),
    ('OpenFlux health check failed','Legacy did not connect within 100 s, or its exit IP was wrong. Check the server status in the Legacy section.'),
    ('Volga did not pass session and exit-IP checks','Volga did not connect within 90 s, or its exit IP was wrong. Check the Volga server status.'),
    ('OpenFlux stopped during its health check','The Legacy client stopped during its check; see openflux-clientctl logs on this mini-PC.'),
    ('Volga stopped during health check','The Volga client stopped during its check; see journalctl -u openflux-volga-client on this mini-PC.'),
    ('Install the verified Volga release first','Install the verified Volga release in Router Updater first.'),
    ('Mail.ru did not pass connection and exit-IP checks','Mail.ru did not connect within 90 s, or its exit IP was wrong. Check that the server’s Mail.ru exit runs with the same document link.'),
    ('Mail.ru stopped during health check','The Mail.ru client stopped during its check; see journalctl -u openflux-mailru-client on this mini-PC.'),
    ('Set up Mail.ru first','Mail.ru is not set up on this mini-PC: save the Mail.ru document link in the Mail.ru section first.'),
    ('This protocol is not installed or supported','This protocol is not installed or supported.'))
BUSY_ERROR='Another router or Volga operation is running; retry shortly.'
# What a failed Mail.ru link save tells the browser: fixed texts only.
MAILRU_SETUP_ERRORS=(
    ('MAILRU_DOCUMENT=<public link>','Upload a Mail.ru config file with one line: MAILRU_DOCUMENT=https://cloud.mail.ru/public/…'),
    ('Set up Mail.ru first','Mail.ru is not set up on this mini-PC yet: save its document link first.'),
    ('cloud.mail.ru/public/','Paste the public link of a Mail.ru Cloud document: https://cloud.mail.ru/public/… with nothing after it.'),
    ('Invalid Mail.ru document link','Paste the public link of a Mail.ru Cloud document: https://cloud.mail.ru/public/…'),
    ('has no Mail.ru transport','This mini-PC’s OpenFlux has no Mail.ru transport yet: install the current OpenFlux release in Router Updater first.'),
    ('Unknown router','The router helpers on this mini-PC have an unexpected layout; Mail.ru was not set up.'))

def mailru_setup_error(exc):
    text=str(exc)
    if 'Another router/Volga operation is running' in text or isinstance(exc,BlockingIOError):return BUSY_ERROR
    for needle,message in MAILRU_SETUP_ERRORS:
        if needle in text:return message
    return 'Mail.ru setup failed; the previous Mail.ru settings are kept. Check the link and the installed OpenFlux release.'

def protocol_error(exc):
    text=str(exc)
    if 'Another router/Volga operation is running' in text or isinstance(exc,BlockingIOError):return BUSY_ERROR
    for needle,message in PROTOCOL_ERRORS:
        if needle in text:return message+' The previous protocol is selected again.'
    return 'Protocol switch failed; the previous protocol is selected again. Check this protocol’s cookies, configuration and server status.'

def volga_panel_post(handler):
    import json,types,contextlib,fcntl
    paths=('/api/openflux/protocol','/api/openflux/volga/config','/api/openflux/volga/cookies','/api/openflux/volga/recovery','/api/openflux/volga/update','/api/openflux/mailru/config')
    if handler.path not in paths:return False
    if not handler.allowed() or not handler.authorized():handler.j({'ok':False,'error':'Request rejected'},403);return True
    if handler.path.endswith('/update'):
        # A page opened before the update still shows the old buttons.
        handler.j({'ok':False,'error':'Volga is updated together with OpenFlux in Router Updater.'},400);return True
    try:
        length=int(handler.headers.get('Content-Length','0'))
        if not 1<=length<=140000 or 'application/json' not in handler.headers.get('Content-Type',''):raise ValueError('Invalid request')
        body=json.loads(handler.rfile.read(length));runtime=VolgaRuntime()
        if not isinstance(body,dict):raise ValueError('JSON object required')
        if handler.path=='/api/openflux/volga/config' and body.get('operation')=='upload' and runtime.config_path.is_file():
            handler.j({'ok':False,'error':'Volga is configured. Replace its file under Configuration.'},400);return True
        if handler.path.endswith('/protocol'):
            result=run_volga_cli(types.SimpleNamespace(action='select-protocol',protocol=body.get('protocol')))
        elif handler.path=='/api/openflux/mailru/config':
            # The link field, or the config file (download and upload) beside it.
            operation=body.get('operation','link')
            if operation=='download':
                document=MailruRuntime().document()
                if not document:raise ValueError('Set up Mail.ru first')
                result={'ok':True,'config':'MAILRU_DOCUMENT='+document+'\n','filename':'mailru.env'}
            elif operation in ('link','upload'):
                if operation=='upload' and (not isinstance(body.get('config'),str) or len(body['config'])>4096):raise ValueError('Expected one MAILRU_DOCUMENT=<public link> line')
                document=mailru_env(body['config']) if operation=='upload' else body.get('document')
                result=run_mailru_cli(types.SimpleNamespace(action='setup-mailru',document_url=document,image=None,network=None))
            else:raise ValueError('Invalid Mail.ru config operation')
        else:
            with open('/run/openflux-volga.lock','a') as lock:
                fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
                if handler.path.endswith('/config'):
                    # First setup only: a template to fill, then one upload.
                    if body.get('operation')=='download':result={'ok':True,'config':runtime.template()}
                    elif body.get('operation')=='upload':result=runtime.configure(body.get('config'))
                    else:raise ValueError('Invalid config operation')
                elif handler.path.endswith('/cookies'):result=runtime.import_cookies(body.get('document'),body.get('curl'))
                else:result=runtime.package_cookies(body.get('document'),body.get('curl'),upload=body.get('upload') is True)
        handler.j(result)
    except Exception as exc:
        # No browser text, credential, raw subprocess output or key path is
        # reflected to a remote browser on an exception: fixed texts only.
        if handler.path.endswith('/protocol'):error=protocol_error(exc)
        elif handler.path=='/api/openflux/mailru/config':error=mailru_setup_error(exc)
        elif isinstance(exc,BlockingIOError):error=BUSY_ERROR
        else:error='Volga operation failed. Check configuration, release availability and cookie pairing. Any runtime switch/update is restored on failure.'
        handler.j({'ok':False,'error':error},400)
    return True
