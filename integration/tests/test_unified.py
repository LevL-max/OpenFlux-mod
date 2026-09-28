import contextlib,hashlib,io,json,pathlib,shutil,tarfile,tempfile,types,unittest
from unittest.mock import patch
import openflux_release as r
import openflux_node as n
import router_integration
from router_integration import VOLGA_PROFILE

SOURCE=pathlib.Path(__file__).resolve().parents[1]
IMAGE='sha256:'+'c'*64
def digest(data):return hashlib.sha256(data).hexdigest()
def sha(path):return digest(pathlib.Path(path).read_bytes())

def bundle_bytes():
 out=io.BytesIO()
 with tarfile.open(fileobj=out,mode='w:gz') as tar:
  for name in (*r.MODULES,'install.py','README.md'):
   data=(SOURCE/name).read_bytes();info=tarfile.TarInfo('integration/'+name);info.size=len(data);tar.addfile(info,io.BytesIO(data))
 return out.getvalue()

def release(tag,volga=True,container=False,profile=VOLGA_PROFILE):
 files={'openflux-linux-amd64':b'legacy '+tag.encode(),'openflux-yandex-cookie-import':(SOURCE/'cookie_import.py').read_bytes(),
        'openflux-integration-linux.tar.gz':bundle_bytes(),'openflux-node.py':(SOURCE/'openflux_node.py').read_bytes()}
 if volga:
  files['openflux-volga-linux-amd64']=b'volga '+tag.encode()
  row={'id':'volga','binary':{'name':'openflux-volga-linux-amd64','sha256':digest(files['openflux-volga-linux-amd64'])},
       'image':'ghcr.io/levl-max/openflux-mod-volga@sha256:'+'b'*64,'image_id':IMAGE,
       'performance_profile':profile,'config_protocol':'volga-stream-v1','max_streams':64}
  if container:
   files[n.VOLGA_ARCHIVE]=b'image '+tag.encode();row['container_archive']={'name':n.VOLGA_ARCHIVE,'sha256':digest(files[n.VOLGA_ARCHIVE])}
  files['protocol-manifest.json']=json.dumps({'schema':1,'version':tag,'default':'yandex','protocols':[row]}).encode()
 files['SHA256SUMS']=''.join(digest(v)+'  '+k+'\n' for k,v in files.items()).encode()
 base='https://github.com/'+r.REPO+'/releases/'
 data={'id':1,'tag_name':tag,'html_url':base+'tag/'+tag,'published_at':'2026-09-29','prerelease':True,
       'assets':[{'name':k,'digest':'sha256:'+digest(v),'browser_download_url':base+'download/'+tag+'/'+k} for k,v in files.items()]}
 return data,files

def run_versions(calls):
 def run(argv,**kwargs):
  calls.append(argv)
  out={'--help':'--yandex-cookie-store','-version':'openflux-volga v4.1.0'}.get(argv[-1],IMAGE)
  return types.SimpleNamespace(returncode=0,stdout=out,stderr='')
 return run

