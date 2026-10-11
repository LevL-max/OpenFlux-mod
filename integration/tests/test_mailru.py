"""Mail.ru, the third protocol (v4.2.0): its link, client unit and server exit,
status, protocol switching, router helpers, panel v8 and configuration card."""
import io,json,pathlib,re,tempfile,time,types,unittest
from unittest.mock import patch
import openflux_auth
import router_integration as ri
from router_integration import (MailruRuntime,VolgaRuntime,MAILRU_UNIT,MAILRU_EXIT_UNIT,VOLGA_UNIT,
    mailru_document,mailru_env,mailru_exit_settings,patch_runtime_selector)
from test_volga import BASE_PANEL

DOC='https://cloud.mail.ru/public/AbCd/EfGh1234'
LEGACY='openflux-yandex-client.service'

def result(rc=0,out='',err=''):return types.SimpleNamespace(returncode=rc,stdout=out,stderr=err)

class FakeHost:
    """systemd, journald and docker as the Mail.ru runtime sees them."""
    def __init__(self):
        self.active=set();self.calls=[];self.journal=[];self.invocation='a'*32;self.help='  -mailru-editor-activity'
        self.legacy_exit={'Config':{'Image':'openflux-runtime:24.04'},'NetworkSettings':{'Networks':{'openflux-net':{}}}}
    def run(self,argv,check=True,timeout=30):
        self.calls.append(argv)
        if argv[-1]=='--help':return result(out=self.help)
        if argv[:2]==['systemctl','is-active']:return result(0 if argv[-1] in self.active else 3)
        if argv[:2]==['systemctl','show']:
            state='active' if argv[2] in self.active else 'inactive'
            return result(out='ActiveState='+state+'\nInvocationID='+(self.invocation if state=='active' else '')+'\n')
        if argv[:2]==['systemctl','start'] or argv[:2]==['systemctl','restart']:self.active.add(argv[2])
        if argv[:2]==['systemctl','stop']:self.active.discard(argv[2])
        if argv[0]=='journalctl':
            since=float(argv[argv.index('--since')+1][1:]) if '--since' in argv else 0
            return result(out='\n'.join(json.dumps({'__REALTIME_TIMESTAMP':str(int(t*1e6)),'MESSAGE':m}) for t,m in self.journal if t>=since))
        if argv[:2]==['docker','inspect']:return result(out=json.dumps([self.legacy_exit])) if self.legacy_exit else result(1)
        return result()

class Base(unittest.TestCase):
    role='client'
    def setUp(self):
        self.temp=tempfile.TemporaryDirectory();self.addCleanup(self.temp.cleanup);self.root=pathlib.Path(self.temp.name)
        self.runtime=MailruRuntime(self.root);self.host=FakeHost()
        self.node={'role':self.role,'binary':'/opt/openflux-client/bin/openflux','service':LEGACY,'container':'openflux-yandex-exit',
                   'cookie_user':'openflux','library':'/usr/local/lib/router-updater','active_transport':'yandex','router_updater':False}
        self.runtime.save(self.runtime.node_path,self.node)
        for target,name in ((MailruRuntime,'run'),(VolgaRuntime,'run')):
            p=patch.object(target,name,side_effect=self.host.run);p.start();self.addCleanup(p.stop)
        self.hooks=patch.object(VolgaRuntime,'install_router_hooks',return_value=[]).start();self.addCleanup(patch.stopall)
    def unit(self):return self.runtime.units/self.runtime.unit()

