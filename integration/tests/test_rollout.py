import contextlib,hashlib,importlib.util,io,json,pathlib,shutil,subprocess,tarfile,tempfile,types,unittest
from unittest.mock import patch
import openflux_release as r
import openflux_auth as a

def digest(p):return hashlib.sha256(pathlib.Path(p).read_bytes()).hexdigest()

class ReleaseTests(unittest.TestCase):
 def test_new_bundle_version_with_identical_binary_is_an_update(self):
  self.assertFalse(r.is_current({'version':'v4.0.0','sha256':'a'},{'version':'v4.0.1','asset_sha256':'a'}))
  self.assertTrue(r.is_current({'version':'v4.0.1','sha256':'a'},{'version':'v4.0.1','asset_sha256':'a'}))
 def test_bundle_rejects_links_traversal_duplicates_and_incomplete(self):
  with tempfile.TemporaryDirectory() as tmp:
   root=pathlib.Path(tmp)
   def archive(names,link=False):
    with tarfile.open(root/'bundle.tar.gz','w:gz') as tar:
     for name in names:
      info=tarfile.TarInfo(name);info.size=1
      if link:info.type=tarfile.SYMTYPE;info.linkname='/etc/passwd';info.size=0
      tar.addfile(info,io.BytesIO(b'x'))
   names=['integration/'+x for x in r.MODULES]
   archive(names);r.extract_bundle(root/'bundle.tar.gz',root/'valid')
   for i,bad in enumerate([names+['../escape'],names+[names[0]],names[:-1],['integration/../../escape']]):
    archive(bad)
    with self.assertRaises(ValueError):r.extract_bundle(root/'bundle.tar.gz',root/str(i))
   archive(names,link=True)
   with self.assertRaises(ValueError):r.extract_bundle(root/'bundle.tar.gz',root/'link')
 def test_semver_order_and_rejection(self):
  tags=['v4.0.0-rc2','v4.0.0-rc3','v4.0.0-rc10','v4.0.0','v4.0.1']
  self.assertEqual(sorted(tags,key=r.version),tags)
  for bad in ['main','v4.0','v04.0.0','v4.0.0-rc0','v4.0.0;id','../v4.0.0']:
   with self.assertRaises(ValueError):r.version(bad)
 def test_metadata_requires_exact_published_repo_assets(self):
  tag='v4.0.0-rc3';base='https://github.com/'+r.REPO+'/releases/'
  data={'id':123,'tag_name':tag,'published_at':'2026-09-25','html_url':base+'tag/'+tag,'assets':[{'name':n,'digest':'sha256:'+'a'*64,'browser_download_url':base+'download/'+tag+'/'+n} for n in r.ASSETS]}
  self.assertEqual(r.metadata(data)['repo'],r.REPO)
  for change in [{'draft':True},{'tag_name':'main'},{'html_url':'https://github.com/other/repo/releases/tag/'+tag},{'assets':data['assets'][:1]}]:
   with self.assertRaises(ValueError):r.metadata(dict(data,**change))
  data['assets'][1]['browser_download_url']='https://github.com/evil/release/helper'
  with self.assertRaises(ValueError):r.metadata(data)
 def test_checksum_manifest(self):
  self.assertEqual(r.checksums('a'*64+'  openflux-linux-amd64\n')['openflux-linux-amd64'],'a'*64)
  self.assertEqual(r.checksums('a'*64+'  dist/openflux-linux-amd64\n')['openflux-linux-amd64'],'a'*64)
  for text in ['x  openflux','a'*64+'  ../payload','a'*64+'  same\n'+'b'*64+'  same']:
   with self.assertRaises(ValueError):r.checksums(text)
 def test_downgrade_rejected(self):
  core=types.SimpleNamespace(installed=lambda n:{'version':'v4.0.0','sha256':'a'})
  with self.assertRaises(ValueError):r.no_downgrade(core,{'version':'v4.0.0-rc3','asset_sha256':'b'})
 def test_full_rollback_on_failed_health(self):
  with tempfile.TemporaryDirectory() as temp:
   root=pathlib.Path(temp);binary=root/'binary';binary.write_text('old binary')
   runner=root/'runner';runner.write_text('old runner');helper=root/'helper';helper.write_text('old helper')
   mode=root/'mode';mode.write_text('awg');state=root/'state';state.mkdir();candidate=state/'candidate-test';candidate.mkdir();new=candidate/'binary';new.write_text('new binary')
   core=types.SimpleNamespace(COMPONENTS={'openflux':{'binary':str(binary)}},MODE=mode,sha=digest,
    installed=lambda n:{'version':'v4.0.0-rc2','sha256':digest(binary)},service_active=lambda u:False,
    component_dir=lambda n:state,write_json=lambda p,d:p.write_text(json.dumps(d)),progress=lambda m:None,
    run=lambda *args,**kw:types.SimpleNamespace(returncode=0,stdout=''),atomic_binary=lambda s,d:shutil.copyfile(s,d),
    exclusive=lambda p:contextlib.nullcontext(),guard_active_route=lambda *args:None,remove_update_guard=lambda g:None,
    restore_runtime=lambda *args:None)
   def support(*args):runner.write_text('new runner');helper.write_text('new helper')
   meta={'version':'v4.0.0-rc3','asset_sha256':digest(new),'bundle_schema':1}
   with patch.object(r,'RUNNER',runner),patch.object(r,'HELPER',helper),patch.object(r,'DROPIN',root/'cookie-dropin'),patch.object(r,'LIBDIR',root/'modules'),patch.object(r,'install_support',side_effect=support),patch.object(r,'health',side_effect=RuntimeError('AUTH_BLOCKED')):
    with self.assertRaisesRegex(RuntimeError,'Previous installation restored'):r.replace(core,new,meta)
   self.assertEqual(binary.read_text(),'old binary');self.assertEqual(runner.read_text(),'old runner');self.assertEqual(helper.read_text(),'old helper')
   self.assertEqual(mode.read_text(),'awg');self.assertFalse((state/'installed.json').exists())
   self.assertTrue((state/'transaction.json').exists())
 def test_authentication_states_and_failure_history(self):
  self.assertEqual(a.classify('[YDOCS] AUTH_BLOCKED: waiting')[0],'auth_blocked')
  self.assertEqual(a.classify('[YDOCS] AUTH_BLOCKED cleared: cookie store changed')[0],'connecting')
  self.assertEqual(a.classify('OnlyOffice authentication successful')[0],'connected')
  self.assertEqual(a.classify('Document authentication not accepted')[0],'auth_failed')
  self.assertIsNone(a.classify('Cookie: SECRET_VALUE'))
  with tempfile.TemporaryDirectory() as tmp,patch.object(a,'CLIENT',True),patch.object(a,'NATIVE',True),patch.object(a,'STORE',pathlib.Path(tmp)/'cookies.json'),patch.object(a,'STATE',pathlib.Path(tmp)/'state.json'),patch.object(a,'runtime',return_value=(True,'a'*32)):
   events=[{'__REALTIME_TIMESTAMP':'1000000','MESSAGE':'AUTH_BLOCKED SECRET_VALUE'},{'__REALTIME_TIMESTAMP':'2000000','MESSAGE':'AUTH_BLOCKED cleared'},{'__REALTIME_TIMESTAMP':'3000000','MESSAGE':'OnlyOffice authentication successful'}]
   with patch.object(a,'command',return_value=types.SimpleNamespace(stdout='\n'.join(json.dumps(e) for e in events))):result=a._status()
   self.assertEqual(result['state'],'connected');self.assertEqual(result['last_failure']['state'],'auth_blocked')
   self.assertNotIn('SECRET_VALUE',json.dumps(result))

if __name__=='__main__':unittest.main()
