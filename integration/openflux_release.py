"""OpenFlux release integration for the existing Router Updater worker."""
import contextlib, hashlib, ipaddress, json, os, pathlib, platform, pwd, re, shutil, subprocess, tarfile, tempfile, time, uuid

REPO='LevL-max/OpenFlux-mod'
ASSETS=('openflux-linux-amd64','openflux-yandex-cookie-import','SHA256SUMS')
BUNDLE='openflux-integration-linux.tar.gz'
NODE_ASSET='openflux-node.py'
# One release carries both protocols. A node set up for Volga updates it with Legacy.
VOLGA_ASSETS=('openflux-volga-linux-amd64','protocol-manifest.json')
MODULES=('openflux_release.py','openflux_auth.py','cookie_import.py','recovery_crypto.py','recovery_inbox.py','router_integration.py')
LIBDIR=pathlib.Path(__file__).parent
RUNNER=pathlib.Path('/usr/local/sbin/openflux-yandex-client-run')
HELPER=pathlib.Path('/usr/local/sbin/openflux-yandex-cookie-import')
STORE=pathlib.Path('/var/lib/openflux-client/yandex-cookies.json')
DROPIN=pathlib.Path('/etc/systemd/system/openflux-yandex-client.service.d/95-cookie-store.conf')
UNIT='openflux-yandex-client.service'
UNIT_DIR=pathlib.Path('/etc/systemd/system')
PANEL=pathlib.Path('/usr/local/lib/router-panel/router-panel.py')
NODE_PROFILE=pathlib.Path('/etc/openflux/node.json')
VOLGA_UNIT='openflux-volga-client.service'
VOLGA_BINARY=pathlib.Path('/opt/openflux-volga/openflux-volga')
VOLGA_CONFIG=pathlib.Path('/etc/openflux-volga/config.json')
VOLGA_STATE=pathlib.Path('/var/lib/openflux-volga-updater')
VOLGA_INSTALLED=VOLGA_STATE/'installed.json'
ROUTER_HOOKS=('/usr/local/sbin/openflux-routerctl','/usr/local/sbin/openflux-clientctl','/usr/local/sbin/router-restore-runtime',
              '/usr/local/sbin/router-runtime-ensure','/usr/local/lib/router-wan/openflux_health.py','/usr/local/lib/router-xray/openflux_health.py')

def version(tag):
    m=re.fullmatch(r'v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-rc([1-9]\d*))?',tag or '')
    if not m:raise ValueError('OpenFlux requires a vMAJOR.MINOR.PATCH or -rcN release version')
    return tuple(map(int,m.group(1,2,3)))+(1 if m[4] is None else 0,int(m[4] or 0))

def metadata(data):
    tag=data.get('tag_name');version(tag)
    if data.get('draft') or not data.get('published_at'):raise ValueError('Only published releases can be installed')
    base='https://github.com/'+REPO+'/releases/'
    if data.get('html_url')!=base+'tag/'+tag:raise ValueError('Unexpected release repository')
    published={a.get('name') for a in data.get('assets',[])}
    if any(name in published for name in VOLGA_ASSETS) and not all(name in published for name in VOLGA_ASSETS):raise ValueError('Incomplete Volga release assets')
    assets={}
    names=ASSETS+tuple(name for name in (BUNDLE,NODE_ASSET)+VOLGA_ASSETS if name in published)
    for name in names:
        items=[a for a in data.get('assets',[]) if a.get('name')==name]
        if len(items)!=1:raise ValueError('Missing or duplicate release asset: '+name)
        a=items[0];digest=a.get('digest','')
        if a.get('browser_download_url')!=base+'download/'+tag+'/'+name:raise ValueError('Unexpected release asset URL')
        if not re.fullmatch(r'sha256:[0-9a-f]{64}',digest or ''):raise ValueError('Missing published asset SHA256')
        assets[name]={'url':a['browser_download_url'],'sha256':digest[7:]}
    result={'version':tag,'repo':REPO,'release_id':data['id'],'prerelease':bool(data.get('prerelease')),
            'published_at':data['published_at'],'release_url':data['html_url'],'assets':assets,
            'asset':ASSETS[0],'url':assets[ASSETS[0]]['url'],'asset_sha256':assets[ASSETS[0]]['sha256']}
    if VOLGA_ASSETS[0] in assets:result['volga_sha256']=assets[VOLGA_ASSETS[0]]['sha256']
    return result