class LinkTests(unittest.TestCase):
    def test_public_document_links_only(self):
        self.assertEqual(mailru_document(DOC+'/'),DOC);self.assertEqual(mailru_document(DOC),DOC)
        for bad in ['http://cloud.mail.ru/public/AbCd/EfGh','https://cloud.mail.ru.evil.example/public/AbCd/EfGh','https://cloud.mail.ru/public/AbCd',
                    'https://cloud.mail.ru/public/AbCd/EfGh/file.docx','https://cloud.mail.ru/public/AbCd/EfGh?x=1','https://cloud.mail.ru/public/AbCd/EfGh#x',
                    'https://user@cloud.mail.ru/public/AbCd/EfGh','https://cloud.mail.ru:8443/public/AbCd/EfGh','https://cloud.mail.ru/public/AbCd/Ef%20Gh',
                    DOC+' ',None,'https://cloud.mail.ru/home/AbCd/EfGh']:
            with self.subTest(bad=bad),self.assertRaises(ValueError):mailru_document(bad)
    def test_env_file_holds_one_link(self):
        self.assertEqual(mailru_env('# comment\nMAILRU_DOCUMENT='+DOC+'\n'),DOC)
        for bad in ['','MAILRU_DOCUMENT='+DOC+'\nMAILRU_DOCUMENT='+DOC,'MAILRU_DOCUMENT='+DOC+'\nOTHER=1','DOC='+DOC,'MAILRU_DOCUMENT="'+DOC+'"']:
            with self.subTest(bad=bad),self.assertRaises(ValueError):mailru_env(bad)
    def test_exit_settings_refuse_host_networking(self):
        good={'image':'openflux-runtime:24.04','network':'openflux-net'};self.assertEqual(mailru_exit_settings(good),good)
        for bad in [dict(good,network='host'),dict(good,network='none'),dict(good,network='container:x'),dict(good,image='Bad Image'),{'image':'x'},None]:
            with self.subTest(bad=bad),self.assertRaises(ValueError):mailru_exit_settings(bad)

class ClientTests(Base):
    def test_client_unit_runs_the_legacy_binary_beside_the_other_protocols(self):
        text=self.runtime.unit_text()
        self.assertIn('Conflicts='+LEGACY+' '+VOLGA_UNIT+'\n',text);self.assertIn('PartOf=sing-box-openflux.service',text)
        self.assertIn('User=openflux\n',text);self.assertIn('EnvironmentFile=/etc/openflux-mailru/mailru.env\n',text)
        self.assertIn('ExecStart=/opt/openflux-client/bin/openflux --client --transport mailru --url ${MAILRU_DOCUMENT} --socks5 127.0.0.1:11080 --compression=true --yandex-queue-size=1024 '
                      '--tcp-buffer-default=1048576 --tcp-buffer-max=4194304 --batch-packets=4 --batch-delay-us=1000\n',text)
        for field in ('NoNewPrivileges=yes','ProtectSystem=strict','CapabilityBoundingSet=\n','PrivateDevices=yes'):self.assertIn(field,text)
        self.assertNotIn('--debug',text);self.assertNotIn('--mailru-editor-activity',text)
        with patch.object(self.runtime,'node',return_value=dict(self.node,binary='relative/openflux')),self.assertRaises(ValueError):self.runtime.unit_text()

    def test_setup_saves_the_link_and_unit_and_never_starts_the_client(self):
        result=self.runtime.configure(DOC+'/')
        self.assertEqual(self.runtime.env_path.read_text(),'MAILRU_DOCUMENT='+DOC+'\n');self.assertEqual(self.runtime.document(),DOC)
        self.assertEqual(self.unit().read_text(),self.runtime.unit_text())
        self.assertIn(['systemctl','daemon-reload'],self.host.calls);self.hooks.assert_called_once()
        self.assertFalse(any(c[:2] in (['systemctl','start'],['systemctl','restart'],['systemctl','enable']) for c in self.host.calls))
        self.assertIn('Select Mail.ru',result['message']);self.assertTrue(self.runtime.installed())

    def test_setup_refuses_a_binary_without_mailru_and_leaves_nothing(self):
        self.host.help='  -yandex-cookie-store'
        with self.assertRaisesRegex(ValueError,'no Mail.ru transport'):self.runtime.configure(DOC)
        self.assertFalse(self.runtime.env_path.exists());self.assertFalse(self.unit().exists())

    def test_failed_router_helpers_restore_the_previous_link(self):
        self.runtime.configure(DOC);other='https://cloud.mail.ru/public/Zzzz/Yyyy'
        self.hooks.side_effect=ValueError('Unknown router shell layout')
        with self.assertRaisesRegex(ValueError,'Unknown router'):self.runtime.configure(other)
        self.assertEqual(self.runtime.document(),DOC);self.assertEqual(self.unit().read_text(),self.runtime.unit_text())

    def test_a_new_link_for_a_running_client_asks_to_apply_it_again(self):
        self.runtime.configure(DOC);self.host.active.add(MAILRU_UNIT)
        result=self.runtime.configure('https://cloud.mail.ru/public/Zzzz/Yyyy')
        self.assertIn('Apply the Mail.ru protocol again',result['message']);self.assertIn(MAILRU_UNIT,self.host.active)

    def test_status_follows_the_newest_line_and_reads_only_new_ones(self):
        self.runtime.configure(DOC)
        stopped=self.runtime.status()
        self.assertEqual((stopped['state'],stopped['active'],stopped['configured'],stopped['documents']),('stopped',False,True,[DOC]))
        self.host.active.add(MAILRU_UNIT);now=time.time()
        self.host.journal=[(now-10,'[BATCH] tx_pkt=0'),(now-9,'2026/10/11 [M-DOCS] connected to the document')]
        s=self.runtime.status();self.assertEqual((s['state'],s['connected_at']),('connected',int(now-9)))
        self.host.journal.append((now+5,'[M-DOCS] connection to the document dropped: websocket: close 1006; reconnecting'))
        self.host.journal.append((now+6,'[M-DOCS] cannot open the document: API returned status 404; retrying'))
        s=self.runtime.status();self.assertEqual(s['state'],'connecting');self.assertIn('(HTTP 404)',s['reason'])
        journal=[c for c in self.host.calls if c[0]=='journalctl']
        self.assertNotIn('--since',journal[0]);self.assertIn('--since',journal[1])
        # A new run starts from scratch: no state of the previous run survives.
        self.host.invocation='b'*32;self.host.journal=[]
        s=self.runtime.status();self.assertEqual(s['state'],'connecting');self.assertNotIn('connected_at',s)
        self.assertNotIn('--since',[c for c in self.host.calls if c[0]=='journalctl'][-1])

    def test_health_needs_the_document_and_the_exit(self):
        self.runtime.configure(DOC);self.host.active.add(MAILRU_UNIT);self.host.journal=[(1.0,'[M-DOCS] connected to the document')]
        answers=iter([False,True])
        with patch.object(VolgaRuntime,'exit_ok',side_effect=lambda:next(answers)),patch('time.sleep'):self.runtime.health(30)
        self.host.active.clear()
        with self.assertRaisesRegex(RuntimeError,'stopped during health check'):self.runtime.health(30)

