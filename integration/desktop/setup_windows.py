#!/usr/bin/env python3
"""Install the localhost recovery UI and protect its signing key with Windows DPAPI."""
import argparse,json,os,pathlib,shutil,subprocess,sys

def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--aws-host',required=True);p.add_argument('--aws-key',type=pathlib.Path,required=True)
    p.add_argument('--mini1',required=True);p.add_argument('--mini2',required=True)
    p.add_argument('--known-hosts',type=pathlib.Path,required=True)
    p.add_argument('--server-public',type=pathlib.Path,help='Public RSA key from the paired server; can be supplied later')
    p.add_argument('--signer',type=pathlib.Path,help='Import an existing private Ed25519 signer; otherwise create once')
    p.add_argument('--dependencies',type=pathlib.Path,help='Use an existing verified Python dependency directory')
    a=p.parse_args()
    if os.name!='nt':p.error('Windows is required')
    root=pathlib.Path(os.environ['LOCALAPPDATA'])/'OpenFluxRecovery';root.mkdir(exist_ok=True)
    identity=subprocess.check_output(['whoami'],text=True).strip()
    subprocess.run(['icacls',str(root),'/inheritance:r','/grant:r',identity+':(OI)(CI)F'],check=True,capture_output=True)
    lib=root/'lib'
    if a.dependencies:shutil.copytree(a.dependencies,lib,dirs_exist_ok=True)
    else:subprocess.run([sys.executable,'-m','pip','install','--target',str(lib),'-r',str(pathlib.Path(__file__).with_name('requirements.txt'))],check=True)
    sys.path.insert(0,str(lib))
    from cryptography.hazmat.primitives import serialization
    from cryptography.hazmat.primitives.asymmetric import ed25519,rsa
    import windows_secrets
    source=pathlib.Path(__file__).parent
    for name in ('recovery_app.py','windows_secrets.py','recovery_crypto.py','cookie_import.py'):
        shutil.copyfile(source/name,root/name)
    keyfile=root/'signer.dpapi'
    if a.signer:raw=a.signer.read_bytes()
    elif keyfile.exists():raw=windows_secrets.unprotect(keyfile.read_bytes())
    else:raw=ed25519.Ed25519PrivateKey.generate().private_bytes(serialization.Encoding.PEM,serialization.PrivateFormat.PKCS8,serialization.NoEncryption())
    signer=serialization.load_pem_private_key(raw,password=None)
    if not isinstance(signer,ed25519.Ed25519PrivateKey):raise ValueError('Expected Ed25519 signing key')
    keyfile.write_bytes(windows_secrets.protect(raw))
    assert windows_secrets.unprotect(keyfile.read_bytes())==raw
    (root/'sender-public.pem').write_bytes(signer.public_key().public_bytes(serialization.Encoding.PEM,serialization.PublicFormat.SubjectPublicKeyInfo))
    if a.server_public:
        public=serialization.load_pem_public_key(a.server_public.read_bytes())
        if not isinstance(public,rsa.RSAPublicKey) or public.key_size<3072:raise ValueError('Expected RSA3072+ server key')
        shutil.copyfile(a.server_public,root/'server-public.pem')
    shutil.copyfile(a.known_hosts,root/'known_hosts')
    config={'targets':{'aws':{'host':a.aws_host,'key':str(a.aws_key.resolve())},'mini1':{'host':a.mini1},'mini2':{'host':a.mini2}}}
    (root/'config.json').write_text(json.dumps(config,indent=2))
    pythonw=pathlib.Path(sys.executable).with_name('pythonw.exe')
    if not pythonw.exists():pythonw=pathlib.Path(sys.executable)
    launcher=root/'OpenFlux Recovery.cmd'
    launcher.write_text('@echo off\r\nstart "" "'+str(pythonw)+'" "'+str(root/'recovery_app.py')+'"\r\n')
    print(json.dumps({'installed':str(root),'launcher':str(launcher),'sender_public_key':str(root/'sender-public.pem'),'paired':(root/'server-public.pem').exists()}))

if __name__=='__main__':main()