def release(core,transport):
    if platform.machine()!='x86_64':raise ValueError('OpenFlux updater currently requires Linux amd64')
    with tempfile.TemporaryDirectory(dir=core.STATE) as temp:
        path=pathlib.Path(temp)/'releases.json'
        core.download('https://api.github.com/repos/'+REPO+'/releases?per_page=100',path,transport,max_bytes=8*1024*1024)
        choices=[]
        for item in core.read_json(path,[]):
            if item.get('draft') or not item.get('published_at') or (item.get('prerelease') and not core.config()['openflux_prereleases']):continue
            try:version(item.get('tag_name'))
            except ValueError:continue
            choices.append(item)
        if not choices:raise ValueError('No supported published OpenFlux release')
        return metadata(max(choices,key=lambda x:version(x['tag_name'])))

def no_downgrade(core,candidate):
    installed=core.installed('openflux')
    try:current=version(installed['version'])
    except ValueError:raise ValueError('Installed release version is unknown; identify its SHA before installing') from None
    if version(candidate['version'])<=current and installed['sha256']!=candidate['asset_sha256']:
        raise ValueError('Downgrade/replacement through Install is disabled. Use Rollback for a local checkpoint.')

def volga_enabled():
    # Setting Volga up (its configuration) makes it follow every OpenFlux release.
    return VOLGA_CONFIG.exists() or VOLGA_BINARY.exists()

def installed_volga():
    if not VOLGA_BINARY.is_file():return None
    try:record=json.loads(VOLGA_INSTALLED.read_text())
    except (FileNotFoundError,ValueError):record={}
    return {'version':record.get('version'),'sha256':hashlib.sha256(VOLGA_BINARY.read_bytes()).hexdigest()}

def is_current(current,candidate):
    # An integration-only release can legitimately reuse the binary. Volga set up
    # on this node must match the same release.
    if candidate.get('volga_sha256') and volga_enabled():
        volga=installed_volga()
        if not volga or volga['version']!=candidate['version'] or volga['sha256']!=candidate['volga_sha256']:return False
    return current['sha256']==candidate['asset_sha256'] and current.get('version')==candidate['version']

def selected_protocol():
    try:return json.loads(NODE_PROFILE.read_text()).get('active_transport','yandex')
    except (FileNotFoundError,ValueError):return 'yandex'

def load_integration(path,name):
    import importlib.util
    spec=importlib.util.spec_from_file_location(name,path)
    module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module)
    return module

def bundle_integration(directory):
    # The candidate's own code: a later release may change the Volga profile.
    return load_integration(directory/'integration'/'router_integration.py','openflux_candidate_integration')

def checksums(text):
    result={}
    for line in text.splitlines():
        m=re.fullmatch(r'([0-9a-f]{64})\s+\*?(?:dist/)?([A-Za-z0-9._-]+)',line.strip())
        if not m:raise ValueError('Malformed SHA256SUMS')
        if m[2] in result:raise ValueError('Duplicate SHA256SUMS entry')
        result[m[2]]=m[1]
    return result

