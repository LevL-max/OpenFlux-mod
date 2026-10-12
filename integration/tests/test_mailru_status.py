"""Mail.ru server status through Yandex Disk (v4.2.2): signed by the server,
fetched by the clients, shown in panel v9."""
import json,time,unittest
from unittest.mock import patch
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric import rsa
import openflux_auth
import recovery_crypto
import recovery_inbox
import router_integration as ri
from router_integration import MAILRU_EXIT_UNIT,MAILRU_UNIT
from test_mailru import Base,DOC
from test_volga import BASE_PANEL

KEY=rsa.generate_private_key(public_exponent=65537,key_size=2048)

class StatusSigningTests(unittest.TestCase):
    def test_mailru_status_has_its_own_schema(self):
        signed=recovery_crypto.sign_status({'state':'connected','reason':'Connected to the Mail.ru document.','active':True,'secret':'x'},KEY,protocol='mailru')
        self.assertEqual(signed['schema'],'openflux-mailru-server-status-v1');self.assertNotIn('secret',json.dumps(signed))
        peer=recovery_crypto.verify_status(signed,KEY.public_key(),protocol='mailru')
        self.assertEqual((peer['state'],peer['stale']),('connected',False))
        # A Volga or Legacy status file is never read as Mail.ru's, nor the reverse.
        for protocol in ('volga','yandex'):
            with self.assertRaises(ValueError):recovery_crypto.verify_status(signed,KEY.public_key(),protocol=protocol)
        with self.assertRaises(ValueError):recovery_crypto.sign_status({},KEY,protocol='other')
        # Mail.ru has no cookie channel.
        with self.assertRaises(ValueError):recovery_crypto.destination('mailru')

class Exchange(Base):
    def setUp(self):
        super().setUp()
        self.keys=self.root/'etc/openflux-recovery';self.keys.mkdir(parents=True)
        (self.keys/'disk-token').write_text('token\n')
        self.runtime.env_path.parent.mkdir(parents=True);self.runtime.env_path.write_text('MAILRU_DOCUMENT='+DOC+'\n')
        self.runtime.units.mkdir(parents=True);(self.runtime.units/self.runtime.unit()).write_text('[Unit]\n')
        for p in (patch.object(openflux_auth,'DISK_TOKEN_REJECTED',self.root/'rejected'),patch.object(ri.MailruRuntime,'path',lambda s,name:s.root/name.lstrip('/'))):
            p.start()

class ServerTests(Exchange):
    role='server'
    def setUp(self):
        super().setUp()
        (self.keys/'server-private.pem').write_bytes(KEY.private_bytes(serialization.Encoding.PEM,serialization.PrivateFormat.PKCS8,serialization.NoEncryption()))
        self.host.active.add(MAILRU_EXIT_UNIT)
        self.uploads=[];patch.object(recovery_inbox,'upload',side_effect=lambda path,value,token:self.uploads.append((path,value))).start()
    def test_the_exit_publishes_its_state_on_a_change_and_hourly(self):
        now=time.time();self.host.journal=[(now-30,'[MAILRU] connected to the document')]
        self.runtime.status_exchange()
        self.assertEqual(len(self.uploads),1);path,value=self.uploads[0]
        self.assertEqual(path,'disk:/OpenFlux Recovery/server-recovery-mailru.json.status.json')
        self.assertEqual(recovery_crypto.verify_status(value,KEY.public_key(),protocol='mailru')['state'],'connected')
        self.runtime.status_exchange();self.assertEqual(len(self.uploads),1)  # unchanged: nothing to send
        # A reconnect within the grace still counts as connected: no upload.
        self.host.journal.append((now-20,'[MAILRU] connection to the document dropped'))
        self.runtime.status_exchange();self.assertEqual(len(self.uploads),1)
        # Refused for longer: the clients learn why.
        self.host.journal=[(now-400,'[MAILRU] connected to the document'),(now-10,'[MAILRU] document server refused the connection (status 409)')]
        (self.runtime.data/'status.json').unlink()
        self.runtime.status_exchange();self.assertEqual(len(self.uploads),2)
        status=recovery_crypto.verify_status(self.uploads[1][1],KEY.public_key(),protocol='mailru')['status']
        self.assertEqual(status['state'],'connecting');self.assertIn('HTTP 409',status['reason'])
    def test_a_refused_disk_token_is_not_sent_again(self):
        import urllib.error
        recovery_inbox.upload.side_effect=urllib.error.HTTPError('u',401,'Unauthorized',{},None)
        self.host.journal=[(time.time(),'[MAILRU] connected to the document')]
        self.runtime.status_exchange()
        self.assertTrue(openflux_auth.disk_token_rejected('token'))
        self.assertEqual(self.runtime.read(self.runtime.data/'recovery.json')['status_publish_error'],'Mail.ru status upload unavailable')
    def test_nothing_happens_before_mailru_is_set_up(self):
        self.runtime.env_path.unlink();self.runtime.status_exchange();self.assertEqual(self.uploads,[])

