"""Generate the release protocol inventory from the actual built artifacts."""
import hashlib, json, pathlib, re, sys

def generate(directory, version, commit, image, image_id):
    if not re.fullmatch(r'v\d+\.\d+\.\d+(?:-rc\d+)?', version):
        raise ValueError('Invalid release version')
    if not re.fullmatch(r'[0-9a-f]{40}', commit):
        raise ValueError('Invalid source commit')
    if not re.fullmatch(r'ghcr.io/levl-max/openflux-mod-volga@sha256:[0-9a-f]{64}', image):
        raise ValueError('Expected immutable Volga image digest')
    if not re.fullmatch(r'sha256:[0-9a-f]{64}',image_id):raise ValueError('Expected immutable Docker image ID')
    def asset(name):
        return {'name': name, 'sha256': hashlib.sha256((directory/name).read_bytes()).hexdigest()}
    return {'schema': 1, 'version': version, 'source_commit': commit, 'default': 'yandex', 'protocols': [
        {'id': 'yandex', 'label': 'Yandex Legacy', 'binary': asset('openflux-linux-amd64')},
        {'id': 'volga', 'label': 'Volga', 'binary': asset('openflux-volga-linux-amd64'),
         'container_archive': asset('openflux-volga-container-linux-amd64.tar.gz'), 'image': image, 'image_id': image_id,
         'config_protocol': 'volga-stream-v1', 'max_streams': 64,
         'performance_profile': json.loads((pathlib.Path(__file__).resolve().parents[1]/'docs/FROZEN-PERFORMANCE-PROFILE.json').read_text())}]}

if __name__ == '__main__':
    directory=pathlib.Path(sys.argv[1])
    (directory/'protocol-manifest.json').write_text(json.dumps(generate(directory,*sys.argv[2:]),indent=2)+'\n')