class ServerTests(Base):
    role='server'
    def test_exit_runs_the_legacy_binary_in_an_isolated_container(self):
        self.runtime.save(self.runtime.node_path,dict(self.node,binary='/opt/openflux/bin/openflux',mailru_exit={'image':'openflux-runtime:24.04','network':'openflux-net'}))
        text=self.runtime.exit_unit_text();start=[l for l in text.splitlines() if l.startswith('ExecStart=')][0]
        for flag in ('--rm','--pull=never','--name openflux-mailru','--network=openflux-net','--cap-drop=ALL','--cap-add=NET_RAW','--cap-add=NET_ADMIN',
                     '--security-opt=no-new-privileges','--memory=512m','--log-opt=max-size=10m','src=/opt/openflux/bin/openflux,dst=/usr/local/bin/openflux,readonly',
                     ' openflux-runtime:24.04 /run/openflux-mailru/run.sh --exit-node --transport mailru --url ${MAILRU_DOCUMENT} --compression=true'):
            self.assertIn(flag,start)
        for forbidden in ('--network=host','--privileged','--publish','-p ','SYS_ADMIN','--client','--socks5'):self.assertNotIn(forbidden,start)
        self.assertIn('SuccessExitStatus=2 143',text);self.assertIn('Restart=always',text);self.assertIn('ExecStop=/usr/bin/docker stop',text)

    def test_setup_takes_the_legacy_exit_image_and_network_and_must_connect(self):
        self.host.journal=[(1.0,'[M-DOCS] connected to the document')]
        result=self.runtime.configure(DOC)
        node=self.runtime.node();self.assertEqual(node['mailru_exit'],{'image':'openflux-runtime:24.04','network':'openflux-net'})
        self.assertEqual((self.runtime.data/'run.sh').read_text(),ri.MAILRU_EXIT_SCRIPT);self.assertIn('RST RST -j DROP',ri.MAILRU_EXIT_SCRIPT)
        self.assertIn(['systemctl','enable',MAILRU_EXIT_UNIT],self.host.calls);self.assertIn(['systemctl','restart',MAILRU_EXIT_UNIT],self.host.calls)
        self.assertIn('connected',result['message']);self.assertIn(MAILRU_EXIT_UNIT,self.host.active)

    def test_failed_first_setup_removes_the_exit_again(self):
        with patch.object(MailruRuntime,'health',side_effect=RuntimeError('Mail.ru did not pass connection and exit-IP checks')):
            with self.assertRaisesRegex(RuntimeError,'did not pass'):self.runtime.configure(DOC)
        self.assertEqual(self.runtime.node(),self.node)
        for path in (self.runtime.env_path,self.unit(),self.runtime.data/'run.sh'):self.assertFalse(path.exists(),str(path))
        self.assertIn(['systemctl','disable',MAILRU_EXIT_UNIT],self.host.calls);self.assertNotIn(MAILRU_EXIT_UNIT,self.host.active)

    def test_failed_link_change_keeps_the_running_exit_on_the_old_link(self):
        self.host.journal=[(1.0,'[M-DOCS] connected to the document')];self.runtime.configure(DOC)
        with patch.object(MailruRuntime,'health',side_effect=RuntimeError('Mail.ru did not pass connection and exit-IP checks')):
            with self.assertRaises(RuntimeError):self.runtime.configure('https://cloud.mail.ru/public/Zzzz/Yyyy')
        self.assertEqual(self.runtime.document(),DOC);self.assertIn(MAILRU_EXIT_UNIT,self.host.active)
        self.assertNotIn(['systemctl','disable',MAILRU_EXIT_UNIT],self.host.calls)

    def test_without_a_usable_legacy_exit_the_settings_are_required(self):
        self.host.legacy_exit['NetworkSettings']['Networks']={'host':{}}
        with self.assertRaisesRegex(ValueError,'image and a network'):self.runtime.configure(DOC)
        self.assertFalse(self.runtime.env_path.exists())