class RouterUpdaterTests(unittest.TestCase):
 def setUp(self):
  self.temp=tempfile.TemporaryDirectory();self.addCleanup(self.temp.cleanup);root=self.root=pathlib.Path(self.temp.name)
  self.paths={'VOLGA_BINARY':root/'opt'/'openflux-volga','VOLGA_CONFIG':root/'etc-volga'/'config.json','VOLGA_STATE':root/'volga-state',
              'VOLGA_INSTALLED':root/'volga-state'/'installed.json','UNIT_DIR':root/'units','PANEL':root/'panel'/'router-panel.py',
              'NODE_PROFILE':root/'node.json','LIBDIR':root/'lib','RUNNER':root/'runner','HELPER':root/'helper','DROPIN':root/'dropin',
              'ROUTER_HOOKS':(str(root/'sbin'/'openflux-routerctl'),)}
  stack=contextlib.ExitStack();self.addCleanup(stack.close)
  for name,value in self.paths.items():stack.enter_context(patch.object(r,name,value))
  self.state=root/'state';self.state.mkdir();self.binary=root/'legacy';self.binary.write_text('legacy old');self.panel_ok=True

 def prepare_core(self,data,files,calls):
  return types.SimpleNamespace(check_binary=lambda name,route:r.metadata(data),installed=lambda name:{'version':'v4.1.0-rc2','sha256':sha(self.binary)},
   component_dir=lambda name:self.state,transport_for=lambda route:contextlib.nullcontext(None),sha=sha,run=run_versions(calls),
   download=lambda url,path,*args,**kwargs:pathlib.Path(path).write_bytes(files[url.rsplit('/',1)[-1]]),
   COMPONENTS={'openflux':{'binary':str(self.binary)}},write_json=lambda path,value:pathlib.Path(path).write_text(json.dumps(value)),progress=lambda message:None)

 def replace_core(self,active=()):
  mode=self.root/'mode';mode.write_text('awg');runs=[]
  def run(argv,**kwargs):
   runs.append(argv)
   return types.SimpleNamespace(returncode=(0 if self.panel_ok else 7) if argv[0]=='curl' else 0,stdout='',stderr='')
  core=types.SimpleNamespace(COMPONENTS={'openflux':{'binary':str(self.binary)}},MODE=mode,sha=sha,
   installed=lambda name:{'version':'v4.1.0-rc2','sha256':sha(self.binary)},service_active=lambda unit:unit in active,
   component_dir=lambda name:self.state,write_json=lambda path,value:pathlib.Path(path).write_text(json.dumps(value)),progress=lambda message:None,
   run=run,atomic_binary=lambda source,target:shutil.copyfile(source,target),exclusive=lambda path:contextlib.nullcontext(),
   guard_active_route=lambda *args:None,remove_update_guard=lambda guard:None,restore_runtime=lambda *args:None,connectivity_gate=lambda:None)
  candidate=self.state/'candidate-test';candidate.mkdir(exist_ok=True);new=candidate/'binary';new.write_text('legacy new')
  return core,runs,new,{'version':'v4.1.0-rc3','asset_sha256':sha(new),'bundle_schema':1,'volga':True}

 def test_metadata_requires_both_volga_assets(self):
  data,files=release('v4.1.0-rc3')
  self.assertEqual(r.metadata(data)['volga_sha256'],digest(files['openflux-volga-linux-amd64']))
  with self.assertRaisesRegex(ValueError,'Incomplete Volga'):r.metadata(dict(data,assets=[a for a in data['assets'] if a['name']!='protocol-manifest.json']))
  self.assertNotIn('volga_sha256',r.metadata(release('v4.0.7',volga=False)[0]))

 def test_is_current_requires_the_same_volga_release(self):
  legacy={'version':'v4.1.0-rc3','sha256':'a'};candidate={'version':'v4.1.0-rc3','asset_sha256':'a','volga_sha256':digest(b'volga new')}
  self.assertTrue(r.is_current(legacy,candidate))
  self.paths['VOLGA_BINARY'].parent.mkdir(parents=True);self.paths['VOLGA_BINARY'].write_bytes(b'volga old')
  self.assertFalse(r.is_current(legacy,candidate))
  self.paths['VOLGA_BINARY'].write_bytes(b'volga new');self.paths['VOLGA_STATE'].mkdir();self.paths['VOLGA_INSTALLED'].write_text(json.dumps({'version':'v4.1.0-rc3'}))
  self.assertTrue(r.is_current(legacy,candidate))

 def test_prepare_takes_volga_and_validates_it_with_the_candidates_code(self):
  self.paths['VOLGA_CONFIG'].parent.mkdir(parents=True);self.paths['VOLGA_CONFIG'].write_text('{}')
  data,files=release('v4.1.0-rc3');calls=[]
  r.prepare(self.prepare_core(data,files,calls),'current')
  staged=json.loads((self.state/'staged.json').read_text())
  self.assertTrue(staged['volga']);self.assertEqual(staged['volga_sha256'],digest(files['openflux-volga-linux-amd64']))
  self.assertEqual((self.state/staged['directory']/'volga').read_bytes(),files['openflux-volga-linux-amd64'])
  self.assertTrue(any(argv[-1]=='-version' for argv in calls))
  # A profile the candidate's own validator rejects is refused before installation.
  data,files=release('v4.1.0-rc4',profile=dict(VOLGA_PROFILE,posts_per_second=999))
  with self.assertRaisesRegex(ValueError,'Unsupported Volga profile'):r.prepare(self.prepare_core(data,files,[]),'current')

 def test_prepare_without_volga_setup_takes_legacy_only(self):
  data,files=release('v4.1.0-rc3');calls=[]
  r.prepare(self.prepare_core(data,files,calls),'current')
  staged=json.loads((self.state/'staged.json').read_text())
  self.assertFalse(staged['volga']);self.assertFalse((self.state/staged['directory']/'volga').exists())
  self.assertFalse(any(argv[-1]=='-version' for argv in calls))

 def test_failed_install_restores_legacy_volga_and_hooks_together(self):
  core,runs,new,meta=self.replace_core()
  files={self.paths['VOLGA_BINARY']:'volga old',self.paths['UNIT_DIR']/r.VOLGA_UNIT:'unit old',self.paths['VOLGA_INSTALLED']:'{"version":"v4.1.0-rc2"}',
         pathlib.Path(self.paths['ROUTER_HOOKS'][0]):'hook old',self.paths['LIBDIR']/'router_integration.py':'x=1',self.paths['PANEL']:'panel=1'}
  for path,text in files.items():path.parent.mkdir(parents=True,exist_ok=True);path.write_text(text)
  def install(*args):
   for path in files:path.write_text('changed')
  with patch.object(r,'install_support',side_effect=install),patch.object(r,'health',side_effect=RuntimeError('health failed')):
   with self.assertRaisesRegex(RuntimeError,'Previous installation restored'):r.replace(core,new,meta)
  self.assertEqual(self.binary.read_text(),'legacy old')
  for path,text in files.items():self.assertEqual(path.read_text(),text,str(path))
  self.assertFalse((self.state/'installed.json').exists())

 def test_selected_but_stopped_volga_is_not_started_to_be_tested(self):
  core,runs,new,meta=self.replace_core()
  self.paths['NODE_PROFILE'].write_text(json.dumps({'active_transport':'volga'}))
  with patch.object(r,'install_support'),patch.object(r,'health') as health:
   r.replace(core,new,meta)
  self.assertFalse(any(argv[:2]==['systemctl','start'] for argv in runs));health.assert_not_called()
  self.assertEqual(json.loads((self.state/'installed.json').read_text())['version'],'v4.1.0-rc3')

 def test_running_selected_volga_is_tested_and_legacy_is_left_alone(self):
  core,runs,new,meta=self.replace_core(active=(r.VOLGA_UNIT,))
  self.paths['NODE_PROFILE'].write_text(json.dumps({'active_transport':'volga'}))
  checked=[];fake=types.SimpleNamespace(VolgaRuntime=lambda:types.SimpleNamespace(health=lambda:checked.append(True)))
  with patch.object(r,'install_support'),patch.object(r,'health') as legacy_health,patch.object(r,'load_integration',return_value=fake):
   r.replace(core,new,meta)
  self.assertIn(['systemctl','start',r.VOLGA_UNIT],runs);self.assertEqual(checked,[True]);legacy_health.assert_not_called()
  self.assertIn(['systemctl','stop',r.UNIT,r.VOLGA_UNIT],runs)

 def test_panel_that_does_not_start_rolls_the_install_back(self):
  core,runs,new,meta=self.replace_core(active=('router-panel.service',));self.panel_ok=False
  self.paths['PANEL'].parent.mkdir(parents=True);self.paths['PANEL'].write_text('panel old')
  real=r.panel_health
  with patch.object(r,'install_support',side_effect=lambda *args:self.paths['PANEL'].write_text('panel new')),patch.object(r,'health'),patch.object(r,'panel_health',side_effect=lambda core:real(core,seconds=0.2)):
   with self.assertRaisesRegex(RuntimeError,'Router panel did not start'):r.replace(core,new,meta)
  self.assertEqual(self.paths['PANEL'].read_text(),'panel old')

 def test_install_volga_writes_binary_unit_hooks_and_state_without_starting_it(self):
  directory=self.root/'candidate';directory.mkdir();(directory/'volga').write_bytes(b'volga new');calls=[]
  def save(path,value):pathlib.Path(path).parent.mkdir(parents=True,exist_ok=True);pathlib.Path(path).write_text(json.dumps(value))
  runtime=types.SimpleNamespace(unit_text=lambda:'[Unit]\n',install_router_hooks=lambda:calls.append('hooks'),install_recovery_timer=lambda:calls.append('timer'),save=save)
  core=types.SimpleNamespace(sha=sha,atomic_binary=lambda source,target:shutil.copyfile(source,target))
  self.paths['UNIT_DIR'].mkdir()
  r.install_volga(core,directory/'binary',{'version':'v4.1.0-rc3','volga_sha256':digest(b'volga new')},types.SimpleNamespace(VolgaRuntime=lambda:runtime))
  self.assertEqual(self.paths['VOLGA_BINARY'].read_bytes(),b'volga new');self.assertEqual((self.paths['UNIT_DIR']/r.VOLGA_UNIT).read_text(),'[Unit]\n')
  self.assertEqual(calls,['hooks','timer']);self.assertEqual(json.loads(self.paths['VOLGA_INSTALLED'].read_text())['version'],'v4.1.0-rc3')
  with self.assertRaisesRegex(ValueError,'checksum'):r.install_volga(core,directory/'binary',{'version':'x','volga_sha256':'0'*64},types.SimpleNamespace(VolgaRuntime=lambda:runtime))

 def test_first_unified_install_retires_the_separate_volga_updater(self):
  state=self.paths['VOLGA_STATE']
  for name in ('backup-1','candidate-1'):(state/name).mkdir(parents=True)
  for name in ('rollback.json','staged.json','installed.json','recovery.json'):(state/name).write_text('{}')
  r.retire_volga_updater()
  self.assertEqual(sorted(p.name for p in state.iterdir()),['installed.json','recovery.json'])