def prepare(core,route):
    candidate=core.check_binary('openflux',route);no_downgrade(core,candidate)
    if is_current(core.installed('openflux'),candidate):
        core.progress('OpenFlux is already current.');return
    if (VOLGA_STATE/'transaction.json').exists():raise ValueError('Recover the interrupted Volga operation first: sudo openfluxctl recover-volga')
    volga=bool(candidate.get('volga_sha256')) and volga_enabled()
    if volga_enabled() and not volga:core.progress('This release has no Volga build; the installed Volga stays unchanged.')
    wanted={name:item for name,item in candidate['assets'].items() if volga or name not in VOLGA_ASSETS}
    directory=core.component_dir('openflux');temp=pathlib.Path(tempfile.mkdtemp(prefix='candidate-',dir=directory))
    with core.transport_for(route) as transport:
        for name,item in wanted.items():
            core.download(item['url'],temp/name,transport,max_bytes=64*1024*1024 if name in (ASSETS[0],VOLGA_ASSETS[0]) else 1024*1024)
            if core.sha(temp/name)!=item['sha256']:raise ValueError('Published asset SHA256 mismatch: '+name)
    sums=checksums((temp/'SHA256SUMS').read_text())
    for name in wanted:
        if name=='SHA256SUMS':continue
        if sums.get(name)!=core.sha(temp/name):raise ValueError('Release SHA256SUMS mismatch: '+name)
    if BUNDLE in wanted:
        extract_bundle(temp/BUNDLE,temp/'integration')
    if volga:
        if BUNDLE not in wanted:raise ValueError('Volga requires the release integration bundle')
        os.replace(temp/VOLGA_ASSETS[0],temp/'volga');os.chmod(temp/'volga',0o755)
        # Offline check only: starting Volga here could take the shared document pair.
        result=core.run([str(temp/'volga'),'-version'],record=False)
        if 'openflux-volga' not in (result.stdout or '')+(getattr(result,'stderr',None) or ''):raise ValueError('Candidate lacks the Volga runtime')
        manifest=json.loads((temp/VOLGA_ASSETS[1]).read_text())
        bundle_integration(temp).validate_volga_manifest(manifest,candidate['version'],core.sha(temp/'volga'))
    os.replace(temp/ASSETS[0],temp/'binary');os.chmod(temp/'binary',0o755)
    os.replace(temp/ASSETS[1],temp/'helper');os.chmod(temp/'helper',0o755)
    helpresult=core.run([str(temp/'binary'),'--help'],record=False);helptext=(helpresult.stdout or '')+(getattr(helpresult,'stderr',None) or '')
    if 'yandex-cookie-store' not in helptext:raise ValueError('Candidate lacks persistent cookie-store support')
    core.run(['python3',str(temp/'helper'),'--help'],record=False)
    candidate.update(directory=temp.name,binary_sha256=core.sha(temp/'binary'),helper_sha256=core.sha(temp/'helper'),integration_sha256=candidate['assets'].get(BUNDLE,{}).get('sha256'),
                     base_sha256=core.sha(core.COMPONENTS['openflux']['binary']),prepared_at=int(time.time()),bundle_schema=1,volga=volga)
    core.write_json(directory/'staged.json',candidate)
    core.progress('Prepared '+candidate['version']+'; '+('Legacy, Volga' if volga else 'binary')+' and cookie helper verified against this release SHA256SUMS. Ready for Install.')

def extract_bundle(archive,destination):
    allowed=set(MODULES)|{'install.py','README.md'}
    destination.mkdir(mode=0o700,exist_ok=True)
    found=set()
    with tarfile.open(archive,'r:gz') as tar:
        for item in tar.getmembers():
            if item.isdir() and item.name in ('integration','integration/'):continue
            name=item.name.removeprefix('integration/')
            if item.name!='integration/'+name or name not in allowed or name in found or not item.isfile() or item.size>512*1024:raise ValueError('Unexpected integration archive member')
            found.add(name)
            with tar.extractfile(item) as src:content=src.read(512*1024+1)
            if len(content)>512*1024:raise ValueError('Integration member too large')
            (destination/name).write_bytes(content)
    if not set(MODULES).issubset(found):raise ValueError('Incomplete integration bundle')

def support_paths():
    paths=[('runner',RUNNER),('helper',HELPER),('cookie-dropin',DROPIN),('node-control',LIBDIR/'openflux_node.py'),('router-panel',PANEL)]+[('integration-'+name,LIBDIR/name) for name in MODULES]
    # Volga and the router hooks share this checkpoint: one rollback restores one release.
    paths+=[('volga-binary',VOLGA_BINARY),('volga-unit',UNIT_DIR/VOLGA_UNIT),('volga-installed',VOLGA_INSTALLED),
            ('volga-recovery-service',UNIT_DIR/'openflux-volga-recovery.service'),('volga-recovery-timer',UNIT_DIR/'openflux-volga-recovery.timer')]
    return paths+[('hook-'+pathlib.Path(p).parent.name+'-'+pathlib.Path(p).name,pathlib.Path(p)) for p in ROUTER_HOOKS]

