import base64,hashlib,json,pathlib,tempfile,types,unittest,sys
from unittest.mock import patch
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric import ed25519,rsa
from router_integration import OpenFluxConfigs,patch_config_panel

class ConfigPanelTests(unittest.TestCase):
 def setUp(self):
  self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup);self.root=pathlib.Path(self.tmp.name)
  self.url='https://disk.yandex.ru/i/old'
  self.node={'role':'client','document_url':self.url,'recovery_path':'disk:/Recovery/cookies.json','channel':'stable','args':['--url',self.url],'binary':'/opt/fixed'}
  self.put('/etc/openflux/node.json',json.dumps(self.node).encode())
  self.put('/etc/openflux-client/yandex.env',('YANDEX_URL='+self.url+'\n').encode())
  self.put('/etc/router/updater/settings.json',b'{"openflux_prereleases": false, "openflux_expected_exit_ip": "", "other": 1}')
  self.put('/etc/openflux-recovery/disk-token',b'test_token_1234567890\n')
  self.key=ed25519.Ed25519PrivateKey.generate();self.put('/etc/openflux-recovery/sender-private.pem',self.pem(self.key))
  self.put('/etc/openflux-recovery/sender-public.pem',self.public(self.key))
  self.put('/etc/openflux-recovery/server-public.pem',self.public(rsa.generate_private_key(public_exponent=65537,key_size=3072)))
  self.put('/var/lib/openflux-client/yandex-cookies.json',json.dumps({self.url:{'session':'test'}}).encode())
  self.core=types.SimpleNamespace(registry=lambda:{'openflux-url':{'path':str(self.p('/etc/openflux-client/yandex.env')),'label':'URL','kind':'url','file':'yandex.env'}},
   upload=lambda body:{'delegated':True},parse=lambda data:data.decode(),contents=lambda path:pathlib.Path(path).read_bytes(),digest=lambda data:hashlib.sha256(data).hexdigest(),
   env_values=lambda text:dict(line.split('=',1) for line in text.strip().splitlines()),ROOT=self.root/'state',BACKUPS=self.root/'backups',MAX_FILE=1048576)
  self.extension=OpenFluxConfigs(self.core,self.root)
  self.addCleanup(patch.stopall)
  patch.dict(sys.modules,{'xray_control':types.SimpleNamespace(atomic=lambda path,obj:path.write_text(json.dumps(obj)))}).start()
 def p(self,name):return self.root/name.lstrip('/')
 def put(self,name,data):
  p=self.p(name);p.parent.mkdir(parents=True,exist_ok=True);p.write_bytes(data);p.chmod(0o600)
 def pem(self,key):return key.private_bytes(serialization.Encoding.PEM,serialization.PrivateFormat.PKCS8,serialization.NoEncryption())
 def public(self,key):return key.public_key().public_bytes(serialization.Encoding.PEM,serialization.PublicFormat.SubjectPublicKeyInfo)
 def body(self,target,data):
  path=self.extension.registry()[target]['path']
  return {'target':target,'data':base64.b64encode(data).decode(),'confirm':True,'revision':self.core.digest(self.core.contents(path))}
 def upload(self,target,data):return self.extension.upload(self.body(target,data))
 def test_url_sync_backup_and_stale_upload(self):
  old=self.p('/etc/openflux/node.json').read_bytes();body=self.body('openflux-url',b'YANDEX_URL=https://disk.yandex.ru/i/new\n')
  result=self.extension.upload(body);node=json.loads(self.p('/etc/openflux/node.json').read_bytes())
  self.assertEqual(node['document_url'],'https://disk.yandex.ru/i/new');self.assertEqual(node['args'][-1],node['document_url'])
  backup=pathlib.Path(result['backup']);self.assertTrue(any((backup/x['copy']).read_bytes()==old for x in json.loads((backup/'manifest.json').read_text())))
  with self.assertRaisesRegex(ValueError,'changed since'):self.extension.upload(body)
 def test_node_rejects_managed_paths_and_syncs_channel(self):
  node=dict(self.node,binary='/tmp/untrusted')
  with self.assertRaises(ValueError):self.upload('openflux-node',json.dumps(node).encode())
  node=dict(self.node,channel='prerelease',recovery_path='disk:/New/cookies.json')
  self.upload('openflux-node',json.dumps(node).encode())
  self.assertTrue(json.loads(self.p('/etc/router/updater/settings.json').read_text())['openflux_prereleases'])
  changed={'openflux_prereleases':False,'openflux_expected_exit_ip':'','other':2}
  with self.assertRaises(ValueError):self.upload('openflux-updater',json.dumps(changed).encode())
 def test_keys_validate_and_private_import_updates_public(self):
  another=ed25519.Ed25519PrivateKey.generate()
  with self.assertRaises(ValueError):self.upload('openflux-client-public',self.public(another))
  self.upload('openflux-client-key',self.pem(another))
  self.assertEqual(self.p('/etc/openflux-recovery/sender-public.pem').read_bytes(),self.public(another))
  with self.assertRaises(ValueError):self.upload('openflux-server-key',self.public(another))
  self.assertEqual(self.p('/etc/openflux-recovery/sender-private.pem').stat().st_mode&0o777,0o600)
 def test_token_cookie_validation_and_unrelated_delegation(self):
  for token in (b'curl https://example.test',b'{"token":"abc"}',b'line1\nline2'):
   with self.assertRaises(ValueError):self.upload('openflux-disk-token',token)
  with self.assertRaises(ValueError):self.upload('openflux-cookies',b'{"https://disk.yandex.ru/i/other":{"a":"b"}}')
  self.assertFalse(self.upload('openflux-cookies',self.p('/var/lib/openflux-client/yandex-cookies.json').read_bytes())['changed'])
  self.assertEqual(self.extension.upload({'target':'awg-1'}),{'delegated':True})
  self.assertTrue(all(x.get('description') for x in self.extension.registry().values()))
 def test_partial_write_failure_restores_both_files(self):
  import os
  before={p:p.read_bytes() for p in (self.p('/etc/openflux-client/yandex.env'),self.p('/etc/openflux/node.json'))}
  real=os.replace;calls=[]
  def fail_second(src,dst):
   calls.append(dst)
   if len(calls)==2:raise OSError('simulated write failure')
   return real(src,dst)
  with patch('os.replace',side_effect=fail_second),self.assertRaises(OSError):self.upload('openflux-url',b'YANDEX_URL=https://disk.yandex.ru/i/new\n')
  for p,content in before.items():self.assertEqual(p.read_bytes(),content)
 def test_panel_patch_is_idempotent_and_adds_descriptions(self):
  source='import panel_extra\n'+'<div class="small" style="overflow-wrap:anywhere">${esc(c.path)}</div>'+' accept=".json,.yaml,.yml,.conf,.env,.txt"'
  changed=patch_config_panel(source)
  self.assertEqual(patch_config_panel(changed),changed);self.assertIn('extend_config_store(config_store)',changed);self.assertIn('c.description',changed);self.assertIn('.pem',changed)

if __name__=='__main__':unittest.main()