class FakeServerVolga:
 def __init__(self,active=False,existed=True):self.state={'active':active,'existed':existed};self.calls=[]
 def active(self):return self.state['active']
 def owned_container(self,name):return self.state['existed']
 def replace_server_container(self,image,record,save):
  self.calls.append(('replace',image))
  if self.state['existed']:record['old_container']='openflux-volga-backup-1';save(record)
  record['created_container']=True;save(record)
 def restore_server_container(self,record):self.calls.append(('restore',record.get('old_container')))
 def action(self,op):self.calls.append(('action',op))
 def health(self,**kwargs):self.calls.append(('health',kwargs))
 def save(self,path,value):pathlib.Path(path).parent.mkdir(parents=True,exist_ok=True);pathlib.Path(path).write_text(json.dumps(value))
 def install_recovery_timer(self):self.calls.append(('timer',))
 def prune_server_containers(self,keep):self.calls.append(('prune',keep))

class ServerUpdaterTests(unittest.TestCase):
 def setUp(self):
  self.temp=tempfile.TemporaryDirectory();self.addCleanup(self.temp.cleanup);root=self.root=pathlib.Path(self.temp.name)
  self.stack=contextlib.ExitStack();self.addCleanup(self.stack.close)
  self.state=root/'state';self.state.mkdir();self.lib=root/'lib';self.lib.mkdir();self.binary=root/'legacy';self.binary.write_bytes(b'legacy v4.1.0-rc3')
  for obj,name,value in [(n,'STATE',self.state),(n,'HELPER',root/'helper'),(n,'VOLGA_BINARY',root/'opt'/'openflux-volga'),(n,'VOLGA_INSTALLED',root/'volga-state'/'installed.json'),
                         (n,'VOLGA_CONFIG',root/'volga.json'),(r,'VOLGA_CONFIG',root/'volga.json'),(r,'VOLGA_BINARY',root/'opt'/'openflux-volga')]:
   self.stack.enter_context(patch.object(obj,name,value))
  (root/'volga.json').write_text('{}');(self.lib/'openflux_node.py').write_text('old updater')
  self.c={'binary':str(self.binary),'library':str(self.lib),'backend':'systemd','service':'exit.service','role':'server','channel':'prerelease'}

 def test_stage_takes_both_protocols_and_loads_the_verified_image(self):
  data,files=release('v4.1.0-rc3',container=True);calls=[]
  self.stack.enter_context(patch.object(n,'support',return_value=r));self.stack.enter_context(patch.object(n,'releases',return_value=[data]))
  self.stack.enter_context(patch.object(n,'identify',return_value={'version':'v4.1.0-rc2','sha256':sha(self.binary)}))
  fetch=self.stack.enter_context(patch.object(n,'fetch',side_effect=lambda url,path,limit:pathlib.Path(path).write_bytes(files[url.rsplit('/',1)[-1]])))
  self.stack.enter_context(patch.object(n,'run',side_effect=run_versions(calls)))
  staged=n.stage(self.c)
  self.assertTrue(staged['volga']);self.assertEqual(staged['image'],IMAGE)
  self.assertTrue(any(argv[:2]==['docker','load'] for argv in calls));self.assertFalse(any(argv[:2]==['docker','pull'] for argv in calls))
  calls.clear();files[n.VOLGA_ARCHIVE]=b'tampered'
  with self.assertRaisesRegex(ValueError,'digest mismatch'):n.stage(self.c)
  self.assertFalse(any(argv[0]=='docker' for argv in calls))

 def staged_release(self,legacy):
  candidate=self.state/'candidate-x';(candidate/'integration').mkdir(parents=True)
  for name,content in [('openflux-linux-amd64',legacy),('openflux-node.py',b'x=1'),('openflux-yandex-cookie-import',b'helper'),('openflux-volga-linux-amd64',b'volga new')]:(candidate/name).write_bytes(content)
  (candidate/'integration'/'openflux_auth.py').write_text('x=2')
  n.save(self.state/'staged.json',{'version':'v4.1.0-rc3','directory':str(candidate),'assets':{p.name:n.sha(p) for p in candidate.iterdir() if p.is_file()},
                                   'base_sha256':n.sha(self.binary),'volga':True,'image':IMAGE})

 def server_patches(self,volga,health_error=None):
  actions=[]
  module=types.SimpleNamespace(MODULES=('openflux_auth.py',),extract_bundle=lambda *args:None,retire_volga_updater=lambda:actions.append('retire'),
                               bundle_integration=lambda directory:types.SimpleNamespace(VOLGA_CONTAINER='openflux-volga',VolgaRuntime=lambda:volga))
  for name,value in [('support',module),('active',True),('identify',{'version':'v4.1.0-rc2','sha256':n.sha(self.binary)})]:
   self.stack.enter_context(patch.object(n,name,return_value=value))
  self.stack.enter_context(patch.object(n,'runtime_action',side_effect=lambda c,op:actions.append(op)))
  self.stack.enter_context(patch.object(n,'run',return_value=types.SimpleNamespace(returncode=0,stdout='',stderr='')))
  self.stack.enter_context(patch.object(n,'health',side_effect=health_error))
  self.stack.enter_context(patch.object(router_integration,'VolgaRuntime',return_value=volga))
  return actions

 def test_unchanged_legacy_exit_keeps_running_and_a_stopped_volga_stays_stopped(self):
  volga=FakeServerVolga(active=False,existed=True);actions=self.server_patches(volga)
  self.staged_release(self.binary.read_bytes())
  result=n.install_staged(self.c)
  self.assertFalse(result['legacy_restarted']);self.assertNotIn('stop',actions);self.assertNotIn('start',actions)
  self.assertEqual(n.VOLGA_BINARY.read_bytes(),b'volga new');self.assertEqual(n.read(n.VOLGA_INSTALLED)['image'],IMAGE)
  self.assertNotIn(('action','start'),volga.calls);self.assertIn(('prune','openflux-volga-backup-1'),volga.calls);self.assertIn('retire',actions)
  self.assertFalse((self.state/'transaction.json').exists())

 def test_failed_volga_health_restores_container_legacy_and_modules(self):
  volga=FakeServerVolga(active=True,existed=True);volga.health=lambda **kwargs:(_ for _ in ()).throw(RuntimeError('carrier failed'))
  actions=self.server_patches(volga)
  n.VOLGA_BINARY.parent.mkdir(parents=True);n.VOLGA_BINARY.write_bytes(b'volga old');(self.lib/'openflux_auth.py').write_text('old status')
  self.staged_release(b'legacy v4.1.0-rc4')
  with self.assertRaisesRegex(RuntimeError,'carrier failed'):n.install_staged(self.c)
  self.assertEqual(self.binary.read_bytes(),b'legacy v4.1.0-rc3');self.assertEqual(n.VOLGA_BINARY.read_bytes(),b'volga old')
  self.assertEqual((self.lib/'openflux_auth.py').read_text(),'old status')
  self.assertIn(('restore','openflux-volga-backup-1'),volga.calls);self.assertEqual(volga.calls[-1],('action','start'))
  self.assertEqual(actions,['stop','start','stop','start']);self.assertFalse((self.state/'transaction.json').exists())

if __name__=='__main__':unittest.main()
