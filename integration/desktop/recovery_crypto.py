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

def seal(curl,server_public,signer,now=None):
    if not isinstance(curl,str) or not 1<=len(curl.encode())<=131072:raise ValueError('Invalid cookie input')
    now=int(time.time() if now is None else now)
    header={'schema':SCHEMA,'target':TARGET,'id':uuid.uuid4().hex,'created':now,'expires':now+3600}
    key=os.urandom(32);nonce=os.urandom(12)
    cipher=AESGCM(key).encrypt(nonce,canonical({'curl':curl}),canonical(header))
    envelope=dict(header,key=encode(server_public.encrypt(key,padding.OAEP(mgf=padding.MGF1(hashes.SHA256()),algorithm=hashes.SHA256(),label=SCHEMA.encode()))),nonce=encode(nonce),ciphertext=encode(cipher))
    envelope['signature']=encode(signer.sign(canonical(envelope)))
    return envelope

def unseal(envelope,server_private,sender_public,seen,now=None):
    now=int(time.time() if now is None else now)
    if not isinstance(envelope,dict) or len(canonical(envelope))>220000:raise ValueError('Invalid envelope')
    signed=dict(envelope);signature=signed.pop('signature',None)
    sender_public.verify(decode(signature),canonical(signed))
    if signed.get('schema')!=SCHEMA or signed.get('target')!=TARGET:raise ValueError('Wrong recovery destination')
    if not isinstance(signed.get('created'),int) or not isinstance(signed.get('expires'),int):raise ValueError('Invalid expiry')
    if signed['created']>now+120 or not 0<signed['expires']-signed['created']<=3600 or now>signed['expires']:raise ValueError('Recovery package has expired')
    ident=signed.get('id')
    if not isinstance(ident,str) or len(ident)!=32 or any(c not in '0123456789abcdef' for c in ident):raise ValueError('Invalid package ID')
    if ident in seen:raise ValueError('Recovery package already applied')
    header={k:signed[k] for k in ('schema','target','id','created','expires')}
    key=server_private.decrypt(decode(signed['key']),padding.OAEP(mgf=padding.MGF1(hashes.SHA256()),algorithm=hashes.SHA256(),label=SCHEMA.encode()))
    payload=json.loads(AESGCM(key).decrypt(decode(signed['nonce']),decode(signed['ciphertext']),canonical(header)))
    curl=payload.get('curl')
    if not isinstance(curl,str) or not 1<=len(curl.encode())<=131072:raise ValueError('Invalid cookie payload')
    return curl,ident
