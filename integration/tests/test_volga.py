import hashlib,io,itertools,json,pathlib,re,tempfile,time,types,unittest
from unittest.mock import patch
from cryptography.hazmat.primitives.asymmetric import rsa,ed25519
from cryptography.hazmat.primitives import serialization
import router_integration
from router_integration import (VolgaRuntime,VOLGA_PROFILE,VOLGA_UNIT,VOLGA_CONFIG,
    validate_volga_config,validate_volga_manifest,patch_runtime_selector,patch_volga_panel,run_volga_cli)
import recovery_crypto as crypto

DOC='https://disk.yandex.ru/i/fixtureA'
EDITOR='https://docs.yandex.ru/edit/d/fixture?from_public=1'
# The anchors a Router Panel offers before any OpenFlux patch.
BASE_PANEL='''import panel_updates as updater_ui
import panel_extra
class Handler:
    def do_POST(self):
        return
def status():
    return {"openflux":{"client_active":service_active("openflux-yandex-client.service")}}
INDEX=r"""
<div class="small" id="openfluxHealth" style="margin-top:8px"></div>
<div class="small" style="overflow-wrap:anywhere">${esc(c.path)}</div><input accept=".json,.yaml,.yml,.conf,.env,.txt">
<script>
const CFG_GROUPS=[{id:'openflux',kinds:['openflux','url','env','openflux-node','openflux-updater'],modes:['openflux']}];
const CFG_SECRET=new Set(['openflux-disk-token','openflux-client-key','openflux-cookies']);
function render(s){
  const of=s.openflux||{};
  const ofReady=!!(of.client_active&&of.bridge_active&&s.interfaces?.oflux0);
  $('#openfluxState').textContent=ofReady?'Ready':(of.client_active||of.bridge_active||s.interfaces?.oflux0?'Partial':'Stopped');
  dot('#openfluxDot',s.mode==='openflux'?(s.health?.level||'checking'):(ofReady?'warn':'bad'));
  $('#openfluxHealth').textContent=s.mode==='openflux'?(s.health?.summary||''):'';
}
refreshNetwork();setInterval(refreshNetwork,5000);
</script>
"""
'''

