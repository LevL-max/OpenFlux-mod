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
    if 'import openflux_auth' in text:return patch_config_panel(patch_recovery(text))
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
    return patch_config_panel(patch_recovery(text))


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
        if target=='openflux-node':
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
        custom={'openflux-url','openflux-node','openflux-updater','openflux-disk-token','openflux-server-key','openflux-client-key','openflux-client-public','openflux-cookies'}
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
            else:message+=' Used by the next recovery/update operation.'
            result={'ok':True,'changed':True,'target':target,'message':message,'backup':str(backup),'saved_at':int(time.time())}
            from xray_control import atomic
            atomic(self.core.ROOT/'last-upload.json',result)
            return result


def extend_config_store(core):
    if getattr(core,'_openflux_extended',False):return
    extension=OpenFluxConfigs(core)
    core.registry=extension.registry;core.upload=extension.upload;core._openflux_extended=True
