"""Signed, encrypted, expiring cookie envelopes. Payloads are data, never commands."""
import base64,json,os,time,uuid
from cryptography.hazmat.primitives import hashes,serialization
from cryptography.hazmat.primitives.asymmetric import padding
from cryptography.hazmat.primitives.ciphers.aead import AESGCM

SCHEMA='openflux-cookie-recovery-v1'
TARGET='openflux-server'
def encode(data):return base64.b64encode(data).decode('ascii')
def decode(value):return base64.b64decode(value,validate=True)
def canonical(value):return json.dumps(value,sort_keys=True,separators=(',',':')).encode()

def destination(protocol):
    if protocol=='yandex':return SCHEMA,TARGET
    if protocol=='volga':return 'openflux-volga-cookie-recovery-v1','openflux-volga-server'
    raise ValueError('Unsupported recovery protocol')

def seal(curl,server_public,signer,now=None,protocol='yandex'):
    if not isinstance(curl,str) or not 1<=len(curl.encode())<=131072:raise ValueError('Invalid cookie input')
    now=int(time.time() if now is None else now)
    schema,target=destination(protocol)
    header={'schema':schema,'target':target,'id':uuid.uuid4().hex,'created':now,'expires':now+3600}
    key=os.urandom(32);nonce=os.urandom(12)
    cipher=AESGCM(key).encrypt(nonce,canonical({'curl':curl}),canonical(header))
    envelope=dict(header,key=encode(server_public.encrypt(key,padding.OAEP(mgf=padding.MGF1(hashes.SHA256()),algorithm=hashes.SHA256(),label=schema.encode()))),nonce=encode(nonce),ciphertext=encode(cipher))
    envelope['signature']=encode(signer.sign(canonical(envelope)))
    return envelope

def unseal(envelope,server_private,sender_public,seen,now=None,protocol='yandex'):
    now=int(time.time() if now is None else now)
    if not isinstance(envelope,dict) or len(canonical(envelope))>220000:raise ValueError('Invalid envelope')
    signed=dict(envelope);signature=signed.pop('signature',None)
    sender_public.verify(decode(signature),canonical(signed))
    schema,target=destination(protocol)
    if signed.get('schema')!=schema or signed.get('target')!=target:raise ValueError('Wrong recovery destination')
    if not isinstance(signed.get('created'),int) or not isinstance(signed.get('expires'),int):raise ValueError('Invalid expiry')
    if signed['created']>now+120 or not 0<signed['expires']-signed['created']<=3600 or now>signed['expires']:raise ValueError('Recovery package has expired')
    ident=signed.get('id')
    if not isinstance(ident,str) or len(ident)!=32 or any(c not in '0123456789abcdef' for c in ident):raise ValueError('Invalid package ID')
    if ident in seen:raise ValueError('Recovery package already applied')
    header={k:signed[k] for k in ('schema','target','id','created','expires')}
    key=server_private.decrypt(decode(signed['key']),padding.OAEP(mgf=padding.MGF1(hashes.SHA256()),algorithm=hashes.SHA256(),label=schema.encode()))
    payload=json.loads(AESGCM(key).decrypt(decode(signed['nonce']),decode(signed['ciphertext']),canonical(header)))
    curl=payload.get('curl')
    if not isinstance(curl,str) or not 1<=len(curl.encode())<=131072:raise ValueError('Invalid cookie payload')
    return curl,ident

def sign_status(status,server_private,now=None,protocol='yandex'):
    """Publish operational status only; never cookies, URLs, arguments or tokens."""
    safe={k:status[k] for k in ('state','reason','active','needs_cookies','last_failure') if k in status}
    destination(protocol)
    value={'schema':'openflux-server-status-v1' if protocol=='yandex' else 'openflux-volga-server-status-v1','created':int(time.time() if now is None else now),'status':safe}
    value['signature']=encode(server_private.sign(canonical(value),padding.PSS(mgf=padding.MGF1(hashes.SHA256()),salt_length=padding.PSS.MAX_LENGTH),hashes.SHA256()))
    return value

# A server publishes its status on a change and at least every hour, and a
# client fetches it while someone looks; older than this, a status no longer counts.
STATUS_STALE_AFTER=4500

def verify_status(value,server_public,now=None,protocol='yandex'):
    if not isinstance(value,dict) or len(canonical(value))>16000:raise ValueError('Invalid server status')
    signed=dict(value);signature=signed.pop('signature',None)
    server_public.verify(decode(signature),canonical(signed),padding.PSS(mgf=padding.MGF1(hashes.SHA256()),salt_length=padding.PSS.MAX_LENGTH),hashes.SHA256())
    now=int(time.time() if now is None else now);created=signed.get('created')
    destination(protocol)
    schema='openflux-server-status-v1' if protocol=='yandex' else 'openflux-volga-server-status-v1'
    if signed.get('schema')!=schema or not isinstance(created,int) or created>now+120 or not isinstance(signed.get('status'),dict):raise ValueError('Invalid server status timestamp')
    return {'state':signed['status'].get('state','unknown'),'status':signed['status'],'reported_at':created,'stale':now-created>STATUS_STALE_AFTER}
