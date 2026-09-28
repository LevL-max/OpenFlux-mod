import hashlib,json,pathlib,tempfile,types,unittest
from unittest.mock import patch
from cryptography.hazmat.primitives.asymmetric import rsa,ed25519
from cryptography.hazmat.primitives import serialization
from router_integration import (VolgaRuntime,VOLGA_PROFILE,VOLGA_UNIT,VOLGA_CONFIG,
    validate_volga_config,validate_volga_manifest,patch_runtime_selector,patch_volga_panel)
import recovery_crypto as crypto

DOC='https://disk.yandex.ru/i/fixtureA'
EDITOR='https://docs.yandex.ru/edit/d/fixture?from_public=1'

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

 def test_grouped_panel_keeps_groups_and_marks_volga_config_private(self):
  source='''import openflux_auth
  <div class="small" id="openfluxHealth" style="margin-top:8px"></div>
  $('#openfluxState').textContent=of.state;
  const CFG_GROUPS=[{id:'openflux',kinds:['openflux','url','env','openflux-node','openflux-updater'],modes:['openflux']}];
  const CFG_SECRET=new Set(['openflux-disk-token','openflux-client-key','openflux-cookies']);
  refreshNetwork();setInterval(refreshNetwork,5000);
  '''
  changed=patch_volga_panel(source)
  self.assertEqual(patch_volga_panel(changed),changed)
  self.assertIn("'openflux-updater','openflux-volga-config'],modes:['openflux']",changed)
  self.assertIn("'openflux-cookies','openflux-volga-config']);",changed)
  self.assertEqual(changed.count('id="openfluxProtocolControls"'),1)

 def test_optional_runtime_dispatcher_without_fixed_unit_is_preserved(self):
  self.runtime.save(self.runtime.node_path,dict(self.node,router_updater=True))
  for name in ('openflux-routerctl','openflux-clientctl','router-restore-runtime'):
   p=self.runtime.path('/usr/local/sbin/'+name);p.parent.mkdir(parents=True,exist_ok=True)
   p.write_text('#!/usr/bin/env bash\nset -Eeuo pipefail\nUNIT="openflux-yandex-client.service"\nsystemctl status "$UNIT"\n')
  health=self.runtime.path('/usr/local/lib/router-wan/openflux_health.py');health.parent.mkdir(parents=True)
  health.write_text("UNIT='openflux-yandex-client.service'\n")
  dispatcher=self.runtime.path('/usr/local/sbin/router-runtime-ensure')
  content=b'#!/usr/bin/env bash\nset -Eeuo pipefail\ncase "$MODE" in\n openflux) /usr/local/sbin/openflux-routerctl assert || /usr/local/sbin/router-restore-runtime;;\nesac\n'
  dispatcher.write_bytes(content);record={}
  with patch.object(self.runtime,'run'):self.runtime.install_router_hooks(record)
  self.assertEqual(dispatcher.read_bytes(),content)
  self.assertEqual(len(record['router_hooks']),4)
  for item in record['router_hooks']:
   self.assertIn('openflux-protocol-selector-v1',pathlib.Path(item['path']).read_text())

 def test_release_manifest_requires_frozen_profile_and_pinned_image(self):
  row={'id':'volga','binary':{'name':'openflux-volga-linux-amd64','sha256':'a'*64},'image':'ghcr.io/levl-max/openflux-mod-volga@sha256:'+'b'*64,'image_id':'sha256:'+'c'*64,
       'performance_profile':VOLGA_PROFILE,'config_protocol':'volga-stream-v1','max_streams':64}
  value={'schema':1,'version':'v4.1.0-rc1','default':'yandex','protocols':[row]}
  validate_volga_manifest(value,'v4.1.0-rc1','a'*64)
  for bad in [dict(value,protocols=[row,row]),dict(value,protocols=[dict(row,image='image:latest')]),dict(value,default='volga')]:
   with self.assertRaises(ValueError):validate_volga_manifest(bad,'v4.1.0-rc1','a'*64)

 def test_server_uses_verified_archive_without_registry_credentials(self):
  import openflux_node as node
  self.runtime.save(self.runtime.node_path,dict(self.node,role='server'))
  self.runtime.save(self.runtime.config_path,dict(self.cfg,role='server',egress_policy='public'))
  files={'openflux-volga-linux-amd64':b'binary','openflux-yandex-cookie-import':b'helper',
         'openflux-integration-linux.tar.gz':b'bundle','openflux-node.py':b'pass',
         'openflux-volga-container-linux-amd64.tar.gz':b'container'}
  digest=lambda b:hashlib.sha256(b).hexdigest()
  image_id='sha256:'+'c'*64
  manifest={'schema':1,'version':'v4.1.0-rc1','default':'yandex','protocols':[{'id':'volga',
   'binary':{'name':'openflux-volga-linux-amd64','sha256':digest(files['openflux-volga-linux-amd64'])},
   'container_archive':{'name':'openflux-volga-container-linux-amd64.tar.gz','sha256':digest(files['openflux-volga-container-linux-amd64.tar.gz'])},
   'image':'ghcr.io/levl-max/openflux-mod-volga@sha256:'+'b'*64,'image_id':image_id,
   'performance_profile':VOLGA_PROFILE,'config_protocol':'volga-stream-v1','max_streams':64}]}
  files['protocol-manifest.json']=json.dumps(manifest).encode()
  files['SHA256SUMS']=''.join(digest(v)+'  '+k+'\n' for k,v in files.items()).encode()
  base='https://github.com/'+node.REPO+'/releases/'
  row={'tag_name':'v4.1.0-rc1','html_url':base+'tag/v4.1.0-rc1','assets':[{'name':k,'digest':'sha256:'+digest(v),'browser_download_url':base+'download/v4.1.0-rc1/'+k} for k,v in files.items()]}
  def fetch(url,path,limit):path.write_bytes(files[url.rsplit('/',1)[-1]])
  with patch.object(self.runtime,'release_candidate',return_value=row),patch.object(node,'fetch',side_effect=fetch),patch.object(node,'run',return_value=types.SimpleNamespace(stdout='openflux-volga v4.1.0-rc1',stderr='')) as version,patch.object(self.runtime,'run',return_value=types.SimpleNamespace(returncode=0,stdout=image_id)) as docker:
   self.runtime.prepare()
   commands=[c.args[0] for c in docker.call_args_list]
   self.assertTrue(any(c[:2]==['docker','load'] for c in commands));self.assertFalse(any(c[:2]==['docker','pull'] for c in commands))
   self.assertEqual(self.runtime.read(self.runtime.state/'staged.json')['image'],image_id)
   docker.reset_mock();version.reset_mock();files['openflux-volga-container-linux-amd64.tar.gz']=b'tampered'
   with self.assertRaisesRegex(ValueError,'digest mismatch'):self.runtime.prepare()
   docker.assert_not_called();version.assert_not_called()

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

 def test_failed_update_restores_binary_unit_and_selection(self):
  import openflux_node as n
  self.runtime.binary.parent.mkdir(parents=True);self.runtime.binary.write_bytes(b'old')
  unit=self.runtime.path('/etc/systemd/system')/VOLGA_UNIT;unit.parent.mkdir(parents=True);unit.write_text('old unit')
  installed={'version':'v4.1.0-rc1','sha256':n.sha(self.runtime.binary)};self.runtime.save(self.runtime.state/'installed.json',installed)
  candidate=self.runtime.state/'candidate';candidate.mkdir();(candidate/'openflux-volga-linux-amd64').write_bytes(b'new')
  digest=n.sha(candidate/'openflux-volga-linux-amd64')
  manifest={'schema':1,'version':'v4.1.0-rc2','default':'yandex','protocols':[{'id':'volga','binary':{'name':'openflux-volga-linux-amd64','sha256':digest},
   'performance_profile':VOLGA_PROFILE,'config_protocol':'volga-stream-v1','max_streams':64,'image':'ghcr.io/levl-max/openflux-mod-volga@sha256:'+'b'*64,'image_id':'sha256:'+'c'*64}]}
  self.runtime.save(candidate/'protocol-manifest.json',manifest)
  library=self.make_support_fixture(candidate)
  old_support={p.name:p.read_bytes() for p in library.iterdir()}
  assets={p.name:n.sha(p) for p in candidate.iterdir()}
  self.runtime.save(self.runtime.state/'staged.json',{'directory':str(candidate),'assets':assets,'base_sha256':n.sha(self.runtime.binary),'version':'v4.1.0-rc2'})
  flags,commands,run=self.fake_services()
  with patch.object(self.runtime,'run',side_effect=run),patch.object(self.runtime,'health',side_effect=RuntimeError('test health failure')):
   with self.assertRaisesRegex(RuntimeError,'test health'):self.runtime.install_staged()
  self.assertEqual(self.runtime.binary.read_bytes(),b'old');self.assertEqual(unit.read_text(),'old unit')
  self.assertEqual(self.runtime.read(self.runtime.state/'installed.json'),installed)
  self.assertEqual(flags,{'legacy':True,'volga':False});self.assertEqual(self.runtime.node(),self.node)
  self.assertEqual({p.name:p.read_bytes() for p in library.iterdir()},old_support)
  watchdog=next(c for c in commands if c[0]=='systemd-run')
  self.assertIn('/recovery/openflux_node.py',watchdog[-2])

 def make_support_fixture(self,candidate):
  import io,tarfile,openflux_release
  source=pathlib.Path(__file__).resolve().parents[1]
  library=self.runtime.path(self.node['library']);library.mkdir(parents=True,exist_ok=True)
  with tarfile.open(candidate/'openflux-integration-linux.tar.gz','w:gz') as tar:
   for name in (*openflux_release.MODULES,'install.py','README.md'):
    data=(source/name).read_bytes();info=tarfile.TarInfo('integration/'+name);info.size=len(data);tar.addfile(info,io.BytesIO(data))
  (candidate/'openflux-node.py').write_bytes((source/'openflux_node.py').read_bytes())
  for name in (*openflux_release.MODULES,'openflux_node.py'):
   (library/name).write_bytes((source/name).read_bytes()+b'\n# previous installed support\n')
  return library

 def test_interrupted_support_replacement_restores_complete_old_modules(self):
  import openflux_node as node
  candidate=self.runtime.state/'candidate';candidate.mkdir()
  library=self.make_support_fixture(candidate)
  before={p.name:p.read_bytes() for p in library.iterdir()}
  plans=self.runtime.support_plan(candidate)
  flags,commands,run=self.fake_services()
  original=node.atomic_file;calls=[]
  def interrupted(source,target,mode=0o755):
   calls.append(str(target))
   if len(calls)==3:raise OSError('injected disk failure')
   original(source,target,mode)
  with patch.object(self.runtime,'run',side_effect=run):
   record=self.runtime.begin_transaction('update')
   with patch.object(node,'atomic_file',side_effect=interrupted),self.assertRaisesRegex(OSError,'disk failure'):
    self.runtime.install_support(plans,record)
   self.assertNotEqual({p.name:p.read_bytes() for p in library.iterdir()},before)
   self.runtime.restore_transaction(record)
  self.assertEqual({p.name:p.read_bytes() for p in library.iterdir()},before)
  self.assertFalse((self.runtime.state/'transaction.json').exists())

 def test_panel_update_does_not_kill_its_own_request_during_copy(self):
  self.runtime.save(self.runtime.node_path,dict(self.node,router_updater=True))
  candidate=self.runtime.state/'candidate';candidate.mkdir()
  self.make_support_fixture(candidate);plans=self.runtime.support_plan(candidate)
  flags,commands,run=self.fake_services()
  with patch.object(self.runtime,'run',side_effect=run):
   record=self.runtime.begin_transaction('update')
   self.runtime.install_support(plans,record)
   self.assertFalse(any('router-panel.service' in c for c in commands))
   self.runtime.restore_transaction(record)
  self.assertFalse((self.runtime.state/'transaction.json').exists())
  self.assertEqual(commands[-1][0],'systemd-run')
  self.assertIn('--on-active=2s',commands[-1])
  self.assertEqual(commands[-1][-2:],['try-restart','router-panel.service'])

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

 def test_volga_prerelease_channel_does_not_change_legacy_channel(self):
  import openflux_node as node
  self.runtime.save(self.runtime.node_path,dict(self.node,channel='stable',volga_channel='prerelease'))
  before=self.runtime.node_path.read_bytes()
  row={'tag_name':'v4.1.0-rc1','assets':[{'name':'openflux-volga-linux-amd64'}]}
  with patch.object(node,'releases',return_value=[row]) as releases:
   self.assertEqual(self.runtime.release_candidate(),row)
   self.assertEqual(releases.call_args.args[0]['channel'],'prerelease')
  self.assertEqual(self.runtime.node_path.read_bytes(),before)
  self.assertEqual(self.runtime.node()['channel'],'stable')
  self.runtime.save(self.runtime.node_path,dict(self.node,channel='stable',volga_channel='invalid'))
  with patch.object(node,'releases') as releases:
   with self.assertRaisesRegex(ValueError,'Invalid Volga release channel'):self.runtime.release_candidate()
   releases.assert_not_called()

if __name__=='__main__':unittest.main()
