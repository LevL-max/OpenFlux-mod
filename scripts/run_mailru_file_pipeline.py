#!/usr/bin/env python3
"""Bounded two-node file pipeline benchmark, with private SSH inventory input.

Inventory: {"key": "path", "aws": {"host": "user@host"},
"pc2": {"host": "user@host", "host_key_alias": "optional"}}.
No per-file control notification travels over SSH: receivers discover names via
the Cloud API. Only whole-batch startup and final measurements use SSH.
"""
import argparse
import base64
import concurrent.futures
import hashlib
import json
import pathlib
import queue
import shlex
import subprocess
import threading
import time
import uuid


class Worker:
    def __init__(self, inventory, role, nonce, source):
        config = inventory[role]
        command = ['ssh', '-i', inventory['key'], '-o', 'BatchMode=yes', '-o',
                   'StrictHostKeyChecking=yes', '-o', 'ConnectTimeout=10']
        if config.get('host_key_alias'):
            command += ['-o', 'HostKeyAlias=' + config['host_key_alias']]
        guard = 'import pathlib,subprocess;'
        if role == 'pc2':
            guard += "assert pathlib.Path('/etc/router/mode').read_text().strip()=='awg';"
            for unit in ('openflux-mailru-client', 'openflux-volga-client',
                         'openflux-yandex-client', 'sing-box-openflux'):
                guard += f"assert subprocess.run(['systemctl','is-active','--quiet','{unit}'],capture_output=True).returncode!=0;"
        code = guard + "import base64;exec(compile(base64.b64decode('" + base64.b64encode(source).decode() + "'),'git-file-probe','exec'))"
        remote = ('sudo -n timeout 600 python3 -u -B -c ' + shlex.quote(code)
                  + ' --profile /var/lib/openflux-mailru-auth/browser.json --nonce ' + nonce + ' --keepalive')
        self.process = subprocess.Popen(command + [config['host'], remote], stdin=subprocess.PIPE,
                                        stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True)
        self.lines = queue.Queue()
        def read():
            for line in self.process.stdout:
                self.lines.put(line)
            self.lines.put('{}')
        threading.Thread(target=read, daemon=True).start()

    def call(self, operation, **arguments):
        self.process.stdin.write(json.dumps(dict(arguments, op=operation)) + '\n')
        self.process.stdin.flush()
        result = json.loads(self.lines.get(timeout=100))
        if not result:
            raise RuntimeError('remote worker stopped')
        return result


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--inventory', type=pathlib.Path, required=True)
    parser.add_argument('--output', type=pathlib.Path, required=True)
    args = parser.parse_args()
    inventory = json.loads(args.inventory.read_text())
    source = pathlib.Path(__file__).with_name('mailru_file_probe.py').read_bytes()
    nonce = uuid.uuid4().hex
    report = {'at': time.time(), 'nonce': nonce, 'probe_sha256': hashlib.sha256(source).hexdigest(),
              'batches': [], 'cleanup': {}}
    workers = {}
    try:
        workers = {role: Worker(inventory, role, nonce, source) for role in ('aws', 'pc2')}
        with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
            initialized = list(pool.map(lambda worker: worker.call('init'), workers.values()))
            if not initialized[0].get('account_tag') or initialized[0].get('account_tag') != initialized[1].get('account_tag'):
                raise RuntimeError('initialization failed or accounts differ')
            print(json.dumps({'ready': True, 'same_account': True}), flush=True)
            # Three small-block observations per direction, then equal-size bulk
            # runs. One run per lane setting; no adaptive retuning or repeats.
            cases = [(1, 1, 65536)] * 3 + [(lanes, 12, 2097152) for lanes in (1, 4, 6)]
            for case, (lanes, count, size) in enumerate(cases):
                for reverse, (origin, dest) in enumerate((('aws', 'pc2'), ('pc2', 'aws'))):
                    batch_id = case * 2 + reverse
                    arguments = dict(origin=origin, batch_id=batch_id, count=count, lanes=lanes, size=size)
                    start = time.monotonic()
                    # Start the receiver before the sender. Readiness comes only
                    # from Mail.ru metadata, including bounded 404 polling.
                    receiving = pool.submit(workers[dest].call, 'batch', direction='get', **arguments)
                    sending = pool.submit(workers[origin].call, 'batch', direction='put', **arguments)
                    received, sent = receiving.result(), sending.result()
                    elapsed = time.monotonic() - start
                    send_rows = {row['index']: row for row in sent.get('rows', [])}
                    receive_rows = {row['index']: row for row in received.get('rows', [])}
                    verified = (len(send_rows) == count and len(receive_rows) == count
                                and not sent.get('failed') and not received.get('failed')
                                and all(send_rows[i].get('sha256') and send_rows[i].get('sha256') == receive_rows[i].get('sha256')
                                        and receive_rows[i].get('size') == size for i in range(count)))
                    row = dict(arguments, destination=dest, elapsed_s=elapsed,
                               payload_Bps=count*size/elapsed if verified else None,
                               verified=verified, sent=sent, received=received)
                    report['batches'].append(row)
                    row['cleanup'] = workers[origin].call('clear_batch', origin=origin, batch_id=batch_id, count=count)
                    print(json.dumps({key: value for key, value in row.items() if key not in ('sent', 'received')}), flush=True)
                    if not verified or row['cleanup'].get('remaining_objects') != 0:
                        raise RuntimeError('batch failed; remaining cases skipped')
    except Exception as error:
        report['stopped'] = type(error).__name__
    finally:
        for role, worker in workers.items():
            try:
                report['cleanup'][role] = worker.call('close')
                worker.process.wait(timeout=5)
            except Exception as error:
                report['cleanup'][role] = {'error_type': type(error).__name__}
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(json.dumps(report, indent=2), encoding='utf-8')
        print(json.dumps({'stopped': report.get('stopped'), 'cleanup': report['cleanup']}), flush=True)
    return 1 if report.get('stopped') else 0


if __name__ == '__main__':
    raise SystemExit(main())