class SwitchTests(Base):
    def setUp(self):
        super().setUp();self.volga=VolgaRuntime(self.root);self.volga.state.mkdir(parents=True)
        self.host.active.add(LEGACY);self.runtime.configure(DOC)
        p=patch.object(MailruRuntime,'health');self.health=p.start()
        p=patch('openflux_node.health');self.legacy_health=p.start()
        start=self.host.run
        def run(argv,check=True,timeout=30):
            # The protocol units conflict: starting one stops the others.
            if argv[:2] in (['systemctl','start'],['systemctl','restart']) and argv[2] in (LEGACY,VOLGA_UNIT,MAILRU_UNIT):
                self.host.active-={LEGACY,VOLGA_UNIT,MAILRU_UNIT}
            return start(argv,check,timeout)
        self.host.run=run
        for target in (MailruRuntime,VolgaRuntime):patch.object(target,'run',side_effect=run).start()

    def test_mailru_is_selected_and_checked_beside_legacy(self):
        result=self.volga.select('mailru')
        self.assertEqual(self.volga.node()['active_transport'],'mailru');self.assertEqual(self.host.active,{MAILRU_UNIT})
        self.health.assert_called_once();self.assertIn('passed',result['message'])
        self.assertFalse((self.volga.state/'transaction.json').exists())
        # And back: Legacy returns, Mail.ru stops.
        self.volga.select('yandex');self.assertEqual(self.host.active,{LEGACY});self.assertEqual(self.volga.node()['active_transport'],'yandex')

    def test_a_failed_check_restores_legacy_and_the_selection(self):
        self.health.side_effect=RuntimeError('Mail.ru did not pass connection and exit-IP checks')
        with self.assertRaisesRegex(RuntimeError,'did not pass'):self.volga.select('mailru')
        self.assertEqual(self.volga.node()['active_transport'],'yandex');self.assertEqual(self.host.active,{LEGACY})
        self.assertFalse((self.volga.state/'transaction.json').exists())

    def test_in_another_router_mode_the_check_leaves_nothing_running(self):
        self.host.active.clear();mode=self.volga.path('/etc/router/mode');mode.parent.mkdir(parents=True);mode.write_text('awg\n')
        self.volga.select('mailru')
        self.assertEqual(self.host.active,set());self.assertEqual(self.volga.node()['active_transport'],'mailru')

    def test_mailru_must_be_set_up_first(self):
        self.runtime.env_path.unlink()
        with self.assertRaisesRegex(ValueError,'Set up Mail.ru first'):self.volga.select('mailru')
        self.assertEqual(self.host.active,{LEGACY})
        self.assertIn('save the Mail.ru document link',ri.protocol_error(ValueError('Set up Mail.ru first')))

    def test_runtime_unit_follows_the_selection(self):
        for selected,unit in (('mailru',MAILRU_UNIT),('volga',VOLGA_UNIT),('yandex',LEGACY)):
            with patch('pathlib.Path.read_text',return_value=json.dumps(dict(self.node,active_transport=selected))):
                self.assertEqual(ri.openflux_runtime_unit(),unit)

