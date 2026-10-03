"""Panel v6: the router's OPENFLUX events, protocol switch reasons, mode errors."""
import io,json,pathlib,tempfile,unittest
from unittest.mock import patch
import openflux_auth
import router_integration as r
from test_volga import BASE_PANEL

PC1_MODE='''            ok,out,rc=run(["/usr/bin/env","ROUTER_UPDATER_LOCK_HELD=1",*cmd],400 if target=="openflux" else 180);actual=current_mode()
            trigger_health();panel_system.invalidate_geo()
            if not ok or actual!=target:
                return self.j({"ok":False,"error":f"{actual.upper()} remains selected; startup failed. Use Restart/Rotate or select another mode.","output":out[-5000:]},500)
'''
PC2_MODE='''            ok,out,rc=run(["env","ROUTER_UPDATER_LOCK_HELD=1"]+cmd,340);actual=current_mode()
            if not ok or actual!=target:
                trigger_health();panel_system.invalidate_geo()
                return self.j({"ok":False,"error":f"{actual.upper()} remains selected. Use Restart/Rotate to retry.\\n{out[-2000:]}","output":out[-5000:]},500)
'''

class Handler:
    def __init__(self,path,body):
        data=json.dumps(body).encode();self.path=path;self.headers={'Content-Length':str(len(data)),'Content-Type':'application/json'};self.rfile=io.BytesIO(data);self.sent=None
    def allowed(self):return True
    def authorized(self):return True
    def j(self,value,status=200):self.sent=(status,value)

def mode_handler(body):
    """Run a patched /api/mode error branch and return the browser error."""
    source='def handler(self,run,current_mode,trigger_health,panel_system,cmd,target):\n    if True:\n'+body
    scope={};exec(compile(source,'panel','exec'),scope)
    class Self:
        def j(self,value,status=200):return value
    out='LAN mode: awg\nOPENFLUX was not started: AUTH_BLOCKED: Yandex asks Legacy for fresh browser cookies. AWG kept serving the LAN.\n'
    return scope['handler'](Self(),lambda cmd,timeout:(False,out,1),lambda:'awg',lambda:None,type('P',(),{'invalidate_geo':staticmethod(lambda:None)}),['net-openflux'],'openflux')['error']

class RouterEventsTest(unittest.TestCase):
    def test_v5_panel_gains_the_router_event_line(self):
        fresh=r.patch_panel(BASE_PANEL)
        self.assertIn('data-volga-panel="6"',fresh);self.assertEqual(fresh.count('id="openfluxRouterEvent"'),1)
        self.assertIn('of.router_event',fresh)
        v5=fresh.replace(r.VOLGA_HTML,r.VOLGA_HTML_V5).replace(r.VOLGA_HOOK,r.VOLGA_HOOK_V5)
        self.assertIn('data-volga-panel="5"',v5);self.assertNotIn('openfluxRouterEvent',v5)
        self.assertEqual(r.patch_panel(v5),fresh)
        self.assertEqual(r.patch_panel(fresh),fresh)
        # The panel embeds the script in a Python string: no backslashes or template literals.
        for unsafe in ('innerHTML','\\','`'):self.assertNotIn(unsafe,r.VOLGA_HOOK[len(r.VOLGA_HOOK_V5):])

    def test_both_mode_handlers_show_the_controller_reason(self):
        for name,body in (('pc1',PC1_MODE),('pc2',PC2_MODE)):
            with self.subTest(name):
                patched=r.patch_mode_api(body)
                self.assertNotEqual(patched,body);self.assertEqual(r.patch_mode_api(patched),patched)
                self.assertIn('600 if target=="openflux" else 180',patched)
                error=mode_handler(patched)
                self.assertTrue(error.startswith('OPENFLUX was not started; AWG is active.\n'),error)
                self.assertIn('AUTH_BLOCKED',error);self.assertNotIn('Use Restart/Rotate to retry',error)
        # The unified error branch is identical on both layouts.
        tail=lambda text:text.splitlines()[-1].strip()
        self.assertEqual(tail(r.patch_mode_api(PC1_MODE)),tail(r.patch_mode_api(PC2_MODE)))
        # An unknown layout stays untouched.
        self.assertEqual(r.patch_mode_api('x=1\n'),'x=1\n')

    def test_protocol_switch_shows_fixed_reasons_only(self):
        cases=(('AUTH_BLOCKED: import fresh browser cookies','Legacy: Yandex asks for fresh browser cookies'),
               ('Volga requires fresh browser cookies','Volga: Yandex asks for fresh browser cookies'),
               ('Volga did not pass session and exit-IP checks','Volga did not connect within 90 s'),
               ('OpenFlux health check failed; previous release will be restored','Legacy did not connect within 100 s'),
               ('Another router/Volga operation is running; retry shortly','Another router or Volga operation is running'),
               ('curl: (7) /etc/openflux-recovery/disk-token secret-ish output','Protocol switch failed'))
        for raised,expected in cases:
            with self.subTest(raised):
                handler=Handler('/api/openflux/protocol',{'protocol':'yandex'})
                with patch.object(r,'run_volga_cli',side_effect=RuntimeError(raised)),patch.object(r,'VolgaRuntime'):
                    self.assertTrue(r.volga_panel_post(handler))
                status,value=handler.sent
                self.assertEqual(status,400);self.assertIn(expected,value['error'])
                self.assertNotIn('/etc/',value['error']);self.assertNotIn('secret',value['error'])

    def test_router_event_is_sanitized(self):
        with tempfile.TemporaryDirectory() as tmp:
            path=pathlib.Path(tmp)/'openflux-last.json'
            with patch.object(openflux_auth,'ROUTER_EVENT',path):
                self.assertIsNone(openflux_auth.router_event())
                path.write_text('not json');self.assertIsNone(openflux_auth.router_event())
                path.write_text(json.dumps({'kind':'watchdog','ok':True,'at':1791014000,'message':'LAN\x1b[31m returned '+'x'*900,'to':'awg'}))
                event=openflux_auth.router_event()
                self.assertEqual(set(event),{'kind','ok','at','message'});self.assertEqual(event['kind'],'watchdog')
                self.assertNotIn('\x1b',event['message']);self.assertEqual(len(event['message']),600)
                path.write_text(json.dumps({'kind':'<script>','ok':'yes','at':1,'message':'m'}))
                event=openflux_auth.router_event();self.assertEqual(event['kind'],'event');self.assertFalse(event['ok'])
                path.write_text(json.dumps({'kind':'start','at':'1','message':'m'}));self.assertIsNone(openflux_auth.router_event())

if __name__=='__main__':unittest.main()
