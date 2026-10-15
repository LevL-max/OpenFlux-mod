"""Mail.ru browser imports, isolated recovery, retained status and panel v10."""
import copy,json,os,pathlib,shlex,time,types,unittest,urllib.error
from unittest.mock import patch
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric import ed25519
import openflux_auth,recovery_crypto,recovery_inbox,router_integration as ri
from test_mailru import Base,DOC,Handler
from test_mailru_status import KEY
from test_volga import BASE_PANEL

def curl(document=DOC,cookie='fixture-secret',endpoint='https://cloud.mail.ru/api/v4/r7/edit?token=query-fixture'):
    body={'public':'/'+document.split('/public/')[1],'x-email':'fixture@example.test','token':'body-fixture','platform':'desktop_web'}
    return 'curl '+shlex.quote(endpoint)+' -b '+shlex.quote('Mpop='+cookie+'; other=abc')+' -H '+shlex.quote('User-Agent: FixtureBrowser')+' -H '+shlex.quote('X-Csrf-Token: header-fixture')+' -H '+shlex.quote('Host: evil.invalid')+' --data-raw '+shlex.quote(json.dumps(body))

class BrowserTests(Base):
    def setUp(self):
        super().setUp();self.runtime.configure(DOC)
    def test_import_is_private_atomic_and_preserves_request_and_other_protocols(self):
        legacy=self.root/'legacy';legacy.write_text('unchanged')
        result=self.runtime.import_cookies(DOC,curl())
        path=self.runtime.path(ri.MAILRU_BROWSER);profile=json.loads(path.read_text())
        self.assertEqual(profile['cookies']['Mpop'],'fixture-secret')
        self.assertEqual(profile['headers'],{'user-agent':'FixtureBrowser','x-csrf-token':'header-fixture'})
        self.assertEqual(profile['query'],{'token':['query-fixture']})
        self.assertEqual(profile['body']['token'],'body-fixture')
        self.assertEqual(path.stat().st_mode&511,0o600);self.assertEqual(path.parent.stat().st_mode&511,0o700)
        self.assertEqual(path.stat().st_uid,os.getuid());self.assertEqual(legacy.read_text(),'unchanged')
        inode=path.stat().st_ino;self.runtime.import_cookies(DOC,curl(cookie='replacement'))
        self.assertNotEqual(path.stat().st_ino,inode)
        self.assertNotIn('fixture-secret',json.dumps(result))
    def test_public_page_curl_bootstraps_without_editor_body(self):
        text='curl --url '+shlex.quote(DOC+'?weblink=AbCd/EfGh1234')+' -b '+shlex.quote('Mpop=fixture:person@example.test:; sdcs=fixture; o=person@example.test:fixture')+' -H '+shlex.quote('User-Agent: FixtureBrowser')
        profile=self.runtime.parse_browser(DOC,text)
        self.assertTrue(profile['bootstrap']);self.assertEqual(profile['body']['x-email'],'person@example.test')
        self.assertEqual(profile['body']['public'],'/AbCd/EfGh1234');self.assertEqual(profile['query'],{})
        self.runtime.import_cookies(DOC,text)
        for changed in (text.replace('weblink=AbCd/EfGh1234','weblink=Other/Doc'),text.replace(DOC,DOC+'other'),text+' --data-raw '+shlex.quote('{}')):
            with self.assertRaises(ValueError):self.runtime.parse_browser(DOC,changed)
    def test_request_validation_rejects_other_hosts_documents_files_and_injection(self):
        for text in (curl(endpoint='https://evil.invalid/api/v4/r7/edit'),curl(endpoint='https://cloud.mail.ru@evil.invalid/api/v4/r7/edit'),curl(endpoint='https://cloud.mail.ru/api/v4/r7/edit#fragment'),curl(document=DOC+'other'),curl().replace('--data-raw','--data-raw @'),curl().replace('Mpop=fixture-secret','Mpop=bad\nheader'),curl().replace('curl ','wget ',1)):
            with self.subTest(text=text[:50]),self.assertRaises(ValueError):self.runtime.parse_browser(DOC,text)
        with self.assertRaises(ValueError):self.runtime.parse_browser(DOC+'other',curl())
        with self.assertRaises(ValueError):self.runtime.parse_browser(DOC,'x'*120001)
        profile=self.runtime.parse_browser(DOC,curl());profile['headers']['Authorization']='bad\r\nheader'
        with self.assertRaises(ValueError):self.runtime.validate_browser(profile,DOC)
    def test_auth_block_is_retained_after_stopping_and_new_cookies_are_pending(self):
        self.host.active.add(ri.MAILRU_UNIT);now=time.time()
        self.host.journal=[(now,'[M-AUTH] AUTH_BLOCKED: Mail.ru requires fresh browser cookies; waiting for a new import')]
        status=self.runtime.status();self.assertEqual(status['state'],'auth_blocked');self.assertTrue(status['needs_cookies'])
        with self.assertRaisesRegex(RuntimeError,'requires fresh'):self.runtime.health(2)
        self.host.active.clear();stopped=self.runtime.status()
        self.assertEqual(stopped['state'],'stopped');self.assertEqual(stopped['client_check']['result'],'refused')
        self.runtime.import_cookies(DOC,curl());os.utime(self.runtime.path(ri.MAILRU_BROWSER),(now+5,now+5))
        self.assertTrue(self.runtime.status()['client_check']['cookies_saved_after'])
    def test_old_binary_401_is_also_an_auth_block(self):
        self.assertEqual(ri.MailruRuntime.classify('[M-DOCS] cannot open the document: API returned status 401; retrying')[0],'auth_blocked')
    def test_config_card_validates_profile_and_keeps_secrets_private(self):
        self.runtime.import_cookies(DOC,curl());core=types.SimpleNamespace(registry=lambda:{},upload=lambda b:None,parse=lambda b:b.decode())
        configs=ri.OpenFluxConfigs(core,self.root);self.assertIn('openflux-mailru-cookies',configs.registry())
        data=self.runtime.path(ri.MAILRU_BROWSER).read_bytes()
        writes=configs.plan('openflux-mailru-cookies',data,lambda p:pathlib.Path(p).read_bytes())
        self.assertEqual(writes[self.runtime.path(ri.MAILRU_BROWSER)],data)
        profile=json.loads(data);profile['document']=DOC+'bad'
        with self.assertRaises(ValueError):configs.plan('openflux-mailru-cookies',json.dumps(profile).encode(),lambda p:pathlib.Path(p).read_bytes())

