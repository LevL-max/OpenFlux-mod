"""Panel v7: this mini-PC's last cookie check stays visible after its client stops."""
import json,os,pathlib,tempfile,time,types,unittest
from unittest.mock import patch
import openflux_auth as a
import router_integration as r
from test_volga import BASE_PANEL

T=1790000000  # a realistic clock: the first journal read starts SCAN_BACK earlier

def journal(*events):
    """journalctl -o json lines for (seconds, message) pairs."""
    return types.SimpleNamespace(stdout='\n'.join(json.dumps({'__REALTIME_TIMESTAMP':str(int((T+t)*1e6)),'MESSAGE':m}) for t,m in events),returncode=0)

def volga_line(t,event,data=None):
    stamp=time.strftime('%Y-%m-%dT%H:%M:%S',time.gmtime(t))+'.123456789Z'
    return json.dumps({'data':data,'event':event,'time':stamp})

class PanelTest(unittest.TestCase):
    def test_v6_panel_keeps_the_last_client_check_visible(self):
        fresh=r.patch_panel(BASE_PANEL)
        self.assertIn('data-volga-panel="7"',fresh);compile(fresh,'router-panel','exec')
        for piece in ('clientBadge(legacyAuth)','clientBadge(vp)',"checkText(vp.client_check,'Volga')","checkText(lc,'Legacy')","'this mini-PC: cookies refused'"):
            self.assertEqual(fresh.count(piece),1,piece)
        # The Legacy line no longer shows a refusal that a later success answered.
        self.assertNotIn("'Last client authentication issue: '+new Date(legacyAuth",fresh)
        v6=fresh.replace(r.VOLGA_HTML,r.VOLGA_HTML_V6).replace(r.VOLGA_HOOK,r.VOLGA_HOOK_V6).replace(r.LEGACY_CHECK_V7[1],r.LEGACY_CHECK_V7[0])
        self.assertIn('data-volga-panel="6"',v6);self.assertNotIn('client_check',v6)
        self.assertEqual(r.patch_panel(v6),fresh);self.assertEqual(r.patch_panel(fresh),fresh)
        # The panel embeds the script in a Python string: no backslashes or template literals.
        for part in (r.CLIENT_CHECK_JS,r.LEGACY_CHECK_V7[1],r.VOLGA_HOOK):
            for unsafe in ('innerHTML','\\','`'):self.assertNotIn(unsafe,part)