class RouterHelperTests(unittest.TestCase):
    V1='#!/usr/bin/env bash\nset -Eeuo pipefail\n# openflux-protocol-selector-v1\nOPENFLUX_SELECTED_UNIT="$(/usr/local/sbin/openfluxctl runtime-unit 2>/dev/null || echo openflux-yandex-client.service)"\n'+ri.SELECTOR_CASE_V1+'UNIT="$OPENFLUX_SELECTED_UNIT"\n'
    def test_helpers_patched_before_accept_the_mailru_unit(self):
        changed=patch_runtime_selector(self.V1)
        self.assertIn('|openflux-volga-client.service|'+MAILRU_UNIT+')',changed);self.assertEqual(changed.count('case "$OPENFLUX_SELECTED_UNIT"'),1)
        self.assertEqual(patch_runtime_selector(changed),changed);self.assertEqual(changed.replace(ri.SELECTOR_CASE,ri.SELECTOR_CASE_V1),self.V1)
        fresh=patch_runtime_selector('#!/usr/bin/env bash\nset -Eeuo pipefail\nUNIT="openflux-yandex-client.service"\n')
        self.assertIn(ri.SELECTOR_CASE,fresh);self.assertNotIn(ri.SELECTOR_CASE_V1,fresh)
    def test_router_health_names_mailru(self):
        v1="# openflux-protocol-selector-v1\nUNIT='x'\n"+ri.HEALTH_NAMES_V1+"def active():pass\n"
        changed=patch_runtime_selector(v1,python=True);compile(changed,'health','exec');self.assertIn("'Mail.ru'",changed)
        scope={'UNIT':MAILRU_UNIT};exec(ri.HEALTH_NAMES,scope);self.assertEqual(scope['protocol'](),'Mail.ru')
        scope['UNIT']=VOLGA_UNIT;self.assertEqual(scope['protocol'](),'Volga');scope['UNIT']=LEGACY;self.assertEqual(scope['protocol'](),'Legacy')
        self.assertEqual(patch_runtime_selector(changed,python=True),changed)
        fresh=patch_runtime_selector("UNIT='openflux-yandex-client.service'\n"+ri.HEALTH_NAMES_V1,python=True)
        self.assertIn(ri.HEALTH_NAMES,fresh);compile(fresh,'health','exec')

class Handler:
    def __init__(self,path,body):
        data=json.dumps(body).encode();self.path=path;self.headers={'Content-Length':str(len(data)),'Content-Type':'application/json'};self.rfile=io.BytesIO(data);self.sent=None
    def allowed(self):return True
    def authorized(self):return True
    def j(self,value,status=200):self.sent=(status,value)