class RecoveryTests(Base):
    def setUp(self):
        super().setUp();self.runtime.configure(DOC)
        self.keys=self.runtime.path('/etc/openflux-recovery');self.keys.mkdir(parents=True)
        self.signer=ed25519.Ed25519PrivateKey.generate()
        (self.keys/'server-public.pem').write_bytes(KEY.public_key().public_bytes(serialization.Encoding.PEM,serialization.PublicFormat.SubjectPublicKeyInfo))
        (self.keys/'sender-private.pem').write_bytes(self.signer.private_bytes(serialization.Encoding.PEM,serialization.PrivateFormat.PKCS8,serialization.NoEncryption()))
        (self.keys/'server-private.pem').write_bytes(KEY.private_bytes(serialization.Encoding.PEM,serialization.PrivateFormat.PKCS8,serialization.NoEncryption()))
        (self.keys/'sender-public.pem').write_bytes(self.signer.public_key().public_bytes(serialization.Encoding.PEM,serialization.PublicFormat.SubjectPublicKeyInfo))
        (self.keys/'disk-token').write_text('disk-fixture')
        patch.object(openflux_auth,'DISK_TOKEN_REJECTED',self.root/'disk-rejected').start()
    def server(self):
        self.runtime.save(self.runtime.node_path,dict(self.node,role='server'))
        patch.object(self.runtime,'status',return_value={'state':'auth_blocked','needs_cookies':True}).start()
    def packet(self):return self.runtime.package_cookies(DOC,curl())['package']
    def test_package_is_encrypted_separated_and_send_does_not_save_locally(self):
        packet=self.packet();self.assertNotIn('fixture-secret',json.dumps(packet))
        payload,ident=recovery_crypto.unseal(packet,KEY,self.signer.public_key(),[],protocol='mailru')
        self.assertEqual(json.loads(payload)['document'],DOC)
        for protocol in ('volga','yandex'):
            with self.assertRaises(ValueError):recovery_crypto.unseal(packet,KEY,self.signer.public_key(),[],protocol=protocol)
        with self.assertRaises(ValueError):recovery_crypto.unseal(packet,KEY,self.signer.public_key(),[ident],protocol='mailru')
        with patch.object(recovery_inbox,'upload') as upload:self.runtime.package_cookies(DOC,curl(),upload=True)
        self.assertIn('-mailru-',upload.call_args.args[0]);self.assertFalse(self.runtime.path(ri.MAILRU_BROWSER).exists())
    def test_poll_imports_once_and_preserves_status_state(self):
        packet=self.packet();self.server();self.runtime.save(self.runtime.data/'recovery.json',{'status_published_at':42})
        with patch.object(recovery_inbox,'disk_md5',return_value='fixture-md5'),patch.object(recovery_inbox,'download',return_value=packet),patch.object(self.runtime,'import_cookies',wraps=self.runtime.import_cookies) as imported:
            self.runtime.recovery_poll();self.runtime.recovery_poll();self.assertEqual(imported.call_count,1)
        state=self.runtime.read(self.runtime.data/'recovery.json');self.assertEqual(state['status_published_at'],42)
        self.assertEqual(len(state['seen']),1);self.assertTrue(self.runtime.path(ri.MAILRU_BROWSER).exists())
    def test_wrong_document_and_wrong_protocol_do_not_import(self):
        bad=recovery_crypto.seal(json.dumps({'protocol':'mailru-chat-auth-v1','document':DOC+'other','curl':curl()}),KEY.public_key(),self.signer,protocol='mailru')
        self.server()
        with patch.object(recovery_inbox,'disk_md5',return_value='bad'),patch.object(recovery_inbox,'download',return_value=bad):self.runtime.recovery_poll()
        self.assertFalse(self.runtime.path(ri.MAILRU_BROWSER).exists())
    def test_disk_rejection_stops_polling_until_token_replacement(self):
        self.server();error=urllib.error.HTTPError('fixture',401,'Unauthorized',{},None)
        with patch.object(recovery_inbox,'disk_md5',side_effect=error) as metadata:
            self.runtime.recovery_poll();self.runtime.recovery_poll();self.assertEqual(metadata.call_count,1)
        self.assertTrue(openflux_auth.disk_token_rejected('disk-fixture'))
        state=self.runtime.read(self.runtime.data/'recovery.json');self.assertNotIn('md5',next(iter(state['inboxes'].values())))
        self.assertFalse(openflux_auth.disk_token_rejected('replacement'))