class VolgaTests(unittest.TestCase):
 def setUp(self):
  self.temp=tempfile.TemporaryDirectory();self.addCleanup(self.temp.cleanup)
  self.root=pathlib.Path(self.temp.name);self.runtime=VolgaRuntime(self.root)
  self.node={'role':'client','service':'openflux-yandex-client.service','library':'/usr/local/lib/router-updater','cookie_user':'openflux','active_transport':'yandex','router_updater':False}
  self.runtime.save(self.runtime.node_path,self.node)
  self.cfg=self.runtime.template();self.cfg.update(documents=[DOC,'https://disk.yandex.ru/i/fixtureB']*2,shared_key='12'*32)
  self.runtime.save(self.runtime.config_path,self.cfg)
  self.runtime.data.mkdir(parents=True)
  self.runtime.state.mkdir(parents=True)
  import openflux_auth
  for name in ('DISK_TOKEN_REJECTED','STATUS_VIEWED'):
   p=patch.object(openflux_auth,name,self.root/name.lower());p.start();self.addCleanup(p.stop)

 def test_frozen_profile_and_managed_paths(self):
  self.assertEqual(json.loads((pathlib.Path(__file__).resolve().parents[2]/'docs/FROZEN-PERFORMANCE-PROFILE.json').read_text()),VOLGA_PROFILE)
  self.assertEqual(validate_volga_config(self.cfg,'client'),self.cfg)
  for values in [{'posts_per_second':481},{'record_window_bytes':524288},{'max_streams':65},{'max_streams':True},
                 {'cookie_store':'/etc/other'},{'listen':'0.0.0.0:11080'},{'shared_key':'0'*64},
                 {'allowed_targets':['127.0.0.1:22']},{'documents':['https://evil.example/x']*4},
                 {'protocol':'yandex'},{'unexpected':1}]:
   with self.subTest(values=values),self.assertRaises((ValueError,TypeError)):validate_volga_config(dict(self.cfg,**values),'client')
  server=dict(self.cfg,role='server',egress_policy='public');validate_volga_config(server,'server')
  with self.assertRaises(ValueError):validate_volga_config(dict(server,egress_policy='allowlist'),'server')

 def test_browser_import_keeps_separate_store_and_filters_headers(self):
  legacy=self.root/'legacy.json';legacy.write_text('{"legacy":"preserve"}')
  text='curl --url "'+EDITOR+'" -b "Session_id=fixture; yandexuid=123" -H "User-Agent: Browser" -H "Authorization: never-forward"'
  result=self.runtime.import_cookies(DOC,text)
  self.assertEqual(result['count'],2);self.assertNotIn('fixture',result['message'])
  saved=self.runtime.read(self.runtime.data/'browser.json')
  self.assertEqual(saved,{'headers':{'User-Agent':'Browser'},'editors':{DOC:EDITOR}})
  self.assertEqual(self.runtime.read(self.runtime.data/'cookies.json')[DOC]['Session_id'],'fixture')
  self.assertEqual(legacy.read_text(),'{"legacy":"preserve"}')
  for bad in ['curl https://evil.example/ -b "s=x"','curl '+EDITOR+' -b "s=x" -H "User-Agent: x\ny"']:
   with self.assertRaises(ValueError):self.runtime.parse_browser(DOC,bad)
  with self.assertRaises(ValueError):self.runtime.parse_browser('https://disk.yandex.ru/i/unconfigured',text)

 def test_crypto_binds_cookie_and_status_to_protocol(self):
  server=rsa.generate_private_key(public_exponent=65537,key_size=3072);sender=ed25519.Ed25519PrivateKey.generate()
  packet=crypto.seal('test payload',server.public_key(),sender,now=1000,protocol='volga')
  self.assertEqual(crypto.unseal(packet,server,sender.public_key(),[],now=1001,protocol='volga')[0],'test payload')
  with self.assertRaises(ValueError):crypto.unseal(packet,server,sender.public_key(),[],now=1001)
  old=crypto.seal('legacy',server.public_key(),sender,now=1000)
  with self.assertRaises(ValueError):crypto.unseal(old,server,sender.public_key(),[],now=1001,protocol='volga')
  with self.assertRaises(ValueError):crypto.unseal(packet,server,sender.public_key(),[packet['id']],now=1001,protocol='volga')
  report=crypto.sign_status({'state':'connected','cookie':'secret'},server,now=1000,protocol='volga')
  self.assertNotIn('secret',json.dumps(report));self.assertEqual(crypto.verify_status(report,server.public_key(),now=1001,protocol='volga')['state'],'connected')
  with self.assertRaises(ValueError):crypto.verify_status(report,server.public_key(),now=1001)

 def test_two_document_recovery_does_not_overwrite_or_block_other_document(self):
  import recovery_inbox,urllib.parse
  server=rsa.generate_private_key(public_exponent=65537,key_size=3072);sender=ed25519.Ed25519PrivateKey.generate()
  keys=self.runtime.path('/etc/openflux-recovery');keys.mkdir(parents=True)
  for name,key,private in [('server-public.pem',server.public_key(),False),('sender-private.pem',sender,True),('server-private.pem',server,True),('sender-public.pem',sender.public_key(),False)]:
   data=key.private_bytes(serialization.Encoding.PEM,serialization.PrivateFormat.PKCS8,serialization.NoEncryption()) if private else key.public_bytes(serialization.Encoding.PEM,serialization.PublicFormat.SubjectPublicKeyInfo)
   (keys/name).write_bytes(data)
  (keys/'disk-token').write_text('fixture-token')
  documents=list(dict.fromkeys(self.cfg['documents']));packets={}
  for doc in documents:
   result=self.runtime.package_cookies(doc,'curl "'+EDITOR+'" -b "Session_id=fixture"')
   packets[self.runtime.disk_path(doc)]=result['package']
  self.assertEqual(len(packets),2);self.assertNotIn(self.runtime.disk_path(),packets)
  self.runtime.save(self.runtime.node_path,dict(self.node,role='server'))
  self.runtime.save(self.runtime.config_path,dict(self.cfg,role='server',egress_policy='public'))
  def fetch(url,**kwargs):
   if url.startswith('https://cloud-api.'):
    path=urllib.parse.parse_qs(urllib.parse.urlsplit(url).query)['path'][0]
    if path==self.runtime.disk_path(documents[0]):raise OSError('missing first document packet')
    return {'href':path}
   return packets[url]
  with patch.object(recovery_inbox,'fetch',side_effect=fetch),patch.object(recovery_inbox,'upload'),patch.object(self.runtime,'status',return_value={'state':'auth_blocked'}),patch.object(self.runtime,'import_cookies') as save:
   self.runtime.recovery_poll();self.runtime.recovery_poll()
  self.assertEqual(save.call_count,1);self.assertEqual(save.call_args.args[0],documents[1])
  state=self.runtime.read(self.runtime.state/'recovery.json')
  self.assertEqual(len(state['seen']),1);self.assertTrue(state['inboxes'][self.runtime.disk_path(documents[0])]['next_check'])

 def test_recovery_polls_disk_sparingly(self):
  import recovery_inbox,urllib.parse
  server=rsa.generate_private_key(public_exponent=65537,key_size=3072);sender=ed25519.Ed25519PrivateKey.generate()
  keys=self.runtime.path('/etc/openflux-recovery');keys.mkdir(parents=True)
  for name,key,private in [('server-public.pem',server.public_key(),False),('sender-private.pem',sender,True),('server-private.pem',server,True),('sender-public.pem',sender.public_key(),False)]:
   data=key.private_bytes(serialization.Encoding.PEM,serialization.PrivateFormat.PKCS8,serialization.NoEncryption()) if private else key.public_bytes(serialization.Encoding.PEM,serialization.PublicFormat.SubjectPublicKeyInfo)
   (keys/name).write_bytes(data)
  (keys/'disk-token').write_text('fixture-token')
  documents=list(dict.fromkeys(self.cfg['documents']));packets={}
  for doc in documents:
   packets[self.runtime.disk_path(doc)]=self.runtime.package_cookies(doc,'curl "'+EDITOR+'" -b "Session_id=fixture"')['package']
  self.runtime.save(self.runtime.node_path,dict(self.node,role='server'))
  self.runtime.save(self.runtime.config_path,dict(self.cfg,role='server',egress_policy='public'))
  calls=[]
  def fetch(url,**kwargs):
   if url.startswith('https://cloud-api.'):
    split=urllib.parse.urlsplit(url);path=urllib.parse.parse_qs(split.query)['path'][0]
    calls.append(('meta' if split.path.endswith('/resources') else 'link',path))
    return {'md5':'md5-'+path,'href':path}
   calls.append(('file',url));return packets[url]
  start=int(time.time());clock=[start]  # the packets expire an hour after sealing
  with patch.object(recovery_inbox,'fetch',side_effect=fetch),patch.object(recovery_inbox,'upload') as upload,patch('time.time',side_effect=lambda:clock[0]),patch.object(self.runtime,'status',return_value={'state':'connecting','active':True}),patch.object(self.runtime,'import_cookies') as applied:
   self.runtime.recovery_poll()
   self.assertEqual(applied.call_count,2);self.assertEqual(upload.call_count,1)
   self.assertEqual(sorted(k for k,_ in calls),['file','file','link','link','meta','meta'])
   # The timer fires every minute; a working server leaves Disk alone for 15 min.
   calls.clear()
   for minute in range(1,15):
    clock[0]=start+60*minute;self.runtime.recovery_poll()
   self.assertEqual(calls,[]);self.assertEqual(upload.call_count,1)
   # Fifteen minutes on: one metadata request per inbox, no download of an unchanged file.
   clock[0]=start+900;self.runtime.recovery_poll()
   self.assertEqual(sorted(k for k,_ in calls),['meta','meta']);self.assertEqual(applied.call_count,2)
   self.assertEqual(upload.call_count,1)
   # The status goes out again on the hourly heartbeat, or at once on a change.
   clock[0]=start+3600;self.runtime.recovery_poll();self.assertEqual(upload.call_count,2)
   with patch.object(self.runtime,'status',return_value={'state':'auth_blocked','active':True,'needs_cookies':True}):
    clock[0]=start+3620;self.runtime.recovery_poll();self.assertEqual(upload.call_count,3)
    # A server waiting for cookies checks its inboxes every 2 min.
    calls.clear();clock[0]=start+3719;self.runtime.recovery_poll();self.assertEqual(calls,[])
    clock[0]=start+3720;self.runtime.recovery_poll()
    self.assertEqual(sorted(k for k,_ in calls),['meta','meta'])

 def server_with_keys(self):
  keys=self.runtime.path('/etc/openflux-recovery');keys.mkdir(parents=True)
  server=rsa.generate_private_key(public_exponent=65537,key_size=2048);sender=ed25519.Ed25519PrivateKey.generate()
  for name,key,private in [('server-public.pem',server.public_key(),False),('sender-private.pem',sender,True),('server-private.pem',server,True),('sender-public.pem',sender.public_key(),False)]:
   data=key.private_bytes(serialization.Encoding.PEM,serialization.PrivateFormat.PKCS8,serialization.NoEncryption()) if private else key.public_bytes(serialization.Encoding.PEM,serialization.PublicFormat.SubjectPublicKeyInfo)
   (keys/name).write_bytes(data)
  (keys/'disk-token').write_text('fixture-token')
  return keys

 def test_refused_token_stops_disk_calls_until_it_changes(self):
  import openflux_auth,recovery_inbox,urllib.error
  keys=self.server_with_keys()
  self.runtime.save(self.runtime.node_path,dict(self.node,role='server'))
  self.runtime.save(self.runtime.config_path,dict(self.cfg,role='server',egress_policy='public'))
  calls=[]
  def refuse(url,**kwargs):
   calls.append(url);raise urllib.error.HTTPError(url,401,'Unauthorized',{},None)
  with patch.object(recovery_inbox,'fetch',side_effect=refuse),patch.object(self.runtime,'status',return_value={'state':'connecting'}):
   self.runtime.recovery_poll()
   self.assertEqual(len(calls),1);self.assertTrue(openflux_auth.disk_token_rejected('fixture-token'))
   for _ in range(3):self.runtime.recovery_poll()
   self.assertEqual(len(calls),1)  # nothing more is sent with the refused token
   (keys/'disk-token').write_text('fresh-token');self.runtime.recovery_poll()
   self.assertEqual(len(calls),2)

 def test_inbox_unchanged_after_a_disk_failure_is_downloaded_again(self):
  import recovery_inbox,urllib.parse
  self.server_with_keys()
  documents=list(dict.fromkeys(self.cfg['documents']));packets={}
  for doc in documents:
   packets[self.runtime.disk_path(doc)]=self.runtime.package_cookies(doc,'curl "'+EDITOR+'" -b "Session_id=fixture"')['package']
  self.runtime.save(self.runtime.node_path,dict(self.node,role='server'))
  self.runtime.save(self.runtime.config_path,dict(self.cfg,role='server',egress_policy='public'))
  down=[False];files=[]
  def fetch(url,**kwargs):
   if down[0]:raise OSError('Disk unavailable')
   if url.startswith('https://cloud-api.'):
    path=urllib.parse.parse_qs(urllib.parse.urlsplit(url).query)['path'][0]
    return {'md5':'md5-'+path,'href':path}
   files.append(url);return packets[url]
  start=int(time.time());clock=[start]
  with patch.object(recovery_inbox,'fetch',side_effect=fetch),patch.object(recovery_inbox,'upload'),patch('time.time',side_effect=lambda:clock[0]),patch.object(self.runtime,'status',return_value={'state':'connecting'}),patch.object(self.runtime,'import_cookies'):
   self.runtime.recovery_poll();self.assertEqual(len(files),2)
   down[0]=True;clock[0]=start+900;self.runtime.recovery_poll()
   entry=self.runtime.read(self.runtime.state/'recovery.json')['inboxes'][self.runtime.disk_path(documents[0])]
   self.assertEqual(entry['inbox'],'Yandex Disk temporarily unavailable')
   # Disk is back and the files are unchanged: they are downloaded again, so
   # the Disk failure does not outlive Disk.
   down[0]=False;clock[0]=start+1800;self.runtime.recovery_poll()
   self.assertEqual(len(files),4)
   for doc in documents:
    entry=self.runtime.read(self.runtime.state/'recovery.json')['inboxes'][self.runtime.disk_path(doc)]
    self.assertEqual((entry['inbox'],entry['failures']),('Applied or already seen',0))

 def test_client_fetches_server_status_only_while_someone_looks(self):
  import openflux_auth,recovery_inbox
  keys=self.runtime.path('/etc/openflux-recovery');keys.mkdir(parents=True);(keys/'disk-token').write_text('fixture-token')
  public=rsa.generate_private_key(public_exponent=65537,key_size=2048).public_key()
  (keys/'server-public.pem').write_bytes(public.public_bytes(serialization.Encoding.PEM,serialization.PublicFormat.SubjectPublicKeyInfo))
  fetched=[];clock=[1_900_000_000]
  def fetch(url,**kwargs):fetched.append(url);raise OSError('offline')
  with patch.object(recovery_inbox,'fetch',side_effect=fetch),patch('time.time',side_effect=lambda:clock[0]):
   self.runtime.recovery_poll();n=len(fetched);self.assertGreater(n,0)
   # Nobody looks: the next fetch waits 6 hours.
   clock[0]+=recovery_inbox.CLIENT_POLL_IDLE-1;self.runtime.recovery_poll();self.assertEqual(len(fetched),n)
   clock[0]+=1;self.runtime.recovery_poll();self.assertGreater(len(fetched),n);n=len(fetched)
   # While the panel is open, every minute.
   with patch.object(openflux_auth,'status_viewed',return_value=True):
    clock[0]+=recovery_inbox.CLIENT_POLL_VIEWED-1;self.runtime.recovery_poll();self.assertEqual(len(fetched),n)
    clock[0]+=1;self.runtime.recovery_poll();self.assertGreater(len(fetched),n)

 def test_resource_constraints_and_egress_container_isolation(self):
  text=self.runtime.unit_text()
  for field in ['MemoryMax=256M','CPUQuota=50%','AF_NETLINK','Conflicts=openflux-yandex-client.service','GOMEMLIMIT=160MiB']:self.assertIn(field,text)
  command=self.runtime.docker_command('ghcr.io/levl-max/openflux-mod-volga@sha256:'+'a'*64)
  for flag in ['--cap-drop=ALL','--read-only','--memory=256m','--memory-swap=256m','--cpus=0.5','--pids-limit=128','--network=bridge']:self.assertIn(flag,command)
  self.assertFalse(any('NET_ADMIN' in x or 'yandex-cookies' in x or '--publish' in x or '--network=host' in x for x in command))
  with patch.object(self.runtime,'node',return_value=dict(self.node,volga_resources={'memory_mib':16,'cpu_percent':500})):
   with self.assertRaises(ValueError):self.runtime.unit_text()

 def test_router_helpers_keep_legacy_fallback_and_are_idempotent(self):
  for declaration in ['UNIT="openflux-yandex-client.service"','UNIT_CLIENT="openflux-yandex-client.service"',
                      'CLIENT_UNIT="openflux-yandex-client.service"','systemctl is-active --quiet openflux-yandex-client.service']:
   source='#!/usr/bin/env bash\nset -Eeuo pipefail\n'+declaration+'\nip route show table 130\n'
   changed=patch_runtime_selector(source)
   self.assertEqual(patch_runtime_selector(changed),changed);self.assertIn('runtime-unit',changed);self.assertIn('ip route show table 130',changed)
  source="import subprocess\nUNIT='openflux-yandex-client.service'\n"
  changed=patch_runtime_selector(source,python=True);compile(changed,'health','exec')
  self.assertEqual(patch_runtime_selector(changed,python=True),changed)
  with self.assertRaises(ValueError):patch_runtime_selector('#!/bin/sh\necho unknown')

 def test_fresh_panel_gets_one_cookie_section_per_protocol(self):
  changed=router_integration.patch_panel(BASE_PANEL);compile(changed,'router-panel','exec')
  self.assertEqual(router_integration.patch_panel(changed),changed)
  self.assertIn("'openflux-updater','openflux-volga-config'],modes:['openflux']",changed)
  self.assertIn("'openflux-cookies','openflux-volga-config']);",changed)
  self.assertIn('data-volga-panel="5"',changed);self.assertNotIn('data-volga-update',changed);self.assertNotIn('volga/update',changed)
  # One browser login per place: no button puts one session on both, and a refused Disk token is shown.
  self.assertNotIn('CookieBoth',changed);self.assertNotIn('cookiesBoth',changed)
  self.assertEqual(changed.count(router_integration.SEPARATE_LOGINS),2)
  self.assertIn('within 2 minutes while it waits for cookies, otherwise within 15 minutes',changed)
  for element in ('id="openfluxDiskToken"','id="volgaDiskToken"','legacyAuth.disk_token_rejected'):self.assertEqual(changed.count(element),1)
  # An idle Volga server is "connecting"; its heading says what that means.
  self.assertIn("connecting:'waiting for a client'",changed);self.assertIn("['auth_blocked','stopped']",changed)
  for summary in ('>Legacy browser cookies<','>Volga browser cookies<'):self.assertEqual(changed.count(summary),1)
  for retired in ('Update Yandex cookies','Server cookies via Yandex Disk',"hidden=selected==='volga'"):self.assertNotIn(retired,changed)
  ids=re.findall(r'\bid="([^"]+)"',changed);self.assertEqual(len(ids),len(set(ids)))
  # The one-mini-PC limitation stays visible next to the selector and is confirmed on Apply.
  self.assertEqual(changed.count('id="volgaOneDevice"'),1);self.assertIn("confirm('Volga works on one mini-PC at a time.",changed)
  # Both sections stay visible, so Legacy lines read Legacy's own status even while Volga is selected.
  self.assertNotRegex(changed,r'\bof\.(last_failure|recovery_sender_ready|recovery_upload_ready|server_authentication)\b')
  self.assertLess(changed.index('const legacyAuth=of.protocols?.yandex||of'),changed.index('legacyAuth.last_failure'))
  # Document links are built with DOM calls from https URLs only; the panel embeds the JS in a Python string.
  for unsafe in ('innerHTML','\\','`'):self.assertNotIn(unsafe,router_integration.VOLGA_JS[len(router_integration.VOLGA_JS_V2):])
  self.assertIn("u.startsWith('https://')",router_integration.VOLGA_JS)

 def test_rc2_and_rc3_panels_migrate_to_the_fresh_layout(self):
  r=router_integration;fresh=r.patch_panel(BASE_PANEL)
  def older(html,js,hook):
   text=fresh.replace(r.VOLGA_HTML,html+r.COOKIE_HTML+r.RECOVERY_HTML).replace(r.VOLGA_JS,js).replace(r.VOLGA_HOOK,hook)
   for old,new in r.LEGACY_STATUS_V3:text=text.replace(new,old)
   return text
  for name,html,js,hook in [('rc3',r.VOLGA_HTML_V2,r.VOLGA_JS_V2,r.VOLGA_HOOK_V2),('rc2',r.VOLGA_HTML_V1,r.VOLGA_JS_V1,r.VOLGA_HOOK_V1)]:
   with self.subTest(name):
    panel=older(html,js,hook);self.assertNotEqual(panel,fresh)
    self.assertEqual(r.patch_panel(panel),fresh)
    # An unexpected layout changes nothing: installation then rolls back.
    with self.assertRaises(ValueError):r.patch_panel(panel.replace(r.COOKIE_HTML,r.COOKIE_HTML.replace('Copy as cURL','Copy')))
  # v4.1.0/v4.1.1 (v3) and v4.1.2-v4.1.5 (v4) panels: they shared the v4 script.
  v3=fresh.replace(r.VOLGA_HTML,r.VOLGA_HTML_V3).replace(r.VOLGA_JS,r.VOLGA_JS_V4).replace(r.VOLGA_HOOK,r.VOLGA_HOOK_V3)
  v4=fresh.replace(r.VOLGA_HTML,r.VOLGA_HTML_V4).replace(r.VOLGA_JS,r.VOLGA_JS_V4).replace(r.VOLGA_HOOK,r.VOLGA_HOOK_V4)
  self.assertNotEqual(r.VOLGA_HOOK,r.VOLGA_HOOK_V3);self.assertIn('data-volga-panel="3"',v3);self.assertIn('data-volga-panel="4"',v4)
  for old in (r.VOLGA_HTML_V4,r.VOLGA_JS_V4,r.VOLGA_HOOK_V4):self.assertIn('CookieBoth',old)
  self.assertEqual(r.patch_panel(v3),fresh);self.assertEqual(r.patch_panel(v4),fresh)
  with self.assertRaises(ValueError):r.patch_panel(v3.replace(r.VOLGA_HOOK_V3,r.VOLGA_HOOK_V3.replace('waiting','idle')))
  with self.assertRaises(ValueError):r.patch_panel(v4.replace(r.VOLGA_JS_V4,r.VOLGA_JS_V4.replace('cookieLinks','links')))

 def test_panel_status_carries_the_legacy_document_link(self):
  import openflux_auth as a
  node=self.root/'node.json';env=self.root/'yandex.env'
  with patch.object(a,'NODE_CONFIG',node),patch.object(a,'CLIENT_ENV',env),patch.object(a,'CLIENT',True):
   self.assertIsNone(a.current_document_url())
   env.write_text("YANDEX_URL='https://disk.yandex.ru/i/legacy'\n");self.assertEqual(a.current_document_url(),'https://disk.yandex.ru/i/legacy')
   node.write_text(json.dumps({'document_url':'https://disk.yandex.ru/i/node'}));self.assertEqual(a.current_document_url(),'https://disk.yandex.ru/i/node')
   node.write_text(json.dumps({'document_url':'javascript:alert(1)'}));self.assertIsNone(a.current_document_url())
   node.write_text('not json');self.assertEqual(a.current_document_url(),'https://disk.yandex.ru/i/legacy')

 def test_optional_runtime_dispatcher_without_fixed_unit_is_preserved(self):
  self.runtime.save(self.runtime.node_path,dict(self.node,router_updater=True))
  for name in ('openflux-routerctl','openflux-clientctl','router-restore-runtime'):
   p=self.runtime.path('/usr/local/sbin/'+name);p.parent.mkdir(parents=True,exist_ok=True)
   p.write_text('#!/usr/bin/env bash\nset -Eeuo pipefail\nUNIT="openflux-yandex-client.service"\nsystemctl status "$UNIT"\n')
  health=self.runtime.path('/usr/local/lib/router-wan/openflux_health.py');health.parent.mkdir(parents=True)
  health.write_text("UNIT='openflux-yandex-client.service'\n")
  dispatcher=self.runtime.path('/usr/local/sbin/router-runtime-ensure')
  content=b'#!/usr/bin/env bash\nset -Eeuo pipefail\ncase "$MODE" in\n openflux) /usr/local/sbin/openflux-routerctl assert || /usr/local/sbin/router-restore-runtime;;\nesac\n'
  dispatcher.write_bytes(content)
  with patch.object(self.runtime,'run'):changed=self.runtime.install_router_hooks()
  self.assertEqual(dispatcher.read_bytes(),content)
  self.assertEqual(len(changed),4)
  for path in changed:
   self.assertIn('openflux-protocol-selector-v1',pathlib.Path(path).read_text())

 def test_release_manifest_requires_frozen_profile_and_pinned_image(self):
  row={'id':'volga','binary':{'name':'openflux-volga-linux-amd64','sha256':'a'*64},'image':'ghcr.io/levl-max/openflux-mod-volga@sha256:'+'b'*64,'image_id':'sha256:'+'c'*64,
       'performance_profile':VOLGA_PROFILE,'config_protocol':'volga-stream-v1','max_streams':64}
  value={'schema':1,'version':'v4.1.0-rc1','default':'yandex','protocols':[row]}
  validate_volga_manifest(value,'v4.1.0-rc1','a'*64)
  for bad in [dict(value,protocols=[row,row]),dict(value,protocols=[dict(row,image='image:latest')]),dict(value,default='volga')]:
   with self.assertRaises(ValueError):validate_volga_manifest(bad,'v4.1.0-rc1','a'*64)

 def test_volga_updates_and_config_duplicates_are_retired(self):
  for args in [types.SimpleNamespace(action=a) for a in ('check','download','update','rollback')]+[types.SimpleNamespace(action='configure',channel='prerelease',config_file=None)]:
   with self.subTest(action=args.action),self.assertRaisesRegex(ValueError,'OpenFlux'):run_volga_cli(args)
  class Handler:
   def __init__(self,path,body):
    data=json.dumps(body).encode();self.path=path;self.headers={'Content-Length':str(len(data)),'Content-Type':'application/json'};self.rfile=io.BytesIO(data);self.sent=None
   def allowed(self):return True
   def authorized(self):return True
   def j(self,value,status=200):self.sent=(status,value)
  handler=Handler('/api/openflux/volga/update',{'action':'update'})
  self.assertTrue(router_integration.volga_panel_post(handler));self.assertEqual(handler.sent[0],400);self.assertIn('Router Updater',handler.sent[1]['error'])
  handler=Handler('/api/openflux/volga/config',{'operation':'upload','config':self.cfg})
  with patch.object(router_integration,'VolgaRuntime',return_value=self.runtime):self.assertTrue(router_integration.volga_panel_post(handler))
  self.assertEqual(handler.sent[0],400);self.assertIn('Configuration',handler.sent[1]['error'])

 def test_health_requires_a_settled_session_and_a_second_exit_service(self):
  status={'active':True,'state':'connected','needs_cookies':False}
  with patch.object(self.runtime,'status',return_value=status),patch.object(self.runtime,'exit_ok',return_value=True) as probe,patch('time.monotonic',side_effect=itertools.count(0.0,1.0)),patch('time.sleep'):
   self.runtime.health(seconds=30)
  self.assertEqual(probe.call_count,2)
  with patch.object(self.runtime,'status',return_value=status),patch.object(self.runtime,'exit_ok',side_effect=itertools.cycle([True,False])),patch('time.monotonic',side_effect=itertools.count(0.0,1.0)),patch('time.sleep'):
   with self.assertRaisesRegex(RuntimeError,'did not pass'):self.runtime.health(seconds=30)
  answers=iter([types.SimpleNamespace(returncode=7,stdout=''),types.SimpleNamespace(returncode=0,stdout='203.0.113.35\n')])
  with patch.object(self.runtime,'run',side_effect=lambda *args,**kwargs:next(answers)):self.assertTrue(self.runtime.exit_ok())

 def test_release_manifest_generator_matches_runtime_validator(self):
  import importlib.util
  script=pathlib.Path(__file__).resolve().parents[2]/'release/protocol_manifest.py'
  spec=importlib.util.spec_from_file_location('protocol_manifest',script);module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module)
  for name in ['openflux-linux-amd64','openflux-volga-linux-amd64','openflux-volga-container-linux-amd64.tar.gz']:(self.root/name).write_bytes(name.encode())
  value=module.generate(self.root,'v4.1.0-rc1','a'*40,'ghcr.io/levl-max/openflux-mod-volga@sha256:'+'b'*64,'sha256:'+'c'*64)
  digest=hashlib.sha256((self.root/'openflux-volga-linux-amd64').read_bytes()).hexdigest()
  self.assertEqual(validate_volga_manifest(value,'v4.1.0-rc1',digest)['image_id'],'sha256:'+'c'*64)

 def fake_services(self):
  flags={'legacy':True,'volga':False};commands=[]
  def run(argv,check=True,timeout=30):
   commands.append(argv);rc=0
   if argv[:2]==['systemctl','is-active']:
    unit=argv[-1];rc=0 if flags['volga' if unit==VOLGA_UNIT else 'legacy'] and unit!='sing-box-openflux.service' else 3
   elif argv[:2] in (['systemctl','start'],['systemctl','stop']):
    for unit in argv[2:]:
     if unit not in (VOLGA_UNIT,self.node['service']):continue
     which='volga' if unit==VOLGA_UNIT else 'legacy';flags[which]=argv[1]=='start'
     if flags[which]:flags['legacy' if which=='volga' else 'volga']=False
   return types.SimpleNamespace(returncode=rc,stdout='',stderr='')
  return flags,commands,run

 def test_failed_switch_restores_selection_and_live_legacy(self):
  self.runtime.save(self.runtime.state/'installed.json',{'version':'v4.1.0-rc1'})
  flags,commands,run=self.fake_services()
  with patch.object(self.runtime,'run',side_effect=run),patch.object(self.runtime,'health',side_effect=RuntimeError('test health failure')):
   with self.assertRaisesRegex(RuntimeError,'test health'):self.runtime.select('volga')
  self.assertEqual(self.runtime.node(),self.node);self.assertEqual(flags,{'legacy':True,'volga':False})
  self.assertFalse((self.runtime.state/'transaction.json').exists())

 def test_server_container_restore_never_deletes_the_original(self):
  record={'old_container':'openflux-volga-backup-test','created_container':True}
  with patch.object(self.runtime,'owned_container',side_effect=lambda name:False),patch.object(self.runtime,'run') as run:
   self.runtime.restore_server_container(record)
  run.assert_not_called()
  with patch.object(self.runtime,'owned_container',return_value=True),patch.object(self.runtime,'run') as run:
   self.runtime.restore_server_container(record)
  self.assertEqual([c.args[0] for c in run.call_args_list],[['docker','rm','-f','openflux-volga'],['docker','rename','openflux-volga-backup-test','openflux-volga']])

 def test_server_update_stops_the_old_server_before_creating_the_new(self):
  # Two servers on one document pair end each other's sessions (seen on AWS with v4.1.0).
  self.runtime.save(self.runtime.node_path,dict(self.node,role='server'))
  self.runtime.save(self.runtime.config_path,dict(self.cfg,role='server',egress_policy='public'))
  calls=[];saved=[]
  def run(argv,**kwargs):
   calls.append(argv)
   return types.SimpleNamespace(returncode=0,stdout='[]' if argv[:2]==['ip','-j'] else '',stderr='')
  with patch.object(self.runtime,'owned_container',return_value=True),patch.object(self.runtime,'run',side_effect=run):
   self.runtime.replace_server_container('sha256:'+'a'*64,{},lambda record:saved.append(dict(record)))
  self.assertEqual([c[:2] for c in calls if c[0]=='docker'],[['docker','stop'],['docker','rename'],['docker','create']])
  self.assertTrue(saved[0]['old_container'].startswith('openflux-volga-backup-'))

 def test_server_rollback_preserves_original_when_rename_failed(self):
  node=dict(self.node,role='server');self.runtime.save(self.runtime.node_path,node)
  record={'kind':'update','node':node,'binary_present':False,'old_container':'openflux-volga-backup-test',
          'unit':None,'installed':{},'active':False,'legacy_active':False,'watchdog':'test'}
  with patch.object(self.runtime,'active',return_value=False),patch.object(self.runtime,'owned_container',return_value=False),patch.object(self.runtime,'run') as run:
   self.runtime.restore_transaction(record)
  self.assertFalse(any(c.args[0][:2]==['docker','rm'] for c in run.call_args_list))

 def test_server_refuses_foreign_container_before_any_action(self):
  self.runtime.save(self.runtime.node_path,dict(self.node,role='server'))
  foreign=types.SimpleNamespace(returncode=0,stdout=json.dumps([{'Config':{'Labels':{}}}]))
  with patch.object(self.runtime,'run',return_value=foreign) as run:
   with self.assertRaisesRegex(ValueError,'not owned'):self.runtime.action('stop')
  self.assertEqual([c.args[0] for c in run.call_args_list],[['docker','inspect','openflux-volga']])

 def test_initial_server_install_checks_carrier_without_requiring_client(self):
  self.runtime.save(self.runtime.node_path,dict(self.node,role='server'))
  with patch.object(self.runtime,'status',return_value={'carrier_ready':True,'active':True,'state':'connecting','needs_cookies':False}):
   self.runtime.health(require_session=False)
  with patch.object(self.runtime,'status',return_value={'carrier_ready':True,'active':True,'state':'auth_blocked','needs_cookies':True}):
   with self.assertRaisesRegex(RuntimeError,'cookies'):self.runtime.health(require_session=False)

 def test_server_prune_keeps_the_rollback_container_and_images_in_use(self):
  repo=router_integration.VOLGA_IMAGE_REPOSITORY;removed=set();calls=[]
  images={'openflux-volga':'sha256:new','openflux-volga-backup-2':'sha256:prev','openflux-volga-backup-1':'sha256:old'}
  def run(argv,**kwargs):
   calls.append(argv);out=''
   if argv[:3]==['docker','ps','-a']:out='\n'.join(n for n in images if n not in removed)
   elif argv[:2]==['docker','rm']:removed.add(argv[-1])
   elif argv[:2]==['docker','inspect']:out=images[argv[-1]]
   elif argv[:2]==['docker','images']:out='\n'.join(['sha256:new '+repo,'sha256:prev '+repo,'sha256:old '+repo,'sha256:base openflux-runtime'])
   return types.SimpleNamespace(returncode=0,stdout=out,stderr='')
  with patch.object(self.runtime,'run',side_effect=run):self.runtime.prune_server_containers('openflux-volga-backup-2')
  self.assertEqual(removed,{'openflux-volga-backup-1'})
  self.assertEqual([a[-1] for a in calls if a[:3]==['docker','image','rm']],['sha256:old'])

 def test_stopped_client_still_reports_the_server_state(self):
  # Clients in AWG mode never run Volga; the panel still shows the server.
  self.runtime.save(self.runtime.state/'peer-status.json',{'state':'connecting','reported_at':int(time.time())-30,'fetch_failed':False})
  with patch.object(self.runtime,'active',return_value=False):status=self.runtime.status()
  self.assertEqual(status['state'],'stopped')
  self.assertEqual(status['server_authentication']['state'],'connecting');self.assertFalse(status['server_authentication']['stale'])
  # A server publishes at least every hour: 70 min old still counts, 76 min does not.
  self.runtime.save(self.runtime.state/'peer-status.json',{'state':'connecting','reported_at':int(time.time())-4200,'fetch_failed':False})
  with patch.object(self.runtime,'active',return_value=False):self.assertFalse(self.runtime.status()['server_authentication']['stale'])
  self.runtime.save(self.runtime.state/'peer-status.json',{'state':'connecting','reported_at':int(time.time())-4560,'fetch_failed':False})
  with patch.object(self.runtime,'active',return_value=False):self.assertTrue(self.runtime.status()['server_authentication']['stale'])

 def test_config_backups_keep_only_the_newest(self):
  self.runtime.save(self.runtime.node_path,dict(self.node,role='server'))
  server=dict(self.cfg,role='server',egress_policy='public')
  for key in ('34','56','78','9a','bc'):self.runtime.configure(dict(server,shared_key=key*32))
  backups=sorted(self.runtime.state.glob('config-backup-*.json'))
  self.assertEqual(len(backups),router_integration.CONFIG_BACKUPS)
  self.assertEqual(self.runtime.read(backups[-1])['shared_key'],'9a'*32)

if __name__=='__main__':unittest.main()
