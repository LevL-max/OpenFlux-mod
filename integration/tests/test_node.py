import contextlib,hashlib,io,json,pathlib,shutil,tarfile,tempfile,types,unittest
from unittest.mock import patch
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric import rsa,ed25519
import openflux_node as n
import openflux_auth as a
import recovery_crypto

class NodeTests(unittest.TestCase):
 def test_fresh_client_and_server_install_from_verified_release(self):
  # Exercise the actual installer on isolated files, replacing only network, user and service calls.
  # No extra machine, running container, firewall rule or host service is created.
  source=pathlib.Path(n.__file__).parent
  bundle=io.BytesIO()
  with tarfile.open(fileobj=bundle,mode='w:gz') as tar:
   for name in ('openflux_release.py','openflux_auth.py','cookie_import.py','recovery_crypto.py','recovery_inbox.py','router_integration.py','install.py','README.md'):
    data=(source/name).read_bytes();info=tarfile.TarInfo('integration/'+name);info.size=len(data);tar.addfile(info,io.BytesIO(data))
  assets={'openflux-linux-amd64':b'release-binary','openflux-yandex-cookie-import':(source/'cookie_import.py').read_bytes(),'openflux-integration-linux.tar.gz':bundle.getvalue(),'openflux-node.py':pathlib.Path(n.__file__).read_bytes()}
  assets['SHA256SUMS']=''.join(hashlib.sha256(v).hexdigest()+'  '+k+'\n' for k,v in assets.items()).encode()
  base='https://github.com/'+n.REPO+'/releases/'
  row={'tag_name':'v4.0.2','html_url':base+'tag/v4.0.2','published_at':'2026-09-25','assets':[{'name':k,'digest':'sha256:'+hashlib.sha256(v).hexdigest(),'browser_download_url':base+'download/v4.0.2/'+k} for k,v in assets.items()]}
  for role in ('client','server'):
   with self.subTest(role=role),tempfile.TemporaryDirectory() as temp,contextlib.ExitStack() as stack:
    root=pathlib.Path(temp);commands=root/'commands';commands.mkdir();units=root/'units';units.mkdir()
    calls=[]
    def fetch(url,path=None,limit=None):
     if path:pathlib.Path(path).write_bytes(assets[url.rsplit('/',1)[-1]])
     else:return [row]
    def run(argv,**kwargs):calls.append(argv);return types.SimpleNamespace(returncode=1 if argv[:2]==['docker','inspect'] else 0,stdout='--yandex-cookie-store',stderr='')
    for field,value in [('CONFIG',root/'config'/'node.json'),('STATE',root/'state'),('HELPER',commands/'cookie-helper'),('COMMAND_DIR',commands),('UNIT_DIR',units),('RUNTIME_DIR',root/'runtime')]:stack.enter_context(patch.object(n,field,value))
    stack.enter_context(patch.object(n,'fetch',side_effect=fetch));stack.enter_context(patch.object(n,'run',side_effect=run));stack.enter_context(patch.object(n.os,'chown'))
    stack.enter_context(patch.object(n.pwd,'getpwnam',return_value=types.SimpleNamespace(pw_uid=0,pw_gid=0)))
    args=types.SimpleNamespace(action='install',role=role,backend=None,binary=str(root/'bin'/'openflux'),library=str(root/'lib'),service='custom.service',container='custom-server',cookie_store=str(root/'cookies'/'store.json'),document_url='https://example.test/document',transport='yandex',socks='127.0.0.1:11880',channel='stable',args_file=None,router_updater=False)
    result=n.setup(args);profile=n.config()
    self.assertEqual(result['installed'],'v4.0.2');self.assertEqual(profile['role'],role);self.assertEqual(pathlib.Path(args.binary).read_bytes(),b'release-binary')
    self.assertTrue((commands/'openfluxctl').is_file());self.assertTrue((units/'openflux-auth-status.timer').is_file())
    self.assertEqual(profile['backend'],'docker' if role=='server' else 'systemd')
    self.assertFalse(any(argv[:2]==['docker','start'] or argv[0]=='iptables' for argv in calls))
    if role=='server':self.assertTrue(any(argv[:2]==['docker','create'] for argv in calls));self.assertTrue((root/'runtime'/'run.sh').is_file())
    else:self.assertTrue((units/'custom.service').is_file())
    with self.assertRaisesRegex(ValueError,'Already configured'):n.setup(args)

 def test_failed_server_update_restores_binary_helpers_and_stopped_state(self):
  with tempfile.TemporaryDirectory() as temp:
   root=pathlib.Path(temp);state=root/'state';state.mkdir();lib=root/'lib';lib.mkdir()
   binary=root/'openflux';binary.write_text('old binary');(lib/'openflux_node.py').write_text('old updater');(lib/'openflux_auth.py').write_text('old status')
   candidate=state/'candidate';candidate.mkdir();(candidate/'integration').mkdir()
   for name,content in [('openflux-linux-amd64','new binary'),('openflux-node.py','x=1'),('openflux-yandex-cookie-import','new helper')]: (candidate/name).write_text(content)
   (candidate/'integration'/'openflux_auth.py').write_text('x=2')
   assets={p.name:n.sha(p) for p in candidate.iterdir() if p.is_file()}
   n.save(state/'staged.json',{'version':'v5.0.0','directory':str(candidate),'assets':assets,'base_sha256':n.sha(binary)})
   c={'binary':str(binary),'library':str(lib),'backend':'docker','container':'any-server','role':'server'}
   actions=[];module=types.SimpleNamespace(MODULES=('openflux_auth.py',),extract_bundle=lambda *args:None)
   # Every mutable host path is redirected; service actions are recorded, not executed.
   with patch.object(n,'STATE',state),patch.object(n,'HELPER',root/'helper'),patch.object(n,'support',return_value=module),patch.object(n,'active',return_value=False),patch.object(n,'identify',return_value={'version':'v4.0.1','sha256':n.sha(binary)}),patch.object(n,'runtime_action',side_effect=lambda c,op:actions.append(op)),patch.object(n,'run',side_effect=lambda argv,**kw:types.SimpleNamespace(returncode=0,stdout=n.sha(binary)+'  /usr/local/bin/openflux' if argv[0]=='docker' else '')),patch.object(n,'health',side_effect=RuntimeError('AUTH_BLOCKED')):
    with self.assertRaisesRegex(RuntimeError,'AUTH_BLOCKED'):n.install_staged(c)
   self.assertEqual(binary.read_text(),'old binary');self.assertEqual((lib/'openflux_auth.py').read_text(),'old status');self.assertEqual((lib/'openflux_node.py').read_text(),'old updater')
   self.assertEqual(actions,['stop','start','stop']);self.assertFalse((state/'transaction.json').exists())
   self.assertEqual(n.read(state/'installed.json')['version'],'v4.0.1')

 def test_release_asset_cannot_redirect_to_another_repository(self):
  row={'tag_name':'v9.1.0','html_url':'https://github.com/'+n.REPO+'/releases/tag/v9.1.0','assets':[{'name':'openflux-linux-amd64','digest':'sha256:'+'a'*64,'browser_download_url':'https://example.com/payload'}]}
  with tempfile.TemporaryDirectory() as temp,patch.object(n,'fetch') as fetch:
   with self.assertRaisesRegex(ValueError,'Unverified asset'):n.verify_download(row,pathlib.Path(temp))
   fetch.assert_not_called()

 def test_client_creates_recovery_package_without_windows_or_local_cookie_change(self):
  with tempfile.TemporaryDirectory() as temp:
   root=pathlib.Path(temp);server=rsa.generate_private_key(public_exponent=65537,key_size=3072);signer=ed25519.Ed25519PrivateKey.generate()
   (root/'server-public.pem').write_bytes(server.public_key().public_bytes(serialization.Encoding.PEM,serialization.PublicFormat.SubjectPublicKeyInfo))
   (root/'sender-private.pem').write_bytes(signer.private_bytes(serialization.Encoding.PEM,serialization.PrivateFormat.PKCS8,serialization.NoEncryption()))
   cookie='curl https://example.test/document -b "session=fixture"'
   with patch.object(a,'CLIENT',True),patch.object(a,'RECOVERY_DIR',root),patch.object(a,'NODE',{'recovery_path':'disk:/Custom Channel/other-server.json'}),patch.object(a,'import_cookies') as local_import:
    result=a.package_cookies(cookie)
    self.assertEqual(result['filename'],'other-server.json');self.assertNotIn('session=fixture',json.dumps(result))
    decoded,_=recovery_crypto.unseal(result['package'],server,signer.public_key(),[])
    self.assertEqual(decoded,cookie);local_import.assert_not_called()

 def test_native_server_status_reads_its_configured_service(self):
  with patch.object(a,'NATIVE',True),patch.object(a,'UNIT','custom-exit.service'),patch.object(a,'command',return_value=types.SimpleNamespace(stdout='ActiveState=active\nInvocationID='+'b'*32)) as command:
   self.assertEqual(a.runtime(),(True,'b'*32));self.assertEqual(command.call_args.args[0][2],'custom-exit.service')

if __name__=='__main__':unittest.main()