class InitialServerSetupTests(Base):
    role='server'
    def test_first_setup_keeps_configuration_for_a_later_cookie_import(self):
        self.host.journal=[(time.time(),'[M-AUTH] AUTH_BLOCKED: Mail.ru requires fresh browser cookies')]
        result=self.runtime.configure(DOC)
        self.assertTrue(result['needs_cookies'])
        self.assertTrue(self.runtime.installed());self.assertEqual(self.runtime.document(),DOC)
        self.assertIn(ri.MAILRU_EXIT_UNIT,self.host.active)
        # Native release health checks must still reject an unauthenticated exit.
        with self.assertRaisesRegex(RuntimeError,'requires fresh'):self.runtime.health(1)
        self.runtime.import_cookies(DOC,curl())
        self.assertTrue(self.runtime.path(ri.MAILRU_BROWSER).is_file())
    def test_existing_server_configuration_still_rolls_back_on_auth_failure(self):
        self.host.journal=[(time.time()-5,'[M-DOCS] connected to the document')]
        self.runtime.configure(DOC)
        self.host.journal.append((time.time(),'[M-AUTH] AUTH_BLOCKED: Mail.ru requires fresh browser cookies'))
        with self.assertRaisesRegex(RuntimeError,'requires fresh'):
            self.runtime.configure('https://cloud.mail.ru/public/Zzzz/Yyyy')
        self.assertEqual(self.runtime.document(),DOC)

class PanelTests(unittest.TestCase):
    def test_all_panel_versions_migrate_and_v10_is_idempotent(self):
        fresh=ri.patch_panel(BASE_PANEL)
        for version in range(4,10):
            old=fresh.replace(ri.VOLGA_HTML,getattr(ri,'VOLGA_HTML_V'+str(version))).replace(ri.VOLGA_JS,getattr(ri,'VOLGA_JS_V'+str(version))).replace(ri.VOLGA_HOOK,getattr(ri,'VOLGA_HOOK_V'+str(version)))
            self.assertEqual(ri.patch_panel(old),fresh)
        self.assertEqual(ri.patch_panel(fresh),fresh);compile(fresh,'panel','exec')
        for text in ('data-volga-panel="10"','id="mailruCurl"','id="mailruCookieDocument"','id="mailruCookiesSave"','id="mailruRecoverySend"','id="mailruRecoveryDownload"'):self.assertEqual(fresh.count(text),1)
        for unsafe in ('innerHTML','\\','`'):self.assertNotIn(unsafe,ri.MAILRU_COOKIE_JS+ri.MAILRU_COOKIE_HOOK)
        self.assertNotIn('cookiesBoth',fresh)
        import re
        mailru=re.search(r'<details id="mailruSettings".*?</details>',fresh,re.S).group()
        self.assertIn('id="mailruCookies"',mailru);self.assertEqual(mailru.count('<details'),1)
        self.assertNotIn('mailruCookieClientState',fresh);self.assertNotIn('mailruCookieServerState',fresh)
    def test_endpoint_auth_and_errors_do_not_reflect_browser_secrets(self):
        for path in ('/api/openflux/mailru/cookies','/api/openflux/mailru/recovery'):
            handler=Handler(path,{'document':DOC,'curl':'sensitive-fixture'});handler.authorized=lambda:False
            with patch.object(ri,'MailruRuntime') as runtime:ri.volga_panel_post(handler)
            runtime.assert_not_called();self.assertEqual(handler.sent[0],403)
            handler=Handler(path,{'document':DOC,'curl':'sensitive-fixture'})
            with patch.object(ri,'MailruRuntime',side_effect=ValueError('sensitive-fixture')):ri.volga_panel_post(handler)
            self.assertEqual(handler.sent[0],400);self.assertNotIn('sensitive-fixture',json.dumps(handler.sent))

if __name__=='__main__':unittest.main()