def backup_files(core,checkpoint):
    records={}
    for name,path in support_paths():
        if path.exists():shutil.copy2(path,checkpoint/name);records[name]={'present':True,'sha256':core.sha(checkpoint/name),'mode':path.stat().st_mode&0o777}
        else:records[name]={'present':False}
    return records

def restore_files(core,checkpoint,records):
    items=[(name,path,records[name]) for name,path in support_paths() if name in records]
    # Validate every copy before the first write.
    for name,path,item in items:
        if item['present'] and core.sha(checkpoint/name)!=item['sha256']:raise ValueError('Rollback support-file checksum mismatch')
    timer=records.get('volga-recovery-timer')
    if timer and not timer['present']:core.run(['systemctl','disable','--now','openflux-volga-recovery.timer'],record=False,check=False)
    for name,path,item in items:
        if item['present']:core.atomic_binary(checkpoint/name,path);os.chmod(path,item.get('mode',0o755))
        else:path.unlink(missing_ok=True)
    core.run(['systemctl','daemon-reload'],record=False)
    if any(name.startswith('integration-') for name in records):core.run(['systemctl','try-restart','router-panel.service'],record=False)

def install_support(core,source,metadata):
    if metadata.get('bundle_schema')!=1 or core.sha(source.parent/'helper')!=metadata.get('helper_sha256'):raise ValueError('Unverified cookie helper; Download this release again')
    text=RUNNER.read_text()
    if '--yandex-cookie-file' in text:raise ValueError('Remove a permanent cookie seed before installing')
    if '--yandex-cookie-store' not in text:
        marker='  --socks5 127.0.0.1:11080 '
        if text.count(marker)!=1:raise ValueError('Unknown OpenFlux runner layout')
        text=text.replace(marker,'  --yandex-cookie-store '+str(STORE)+' \\\n'+marker)
    # Older binaries emit auth events only with --debug; discover support from this artifact.
    helpresult=core.run([str(source),'--help'],record=False);helptext=(helpresult.stdout or '')+(getattr(helpresult,'stderr',None) or '')
    if '--status-events' in helptext or '-status-events' in helptext:
        text=text.replace('  --debug \\\n','')
    elif '  --debug ' not in text:
        text=text.replace('  --client \\\n','  --client \\\n  --debug \\\n')
    account=pwd.getpwnam('openflux')
    STORE.parent.mkdir(parents=True,mode=0o700,exist_ok=True);os.chown(STORE.parent,account.pw_uid,account.pw_gid);os.chmod(STORE.parent,0o700)
    if STORE.exists():os.chown(STORE,account.pw_uid,account.pw_gid);os.chmod(STORE,0o600)
    runner=source.parent/'runner-new';runner.write_text(text)
    core.run(['bash','-n',str(runner)],record=False)
    core.atomic_binary(source.parent/'helper',HELPER);core.atomic_binary(runner,RUNNER)
    dropin=source.parent/'cookie-dropin';dropin.write_text('[Service]\nReadWritePaths=/var/lib/openflux-client\n')
    DROPIN.parent.mkdir(parents=True,exist_ok=True);core.atomic_binary(dropin,DROPIN);os.chmod(DROPIN,0o644)
    core.run(['systemctl','daemon-reload'],record=False)
    if metadata.get('integration_sha256'):
        if core.sha(source.parent/BUNDLE)!=metadata['integration_sha256']:raise ValueError('Integration bundle checksum changed')
        extract_bundle(source.parent/BUNDLE,source.parent/'integration')
        for name in MODULES:
            module=source.parent/'integration'/name;compile(module.read_text(),str(module),'exec')
        for name in MODULES:core.atomic_binary(source.parent/'integration'/name,LIBDIR/name);os.chmod(LIBDIR/name,0o644)
        if NODE_ASSET in metadata.get('assets',{}):
            node=source.parent/NODE_ASSET
            if core.sha(node)!=metadata['assets'][NODE_ASSET]['sha256']:raise ValueError('Node updater checksum changed')
            compile(node.read_text(),NODE_ASSET,'exec');core.atomic_binary(node,LIBDIR/'openflux_node.py')
        # Use the newly extracted module, not an older module cached by the updater.
        integration=bundle_integration(source.parent)
        if PANEL.exists():
            patched=integration.patch_panel(PANEL.read_text());compile(patched,str(PANEL),'exec')
            staged=source.parent/'panel-new';staged.write_text(patched)
            core.atomic_binary(staged,PANEL);os.chmod(PANEL,0o644)
        if metadata.get('volga'):install_volga(core,source,metadata,integration)
        core.run(['systemctl','try-restart','router-panel.service'],record=False)

