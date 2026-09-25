#!/usr/bin/env python3
"""Local user interface for SSH cookie refresh and encrypted offline AWS recovery."""
import json,os,pathlib,secrets,shlex,sys,threading,time,webbrowser
from http.server import ThreadingHTTPServer,BaseHTTPRequestHandler
ROOT=pathlib.Path(os.environ['LOCALAPPDATA'])/'OpenFluxRecovery'
sys.path.insert(0,str(ROOT/'lib'))
import paramiko
from cryptography.hazmat.primitives import serialization
import recovery_crypto,windows_secrets,cookie_import

CONFIG=json.loads((ROOT/'config.json').read_text())
TOKEN=secrets.token_urlsafe(32)
HTML='''<!doctype html><html lang="ru"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>OpenFlux Recovery</title>
<style>body{margin:0;background:#101922;color:#e8edf2;font:16px system-ui}main{max-width:900px;margin:40px auto;padding:0 22px}h1{font-size:32px;margin:8px 0}h2{font-size:19px;margin:0 0 12px}.muted{color:#abb9c6;line-height:1.6}.box{background:#192632;border:1px solid #344552;border-radius:16px;padding:22px;margin:20px 0}label{display:block;margin:16px 0 7px}select,input,textarea,button{font:inherit;border-radius:9px;border:1px solid #526577;padding:11px;box-sizing:border-box}select,input,textarea{background:#101922;color:#fff;width:100%}textarea{resize:vertical;min-height:155px}button{cursor:pointer;background:#63d4b0;color:#10251e;border:0;font-weight:600}button.secondary{background:#304858;color:#fff}button:disabled{opacity:.45;cursor:wait}.row{display:flex;gap:12px;flex-wrap:wrap;margin:16px 0}.state{font-size:22px;font-weight:650}.msg{white-space:pre-wrap;line-height:1.55}a{color:#83dabc}.step{color:#63d4b0;text-transform:uppercase;letter-spacing:.12em;font-size:12px}.hidden{display:none}small{line-height:1.5}#result{padding:16px 0}footer{font-size:13px;margin:25px 0;color:#9aacba}</style>
<main><div class="step">OpenFlux · Восстановление подключения</div><h1>Обновить cookies Яндекса</h1><p class="muted">Выберите устройство и вставьте свежие cookies из браузера. Переустановка OpenFlux и перезапуск сервиса не нужны.</p>
<section class="box"><h2>1. Устройство</h2><select id="target"><option value="aws">AWS — сервер</option><option value="mini1">Mini-PC1</option><option value="mini2">Mini-PC2</option></select><div id="passwordRow" class="hidden"><label for="password">Пароль ubuntu</label><input id="password" type="password" autocomplete="off"><small class="muted">Используется только для текущего запроса и не сохраняется.</small></div><div class="row"><button id="statusButton" class="secondary">Проверить состояние</button></div><div id="state" class="state">Состояние ещё не проверено</div><p id="reason" class="muted"></p><p id="lastFailure" class="muted"></p><p id="inbox" class="muted"></p></section>
<section class="box"><h2>2. Свежие cookies</h2><ol class="muted"><li>Откройте свой публичный документ Яндекса и пройдите проверку, если она появится.</li><li>Нажмите F12 → Network / Сеть, затем обновите страницу.</li><li>Нажмите правой кнопкой на запрос документа → Copy → Copy as cURL (bash).</li></ol><label for="curl">Вставьте скопированную команду</label><textarea id="curl" autocomplete="off" spellcheck="false" placeholder="curl 'https://…' …"></textarea><div class="row"><button id="direct">Обновить по SSH</button><button id="package" class="secondary">Создать файл для AWS</button></div><div id="result" class="msg" role="status" aria-live="polite"></div></section>
<section class="box" id="diskBox"><h2>Если AWS недоступен из-за белого списка</h2><p class="muted">Создайте файл для AWS, откройте папку с файлом и загрузите <b>aws-recovery.json</b> в папку <b>OpenFlux Recovery</b> на своём Диске. При загрузке выберите замену старого файла. AWS проверяет Диск примерно раз в минуту через отдельный API-доступ.</p><div class="row"><button id="openOutbox" class="secondary">Открыть папку с файлом</button><a href="https://disk.yandex.com/client/disk" target="_blank" rel="noreferrer">Открыть мой Яндекс Диск ↗</a></div><small class="muted">Пакет зашифрован для вашего AWS, подписан и действует один час. Если API Диска недоступен или его доступ истёк, доставка потребует восстановления этого канала.</small></section><footer>Cookies не попадают в журналы. SSH-ключ используется из указанного вами файла; ключ подписи защищён вашим входом в Windows.</footer></main>
<script>const token='__TOKEN__',$=id=>document.getElementById(id);let working=false;
function choose(){$('passwordRow').classList.toggle('hidden',$('target').value==='aws');$('package').disabled=$('target').value!=='aws';$('state').textContent='Состояние ещё не проверено';$('reason').textContent='';$('lastFailure').textContent='';$('inbox').textContent='';} $('target').onchange=choose;
async function request(action){if(working)return;working=true;document.querySelectorAll('button').forEach(b=>b.disabled=true);$('result').textContent='Выполняется…';try{const r=await fetch('/api',{method:'POST',headers:{'Content-Type':'application/json','X-OpenFlux-Token':token},body:JSON.stringify({action,target:$('target').value,password:$('password').value,curl:$('curl').value})});const d=await r.json();if(!r.ok||d.ok===false)throw Error(d.error||'Операция не выполнена');$('result').textContent=d.message||'Готово';if(d.status)show(d.status);if(action==='import'||action==='package')$('curl').value='';}catch(e){$('result').textContent=e.message;}finally{working=false;document.querySelectorAll('button').forEach(b=>b.disabled=false);$('package').disabled=$('target').value!=='aws';}}
function show(s){$('state').textContent=({connected:'Connected · Подключено',connecting:'Connecting · Подключение',auth_blocked:'AUTH_BLOCKED · Нужны свежие cookies',auth_failed:'Authentication failed · Ошибка авторизации',stopped:'Stopped · Сервис остановлен'})[s.state]||'Состояние неизвестно';$('reason').textContent=s.reason||'';$('lastFailure').textContent=s.last_failure?'Последняя ошибка авторизации: '+new Date(s.last_failure.at*1000).toLocaleString()+' — '+s.last_failure.reason:'';$('inbox').textContent=s.recovery?'Канал восстановления: '+s.recovery.inbox+(s.recovery.applied_at?' · Применён: '+new Date(s.recovery.applied_at*1000).toLocaleString():''):'';}
$('statusButton').onclick=()=>request('status');$('direct').onclick=()=>request('import');$('package').onclick=()=>request('package');$('openOutbox').onclick=()=>request('outbox');</script></html>'''