class PanelTests(unittest.TestCase):
    def test_fresh_panel_offers_mailru_with_a_section_of_its_own(self):
        changed=ri.patch_panel(BASE_PANEL);compile(changed,'router-panel','exec')
        self.assertEqual(ri.patch_panel(changed),changed)
        for piece in ('<option value="mailru">Mail.ru</option>','id="mailruSettings"','id="mailruDocument"','id="mailruSave"','id="mailruConfigDownload"',
                      'id="mailruConfigUpload"',"$('#mailruConfigDownload').disabled=!mp.configured;",
                      "of.protocols?.mailru","selected!=='mailru'&&refused(","<b>Volga and Mail.ru: one mini-PC at a time.</b>"):
            self.assertEqual(changed.count(piece),1,piece)
        ids=re.findall(r'\bid="([^"]+)"',changed);self.assertEqual(len(ids),len(set(ids)))
        # The panel embeds the script in a Python string: no backslashes or template literals.
        for part in (ri.MAILRU_JS,ri.MAILRU_HOOK,ri.MAILRU_HTML):
            for unsafe in ('innerHTML','\\','`'):self.assertNotIn(unsafe,part)

    def test_v7_panel_migrates_to_v8(self):
        fresh=ri.patch_panel(BASE_PANEL)
        v7=(fresh.replace(ri.VOLGA_HTML,ri.VOLGA_HTML_V7).replace(ri.VOLGA_JS,ri.VOLGA_JS_V7).replace(ri.VOLGA_HOOK,ri.VOLGA_HOOK_V7)
            .replace(",'openflux-mailru'],modes",'],modes').replace(",'openflux-mailru']);",']);'))
        self.assertIn('data-volga-panel="7"',v7);self.assertNotIn('mailru',v7)
        self.assertEqual(ri.patch_panel(v7),fresh)
        with self.assertRaises(ValueError):ri.patch_panel(v7.replace(ri.VOLGA_JS_V7,ri.VOLGA_JS_V7.replace('cookieLinks','links')))

    def test_saving_the_link_answers_with_fixed_texts_only(self):
        handler=Handler('/api/openflux/mailru/config',{'document':DOC})
        with patch.object(ri,'run_mailru_cli',return_value={'ok':True,'message':'saved'}) as cli:
            self.assertTrue(ri.volga_panel_post(handler))
        self.assertEqual(handler.sent,(200,{'ok':True,'message':'saved'}));self.assertEqual(cli.call_args.args[0].document_url,DOC)
        for error,expected in [(ValueError('Expected a public https://cloud.mail.ru/public/… document link'),'Paste the public link'),
                               (ValueError('The installed OpenFlux binary has no Mail.ru transport; update OpenFlux first'),'Router Updater'),
                               (ValueError('Another router/Volga operation is running; retry shortly'),ri.BUSY_ERROR),
                               (RuntimeError('/etc/secret <script>'),'Mail.ru setup failed')]:
            handler=Handler('/api/openflux/mailru/config',{'document':'x'})
            with patch.object(ri,'run_mailru_cli',side_effect=error):ri.volga_panel_post(handler)
            self.assertEqual(handler.sent[0],400);self.assertIn(expected,handler.sent[1]['error']);self.assertNotIn('secret',handler.sent[1]['error'])

    def test_config_file_is_downloaded_and_uploaded_beside_the_link(self):
        handler=Handler('/api/openflux/mailru/config',{'operation':'download'})
        with patch.object(ri,'MailruRuntime',return_value=types.SimpleNamespace(document=lambda:DOC)):ri.volga_panel_post(handler)
        self.assertEqual(handler.sent,(200,{'ok':True,'config':'MAILRU_DOCUMENT='+DOC+'\n','filename':'mailru.env'}))
        handler=Handler('/api/openflux/mailru/config',{'operation':'download'})
        with patch.object(ri,'MailruRuntime',return_value=types.SimpleNamespace(document=lambda:None)):ri.volga_panel_post(handler)
        self.assertEqual(handler.sent[0],400);self.assertIn('save its document link first',handler.sent[1]['error'])
        # An uploaded file sets Mail.ru up exactly like the link field.
        handler=Handler('/api/openflux/mailru/config',{'operation':'upload','config':'# copied\nMAILRU_DOCUMENT='+DOC+'/\n'})
        with patch.object(ri,'run_mailru_cli',return_value={'ok':True,'message':'saved'}) as cli:ri.volga_panel_post(handler)
        self.assertEqual(handler.sent[0],200);self.assertEqual(cli.call_args.args[0].document_url,DOC)
        for config in ('YANDEX_URL=https://disk.yandex.ru/i/x\n',None,'MAILRU_DOCUMENT='+DOC+'\n'*5000,'MAILRU_DOCUMENT=https://evil.example/x\n'):
            handler=Handler('/api/openflux/mailru/config',{'operation':'upload','config':config})
            with patch.object(ri,'run_mailru_cli') as cli:ri.volga_panel_post(handler)
            cli.assert_not_called();self.assertEqual(handler.sent[0],400)
            self.assertTrue(handler.sent[1]['error'].startswith(('Upload a Mail.ru config file','Paste the public link')),handler.sent[1]['error'])
        handler=Handler('/api/openflux/mailru/config',{'operation':'delete'})
        with patch.object(ri,'run_mailru_cli') as cli:ri.volga_panel_post(handler)
        cli.assert_not_called();self.assertEqual(handler.sent[0],400)

    def test_mailru_switch_failures_say_why(self):
        for text,expected in [('Mail.ru did not pass connection and exit-IP checks','within 90 s'),('Mail.ru stopped during health check','journalctl -u openflux-mailru-client')]:
            self.assertIn(expected,ri.protocol_error(RuntimeError(text)))

    def test_status_shows_the_selected_protocol_and_all_three(self):
        volga=types.SimpleNamespace(status=lambda:{'state':'stopped'},node=lambda:{'active_transport':'mailru'})
        mailru=types.SimpleNamespace(status=lambda:{'state':'connected','transport':'mailru','documents':[DOC]})
        with patch.object(openflux_auth,'note_viewer'),patch.object(openflux_auth,'status',return_value={'state':'stopped','reason':'OpenFlux is stopped.'}),\
             patch.object(openflux_auth,'current_document_url',return_value=None),patch.object(openflux_auth,'router_event',return_value=None),\
             patch.object(ri,'VolgaRuntime',return_value=volga),patch.object(ri,'MailruRuntime',return_value=mailru):
            snap=openflux_auth.snapshot()
        self.assertEqual((snap['state'],snap['active_transport']),('connected','mailru'));self.assertEqual(set(snap['protocols']),{'yandex','volga','mailru'})
        with patch.object(openflux_auth,'note_viewer'),patch.object(openflux_auth,'status',return_value={'state':'connected'}),\
             patch.object(openflux_auth,'current_document_url',return_value=None),patch.object(openflux_auth,'router_event',return_value=None),\
             patch.object(ri,'VolgaRuntime',return_value=types.SimpleNamespace(status=lambda:{'state':'stopped'},node=lambda:{})),\
             patch.object(ri,'MailruRuntime',side_effect=ValueError('no node')):
            snap=openflux_auth.snapshot()
        self.assertEqual(snap['state'],'connected');self.assertEqual(snap['protocols']['mailru']['state'],'unknown')