def install_volga(core,source,metadata,integration):
    # Volga follows the same release: executable, unit with the current limits,
    # router hooks and recovery timer. Verified offline; never started here.
    staged=source.parent/'volga'
    if core.sha(staged)!=metadata['volga_sha256']:raise ValueError('Volga binary checksum changed')
    VOLGA_BINARY.parent.mkdir(parents=True,exist_ok=True);core.atomic_binary(staged,VOLGA_BINARY)
    runtime=integration.VolgaRuntime()
    unit=source.parent/'volga-unit';unit.write_text(runtime.unit_text())
    core.atomic_binary(unit,UNIT_DIR/VOLGA_UNIT);os.chmod(UNIT_DIR/VOLGA_UNIT,0o644)
    runtime.install_router_hooks()
    runtime.install_recovery_timer()
    runtime.save(VOLGA_INSTALLED,{'version':metadata['version'],'sha256':metadata['volga_sha256']})

def retire_volga_updater():
    # The separate Volga updater is gone; its checkpoints would restore mismatched support code.
    for name in ('staged.json','rollback.json'):(VOLGA_STATE/name).unlink(missing_ok=True)
    for path in [*VOLGA_STATE.glob('backup-*'),*VOLGA_STATE.glob('candidate-*')]:
        if path.is_dir() and not path.is_symlink():shutil.rmtree(path)

def panel_health(core,seconds=30):
    # A panel that no longer starts must not survive an update.
    if not PANEL.exists():return
    deadline=time.monotonic()+seconds
    while time.monotonic()<deadline:
        if core.run(['curl','-q','--silent','--fail','--max-time','3','http://127.0.0.1:8080/healthz'],timeout=5,check=False,record=False).returncode==0:return
        time.sleep(1)
    raise RuntimeError('Router panel did not start after the update')

def health(core,seconds=100):
    deadline=time.monotonic()+seconds
    expected=core.config().get('openflux_expected_exit_ip')
    while time.monotonic()<deadline:
        result=core.run(['/usr/local/sbin/openflux-auth','status'],record=False,check=False)
        try:state=json.loads(result.stdout)
        except ValueError:state={}
        if state.get('state')=='auth_blocked':raise RuntimeError('AUTH_BLOCKED: refresh cookies in the OpenFlux panel, then Install again.')
        if state.get('state')=='connected':
            for url in ['https://api.ipify.org','https://checkip.amazonaws.com']:
                p=core.run(['curl','-q','-4','--silent','--fail','--proxy','socks5h://127.0.0.1:11080','--noproxy','','--connect-timeout','6','--max-time','12',url],timeout=15,check=False,record=False)
                if p.returncode==0:
                    try:address=str(ipaddress.ip_address(p.stdout.strip()))
                    except ValueError:continue
                    if not expected or address==expected:return
        time.sleep(2)
    raise RuntimeError('OpenFlux did not pass authentication and AWS exit-IP health checks; inspect authentication status.')

def stop_runtimes(core,active,check=True):
    # Legacy always; Volga only where it runs, so a node without Volga needs no unit.
    core.run(['systemctl','stop',UNIT]+[unit for unit in active if unit==VOLGA_UNIT],timeout=30,record=False,check=check)