def ssh_action(target,password,curl=None):
    cfg=CONFIG['targets'][target];c=paramiko.SSHClient();c.load_host_keys(str(ROOT/'known_hosts'))
    kwargs={'username':'ubuntu','look_for_keys':False,'allow_agent':False,'timeout':12,'auth_timeout':12}
    if cfg.get('key'):kwargs['key_filename']=cfg['key']
    else:
        if not password:raise ValueError('Введите пароль ubuntu для Mini-PC.')
        kwargs['password']=password
    try:
        c.connect(cfg['host'],**kwargs)
        stdin,stdout,stderr=c.exec_command('sudo -n /usr/local/sbin/openflux-auth '+('import' if curl is not None else 'status'),timeout=45)
        if curl is not None:stdin.write(curl)
        stdin.channel.shutdown_write();raw=stdout.read();code=stdout.channel.recv_exit_status()
        if code:raise ValueError('Операция на устройстве не выполнена. Проверьте установленный модуль восстановления.')
        data=json.loads(raw)
        return data if curl is not None else {'ok':True,'status':data,'message':'Состояние обновлено.'}
    except paramiko.AuthenticationException:raise ValueError('SSH authentication failed. Проверьте пароль или ключ.') from None
    except (OSError,paramiko.SSHException):raise ValueError('Устройство недоступно по SSH. Для AWS можно создать файл и передать его через Яндекс Диск.') from None
    finally:c.close()