class ConfigCardTests(unittest.TestCase):
    def test_link_file_is_validated_and_normalized(self):
        temp=tempfile.TemporaryDirectory();self.addCleanup(temp.cleanup);root=pathlib.Path(temp.name)
        env=root/'etc/openflux-mailru/mailru.env';env.parent.mkdir(parents=True);env.write_text('MAILRU_DOCUMENT='+DOC+'\n')
        core=types.SimpleNamespace(registry=lambda:{},upload=lambda body:None,parse=lambda data:data.decode())
        configs=ri.OpenFluxConfigs(core,root);row=configs.registry()['openflux-mailru']
        self.assertEqual(row['path'],str(env));self.assertIn('no cookies',row['description'])
        writes=configs.plan('openflux-mailru',b'# new\nMAILRU_DOCUMENT=https://cloud.mail.ru/public/Zzzz/Yyyy/\n',lambda path:pathlib.Path(path).read_bytes())
        self.assertEqual(writes,{env:b'MAILRU_DOCUMENT=https://cloud.mail.ru/public/Zzzz/Yyyy\n'})
        with self.assertRaises(ValueError):configs.plan('openflux-mailru',b'MAILRU_DOCUMENT=https://evil.example/x\n',lambda path:b'')

if __name__=='__main__':unittest.main()