def test_selected(core,selected,active,rollback):
    # Test the protocol this router uses. Volga is started only where it already
    # runs: starting it elsewhere would take the shared document pair.
    if selected=='volga':
        if VOLGA_UNIT in active:
            core.run(['systemctl','start',VOLGA_UNIT],timeout=30,record=False)
            load_integration(LIBDIR/'router_integration.py','openflux_installed_integration').VolgaRuntime().health()
        else:core.progress('Volga is selected but stopped; its executable was verified without starting it.')
        return
    # An explicit rollback to a stopped historical version restores its original stopped state.
    if not rollback or UNIT in active:
        core.run(['systemctl','start',UNIT],timeout=30,record=False);health(core)
    if UNIT not in active:core.run(['systemctl','stop',UNIT],timeout=30,record=False)

def replace(core,source,metadata):
    rollback=source.parent.name.startswith('backup-')
    if not rollback:no_downgrade(core,metadata)
    if (VOLGA_STATE/'transaction.json').exists():raise ValueError('Recover the interrupted Volga operation first: sudo openfluxctl recover-volga')
    binary=pathlib.Path(core.COMPONENTS['openflux']['binary']);mode=core.MODE.read_text().strip()
    selected=selected_protocol()
    active=[unit for unit in (UNIT,VOLGA_UNIT) if core.service_active(unit)]
    panel=core.service_active('router-panel.service')
    if not rollback and metadata.get('bundle_schema')!=1:raise ValueError('Download the complete verified OpenFlux release first')
    if rollback and 'support_files' not in metadata:raise ValueError('Legacy binary-only rollback lacks its runner backup; use a complete checkpoint')
    if active:core.connectivity_gate()
    directory=core.component_dir('openflux');checkpoint=directory/('backup-'+time.strftime('%Y%m%dT%H%M%SZ',time.gmtime())+'-'+uuid.uuid4().hex[:6]);checkpoint.mkdir(mode=0o700)
    shutil.copy2(binary,checkpoint/'binary');before=core.installed('openflux')
    record={'directory':checkpoint.name,'version':before['version'],'binary_sha256':before['sha256'],'replaces_sha256':core.sha(source),
            'mode':mode,'active':active,'selected':selected,'created_at':int(time.time()),'support_files':backup_files(core,checkpoint)}
    core.write_json(checkpoint/'metadata.json',record)
    # Publish a recoverable checkpoint before the first replacement, including on an interrupted install.
    core.write_json(directory/'transaction.json',dict(record,target_version=metadata['version']))
    core.progress('Complete checkpoint saved (Legacy, Volga and router support). Testing the selected protocol without changing the router mode.')
    with contextlib.ExitStack() as locks:
        locks.enter_context(core.exclusive(pathlib.Path('/run/watchdog-rotator.lock')))
        locks.enter_context(core.exclusive(pathlib.Path('/run/tun-rotator.lock')))
        guard=core.guard_active_route('openflux',mode,active);recovered=False
        try:
            stop_runtimes(core,active)
            if rollback:restore_files(core,source.parent,metadata['support_files'])
            else:install_support(core,source,metadata)
            core.atomic_binary(source,binary)
            test_selected(core,selected,active,rollback)
            core.restore_runtime(mode,active,'openflux')
            if panel:panel_health(core)
            if active:core.connectivity_gate()
            recovered=True
        except BaseException as failure:
            core.progress('Health gate failed. Restoring the complete local checkpoint.')
            stop_runtimes(core,active,check=False)
            core.atomic_binary(checkpoint/'binary',binary);restore_files(core,checkpoint,record['support_files'])
            core.restore_runtime(mode,active,'openflux');recovered=True
            raise RuntimeError('Previous installation restored. '+str(failure)) from failure
        finally:
            if recovered:core.remove_update_guard(guard)
    core.write_json(directory/'rollback.json',record)
    core.write_json(directory/'installed.json',{'version':metadata['version'],'sha256':core.sha(binary)})
    (directory/'transaction.json').unlink(missing_ok=True)
    if not rollback:retire_volga_updater()
    tested={'volga':'Volga session + SOCKS + configured exit verified.' if VOLGA_UNIT in active else 'Volga stays stopped; its executable was verified.'}
    core.progress('Installed '+metadata['version']+'. '+('Previous stopped runtime restored.' if rollback and not active else tested.get(selected,'Authentication + SOCKS + configured exit verified.'))+' Router mode remains '+mode.upper()+'.')
