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
    if 'import openflux_auth' in text:return patch_recovery(text)
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
    return patch_recovery(text)