def validate_curl(text):
    if not isinstance(text,str) or not 1<=len(text.encode())<=131072:raise ValueError('Вставьте Copy as cURL (bash), не больше 128 KiB.')
    # Only parse; never execute pasted commands.
    try:cookie_import.parse_cookie_map(cookie_import.extract_cookie_header(text))
    except (ValueError,SystemExit):raise ValueError('В команде не найдены cookies. Скопируйте запрос документа вместе с заголовками.') from None

def action(body):
    kind=body.get('action');target=body.get('target')
    if target not in CONFIG['targets']:raise ValueError('Неизвестное устройство.')
    if kind=='status':return ssh_action(target,body.get('password',''))
    outbox=ROOT/'outbox';outbox.mkdir(exist_ok=True)
    if kind=='outbox':os.startfile(outbox);return {'ok':True,'message':'Папка с aws-recovery.json открыта.'}
    text=body.get('curl');validate_curl(text)
    if kind=='import':return ssh_action(target,body.get('password',''),text)
    if kind=='package':
        if target!='aws':raise ValueError('Файл предназначен для AWS.')
        signer=serialization.load_pem_private_key(windows_secrets.unprotect((ROOT/'signer.dpapi').read_bytes()),password=None)
        public=serialization.load_pem_public_key((ROOT/'server-public.pem').read_bytes())
        package=recovery_crypto.seal(text,public,signer)
        dest=outbox/'aws-recovery.json';tmp=outbox/'aws-recovery.json.tmp';tmp.write_text(json.dumps(package,indent=2));os.replace(tmp,dest)
        return {'ok':True,'message':'Файл aws-recovery.json создан. Откройте папку с файлом и загрузите его в OpenFlux Recovery на Яндекс Диске, заменив старый файл. Пакет действует один час.'}
    raise ValueError('Неизвестная операция.')

class Handler(BaseHTTPRequestHandler):
    def log_message(self,*args):pass
    def reply(self,value,status=200,html=False):
        data=(value if html else json.dumps(value,ensure_ascii=False)).encode();self.send_response(status)
        self.send_header('Content-Type','text/html; charset=utf-8' if html else 'application/json; charset=utf-8');self.send_header('Content-Length',str(len(data)));self.send_header('Cache-Control','no-store');self.send_header('X-Frame-Options','DENY');self.send_header('Referrer-Policy','no-referrer');self.end_headers();self.wfile.write(data)
    def do_GET(self):
        if self.path=='/':return self.reply(HTML.replace('__TOKEN__',TOKEN),html=True)
        self.reply({'ok':False},404)
    def do_POST(self):
        origin='http://127.0.0.1:'+str(self.server.server_port)
        if self.path!='/api' or self.headers.get('Origin')!=origin or self.headers.get('X-OpenFlux-Token')!=TOKEN:return self.reply({'ok':False,'error':'Request rejected'},403)
        try:
            length=int(self.headers.get('Content-Length','0'))
            if not 1<=length<=150000:raise ValueError('Недопустимый размер запроса.')
            self.reply(action(json.loads(self.rfile.read(length))))
        except ValueError as e:self.reply({'ok':False,'error':str(e)},400)
        except Exception:self.reply({'ok':False,'error':'Операция не выполнена. Проверьте настройки OpenFlux Recovery.'},500)

if __name__=='__main__':
    server=ThreadingHTTPServer(('127.0.0.1',0),Handler);url='http://127.0.0.1:'+str(server.server_port)+'/'
    (ROOT/'running.json').write_text(json.dumps({'url':url,'pid':os.getpid()}))
    if '--no-browser' not in sys.argv:webbrowser.open(url)
    server.serve_forever()