class LegacyCheckTest(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();root=pathlib.Path(self.tmp.name)
        self.store=root/'cookies.json';self.store.write_text('{}');os.utime(self.store,(1000,1000))
        self.patches=[patch.object(a,'CLIENT',True),patch.object(a,'NATIVE',True),patch.object(a,'STORE',self.store),
                      patch.object(a,'STATE',root/'state.json'),patch.object(a,'RECOVERY_DIR',root/'recovery'),patch.object(a,'runtime',return_value=(False,''))]
        for p in self.patches:p.start()
    def tearDown(self):
        for p in reversed(self.patches):p.stop()
        self.tmp.cleanup()
    def status(self,result,now):
        with patch.object(a,'command',return_value=result) as command,patch.object(a.time,'time',return_value=T+now):
            return a._status(),command
    def test_a_stopped_client_reports_its_last_refusal_until_new_cookies_or_a_success(self):
        out,command=self.status(journal((5000,'[YDOCS] CAPTCHA_REQUIRED: SmartCaptcha SECRET'),(5001,'[YDOCS] AUTH_BLOCKED: waiting for cookie store change')),10000)
        self.assertEqual(out['state'],'stopped')
        self.assertEqual(out['client_check'],{'result':'refused','at':T+5001,'cookies_saved_after':False})
        argv=command.call_args.args[0];self.assertIn('journalctl',argv);self.assertIn('@'+str(T+10000-a.SCAN_BACK),argv)
        self.assertNotIn('SECRET',json.dumps(out));self.assertNotIn('scanned_at',out)
        # Read again within SCAN_EVERY: the journal is not read twice.
        out,command=self.status(journal(),10000+a.SCAN_EVERY-1);command.assert_not_called()
        self.assertEqual(out['client_check']['result'],'refused')
        # Fresh cookies saved after the refusal wait for the next start.
        os.utime(self.store,(T+6000,T+6000))
        out,_=self.status(journal(),10000+a.SCAN_EVERY+1)
        self.assertEqual(out['client_check'],{'result':'refused','at':T+5001,'cookies_saved_after':True})
        # A later run connected: the refusal is history.
        out,command=self.status(journal((7000,'OnlyOffice authentication successful')),20000)
        self.assertEqual(out['client_check'],{'result':'ok','at':T+7000})
        self.assertIn('@'+str(T+10000+a.SCAN_EVERY),command.call_args.args[0])
    def test_store_written_by_the_refused_run_itself_is_not_new_cookies(self):
        os.utime(self.store,(T+5000.5,T+5000.5))  # a first-tier captcha just before the refusal
        out,_=self.status(journal((5001,'AUTH_BLOCKED: waiting')),10000)
        self.assertFalse(out['client_check']['cookies_saved_after'])
    def test_a_handshake_failure_is_not_a_cookie_refusal(self):
        out,_=self.status(journal((5000,'OnlyOffice authentication successful'),(6000,'AUTH_REJECTED: document authentication rejected')),10000)
        self.assertEqual(out['client_check'],{'result':'ok','at':T+5000})
    def test_a_running_client_records_its_success(self):
        with patch.object(a,'runtime',return_value=(True,'b'*32)):
            out,_=self.status(journal((5000,'AUTH_BLOCKED: waiting'),(6000,'AUTH_BLOCKED cleared'),(6001,'OnlyOffice authentication successful')),10000)
        self.assertEqual(out['state'],'connected');self.assertEqual(out['client_check'],{'result':'ok','at':T+6001})

class VolgaCheckTest(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.root=pathlib.Path(self.tmp.name)
        node=self.root/'etc/openflux/node.json';node.parent.mkdir(parents=True);node.write_text(json.dumps({'role':'client'}))
        binary=self.root/r.VOLGA_BINARY.lstrip('/');binary.parent.mkdir(parents=True);binary.write_text('x')
        self.data=self.root/r.VOLGA_DATA.lstrip('/');self.data.mkdir(parents=True)
        for name in ('cookies.json','browser.json'):(self.data/name).write_text('{}');os.utime(self.data/name,(1000,1000))
        self.runtime=r.VolgaRuntime(str(self.root))
    def tearDown(self):self.tmp.cleanup()
    def test_a_stopped_client_shows_the_refusal_of_its_last_run(self):
        lines='\n'.join([volga_line(5000,'carrier_started'),volga_line(6000,'auth_blocked')])
        calls=[]
        def run(argv,check=True,timeout=30):calls.append(argv);return types.SimpleNamespace(stdout=lines,stderr='',returncode=0)
        with patch.object(r.VolgaRuntime,'active',return_value=False),patch.object(r.VolgaRuntime,'run',side_effect=run):
            out=self.runtime.status()
        self.assertEqual(out['state'],'stopped')
        check=out['client_check'];self.assertEqual((check['result'],check['cookies_saved_after']),('refused',False));self.assertAlmostEqual(check['at'],6000.123456,places=3)
        self.assertIn('-u',calls[0]);self.assertIn(r.VOLGA_UNIT,calls[0])
        # The panel saves fresh cookies for a document.
        os.utime(self.data/'browser.json',(7000,7000))
        with patch.object(r.VolgaRuntime,'active',return_value=False),patch.object(r.VolgaRuntime,'run',side_effect=run),patch('time.time',return_value=time.time()+60):
            out=self.runtime.status()
        self.assertTrue(out['client_check']['cookies_saved_after'])
    def test_a_running_client_records_a_session(self):
        lines='\n'.join([volga_line(5000,'auth_blocked'),volga_line(8000,'status',{'auth_blocked':False}),volga_line(8001,'session_ready')])
        def run(argv,check=True,timeout=30):
            stdout='a'*32 if argv[:2]==['systemctl','show'] else lines
            return types.SimpleNamespace(stdout=stdout,stderr='',returncode=0)
        with patch.object(r.VolgaRuntime,'active',return_value=True),patch.object(r.VolgaRuntime,'run',side_effect=run):
            out=self.runtime.status()
        self.assertEqual(out['state'],'connected');self.assertEqual(out['client_check']['result'],'ok')
        self.assertTrue((self.root/'var/lib/openflux-volga-updater/client-check.json').is_file())
    def test_a_server_has_no_client_check(self):
        (self.root/'etc/openflux/node.json').write_text(json.dumps({'role':'server'}))
        with patch.object(r.VolgaRuntime,'active',return_value=False):
            self.assertNotIn('client_check',self.runtime.status())

if __name__=='__main__':unittest.main()