class ClientTests(Exchange):
    def setUp(self):
        super().setUp()
        (self.keys/'server-public.pem').write_bytes(KEY.public_key().public_bytes(serialization.Encoding.PEM,serialization.PublicFormat.SubjectPublicKeyInfo))
        patch.object(recovery_inbox,'client_poll_due',return_value=True).start()
    def test_the_client_shows_the_servers_exit(self):
        signed=recovery_crypto.sign_status({'state':'connecting','reason':'Mail.ru refused the document connection (HTTP 409); retrying.','active':True},KEY,protocol='mailru')
        with patch.object(recovery_inbox,'download',return_value=signed) as download:self.runtime.status_exchange()
        self.assertTrue(download.call_args.args[0].endswith('-mailru.json.status.json'))
        server=self.runtime.status()['server_authentication']
        self.assertEqual((server['state'],server['stale'],server['fetch_failed']),('connecting',False,False))
        self.assertIn('HTTP 409',server['status']['reason'])
        # A failed fetch keeps the last report but marks it stale.
        with patch.object(recovery_inbox,'download',side_effect=OSError('timeout')):self.runtime.status_exchange()
        server=self.runtime.status()['server_authentication']
        self.assertEqual((server['state'],server['stale'],server['fetch_failed']),('connecting',True,True))
    def test_a_forged_status_is_ignored(self):
        other=rsa.generate_private_key(public_exponent=65537,key_size=2048)
        with patch.object(recovery_inbox,'download',return_value=recovery_crypto.sign_status({'state':'connected'},other,protocol='mailru')):
            self.runtime.status_exchange()
        self.assertEqual(self.runtime.status()['server_authentication']['state'],'unknown')

class PanelTests(unittest.TestCase):
    def test_v8_panel_shows_the_server_exit(self):
        fresh=ri.patch_panel(BASE_PANEL)
        self.assertIn('data-volga-panel="9"',fresh);compile(fresh,'router-panel','exec')
        for piece in ('id="mailruServerState"',"const ms=mp.server_authentication||{}","' · server: '"):self.assertEqual(fresh.count(piece),1,piece)
        v8=fresh.replace(ri.VOLGA_HTML,ri.VOLGA_HTML_V8).replace(ri.VOLGA_JS,ri.VOLGA_JS_V8).replace(ri.VOLGA_HOOK,ri.VOLGA_HOOK_V8)
        self.assertIn('data-volga-panel="8"',v8);self.assertNotIn('mailruServerState',v8)
        self.assertEqual(ri.patch_panel(v8),fresh);self.assertEqual(ri.patch_panel(fresh),fresh)
        for unsafe in ('innerHTML','\\','`'):self.assertNotIn(unsafe,ri.MAILRU_SERVER_HOOK)

if __name__=='__main__':unittest.main()
