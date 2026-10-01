import json,pathlib,tempfile,time,unittest,urllib.error,urllib.parse
from unittest.mock import patch
from cryptography.hazmat.primitives.asymmetric import rsa,ed25519
from cryptography.hazmat.primitives import serialization
import openflux_auth,recovery_crypto,recovery_inbox

def pem(key,private):
 if private:return key.private_bytes(serialization.Encoding.PEM,serialization.PrivateFormat.PKCS8,serialization.NoEncryption())
 return key.public_bytes(serialization.Encoding.PEM,serialization.PublicFormat.SubjectPublicKeyInfo)

class LegacyInboxTests(unittest.TestCase):
 """recovery_inbox.main on a server and a client against a fake Disk."""
 def setUp(self):
  self.temp=tempfile.TemporaryDirectory();self.addCleanup(self.temp.cleanup);root=pathlib.Path(self.temp.name)
  self.config=root/'etc';self.config.mkdir();self.state=root/'lib'/'state.json';self.state.parent.mkdir()
  self.server=rsa.generate_private_key(public_exponent=65537,key_size=2048);sender=ed25519.Ed25519PrivateKey.generate()
  for name,key,private in [('server-private.pem',self.server,True),('server-public.pem',self.server.public_key(),False),('sender-public.pem',sender.public_key(),False)]:
   (self.config/name).write_bytes(pem(key,private))
  (self.config/'disk-token').write_text('fixture-token')
  self.envelope=recovery_crypto.seal('curl https://docs.yandex.ru/edit -b "Session_id=fixture"',self.server.public_key(),sender)
  self.status={'state':'connected','active':True,'needs_cookies':False}
  self.calls=[];self.down=False;self.refuse=False;self.client=False
  self.start=int(time.time());self.clock=[self.start]
  def start(p):
   self.addCleanup(p.stop);return p.start()
  for target,name,value in [(recovery_inbox,'CONFIG',self.config),(recovery_inbox,'STATE',self.state),(recovery_inbox,'LOCK',root/'lock'),
                            (openflux_auth,'NODE',{'recovery_path':'disk:/OpenFlux Recovery/aws-recovery.json'}),(openflux_auth,'CLIENT',False),
                            (openflux_auth,'DISK_TOKEN_REJECTED',root/'rejected'),(openflux_auth,'STATUS_VIEWED',root/'viewed')]:
   start(patch.object(target,name,value))
  start(patch.object(recovery_inbox,'fetch',side_effect=self.fetch));start(patch.object(recovery_inbox,'upload',side_effect=self.upload))
  start(patch.object(openflux_auth,'status',side_effect=lambda:dict(self.status)))
  self.imported=start(patch.object(openflux_auth,'import_cookies',return_value={'count':1}))
  start(patch('time.time',side_effect=lambda:self.clock[0]))

 def fetch(self,url,limit=230000,token=None,timeout=20):
  if self.refuse:self.calls.append('refused');raise urllib.error.HTTPError(url,401,'Unauthorized',{},None)
  if self.down:self.calls.append('down');raise OSError('Disk unavailable')
  split=urllib.parse.urlsplit(url)
  if split.netloc=='cloud-api.yandex.net':
   self.calls.append('meta' if split.path.endswith('/resources') else 'link');return {'md5':'md5-1','href':'https://downloader.disk.yandex.ru/file'}
  self.calls.append('file')
  return recovery_crypto.sign_status(dict(self.status),self.server,now=self.clock[0]) if self.client else self.envelope

 def upload(self,path,envelope,token):
  if self.refuse:self.calls.append('refused');raise urllib.error.HTTPError(path,401,'Unauthorized',{},None)
  if self.down:self.calls.append('down');raise OSError('Disk unavailable')
  self.calls.append('upload')

 def at(self,seconds):
  self.calls.clear();self.clock[0]=self.start+seconds;recovery_inbox.main();return list(self.calls)

 def saved(self):return json.loads(self.state.read_text())

 def test_a_working_server_checks_rarely_and_a_waiting_one_often(self):
  self.assertEqual(self.at(0),['upload','meta','link','file']);self.assertEqual(self.imported.call_count,1)
  for minute in range(1,15):self.assertEqual(self.at(60*minute),[])
  self.assertEqual(self.at(900),['meta'])  # an unchanged file is not downloaded
  self.status.update(state='auth_blocked',needs_cookies=True)
  self.assertEqual(self.at(960),['upload'])  # a change is published at once
  self.assertEqual(self.at(1019),[])
  self.assertEqual(self.at(1020),['meta'])  # waiting for cookies: every 2 min
  self.status.update(state='connected',needs_cookies=False)
  self.assertEqual(self.at(1080),['upload'])
  self.assertEqual(self.at(1919),[])
  self.assertEqual(self.at(1920),['meta'])
  self.assertEqual(self.at(1080+3600),['upload','meta'])  # the hourly heartbeat

 def test_a_refused_token_silences_the_server_until_a_new_token(self):
  self.refuse=True
  self.assertEqual(self.at(0),['refused'])
  self.assertEqual((self.saved()['inbox'],self.saved()['needs_attention']),('DISK_AUTH_REQUIRED',True))
  for minute in range(1,30):self.assertEqual(self.at(60*minute),[])
  self.refuse=False;(self.config/'disk-token').write_text('fresh-token')
  self.assertEqual(self.at(1800),['upload','meta','link','file'])
  self.assertEqual((self.saved()['inbox'],self.saved()['failures']),('Applied',0))

 def test_an_unchanged_inbox_is_downloaded_again_after_a_disk_failure(self):
  self.at(0)
  self.down=True;self.assertEqual(self.at(900),['down'])
  state=self.saved();self.assertEqual((state['inbox'],state['needs_attention']),('Yandex Disk temporarily unavailable',True))
  self.assertNotIn('inbox_md5',state)
  self.down=False;self.assertEqual(self.at(1800),['meta','link','file'])
  state=self.saved();self.assertEqual((state['inbox'],state['failures'],state['needs_attention']),('Already applied',0,False))

 def test_a_client_fetches_the_server_status_while_someone_looks(self):
  self.client=True
  peer=self.state.with_name('peer-status.json')
  with patch.object(openflux_auth,'CLIENT',True):
   self.assertEqual(self.at(0),['link','file']);self.assertFalse(json.loads(peer.read_text())['fetch_failed'])
   self.assertEqual(self.at(600),[])  # nobody looks
   with patch.object(openflux_auth,'status_viewed',return_value=True):
    self.assertEqual(self.at(600),['link','file'])
    self.assertEqual(self.at(659),[])
    self.refuse=True;self.assertEqual(self.at(660),['refused'])
    self.assertTrue(json.loads(peer.read_text())['fetch_failed'])
    self.assertEqual(self.at(720),[])  # the refused token is not sent again

 def test_a_dead_storage_host_gets_a_new_link(self):
  replies=[OSError('timed out'),self.envelope]
  def fetch(url,limit=230000,token=None,timeout=20):
   if url.startswith('https://cloud-api.'):self.calls.append('link');return {'href':'https://downloader.disk.yandex.ru/file'}
   self.calls.append('file:%d'%timeout);reply=replies.pop(0)
   if isinstance(reply,Exception):raise reply
   return reply
  with patch.object(recovery_inbox,'fetch',side_effect=fetch):
   self.assertEqual(recovery_inbox.download('disk:/x','fixture-token'),self.envelope)
  timeout=recovery_inbox.DOWNLOAD_TIMEOUT
  self.assertEqual(self.calls,['link','file:%d'%timeout,'link','file:%d'%timeout])
