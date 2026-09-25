import unittest,json
from cryptography.hazmat.primitives.asymmetric import rsa,ed25519
import recovery_crypto as r
class RecoveryTests(unittest.TestCase):
 @classmethod
 def setUpClass(cls):cls.server=rsa.generate_private_key(public_exponent=65537,key_size=3072);cls.sender=ed25519.Ed25519PrivateKey.generate()
 def test_roundtrip(self):
  e=r.seal('curl https://docs.yandex.ru -b "cookie=TEST"',self.server.public_key(),self.sender,now=1000)
  self.assertNotIn('cookie=TEST',json.dumps(e));text,ident=r.unseal(e,self.server,self.sender.public_key(),[],now=1001);self.assertIn('cookie=TEST',text)
  with self.assertRaises(ValueError):r.unseal(e,self.server,self.sender.public_key(),[ident],now=1001)
  with self.assertRaises(ValueError):r.unseal(e,self.server,self.sender.public_key(),[],now=4601)
 def test_tampering_and_wrong_sender(self):
  e=r.seal('test',self.server.public_key(),self.sender,now=1000)
  with self.assertRaises(Exception):r.unseal(e,self.server,ed25519.Ed25519PrivateKey.generate().public_key(),[],now=1001)
  e['target']='other'
  with self.assertRaises(Exception):r.unseal(e,self.server,self.sender.public_key(),[],now=1001)
if __name__=='__main__':unittest.main()
