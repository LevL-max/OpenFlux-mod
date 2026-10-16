#!/usr/bin/env python3
"""Bounded hybrid proof: existing chat SOCKS tunnel controls Cloud file delivery.

No general proxy or installed configuration is created. Server listens only on
loopback. Payloads are random; the Cloud objects use the file probe's unique
namespace. Run server and client with the same nonce and existing profiles.
"""
import argparse
import concurrent.futures
import ctypes
import json
import os
import socket
import statistics
import struct
import sys
import threading
import time

from mailru_file_probe import Probe


def exact(sock, size):
    data = bytearray()
    while len(data) < size:
        part = sock.recv(size-len(data))
        if not part:
            raise EOFError('control connection closed')
        data.extend(part)
    return bytes(data)


class Control:
    def __init__(self, sock):
        self.sock = sock
        self.lock = threading.Lock()

    def send(self, message):
        data = json.dumps(message, separators=(',', ':')).encode()
        if len(data) > 4096:
            raise ValueError('control message too large')
        with self.lock:
            self.sock.sendall(struct.pack('!I', len(data)) + data)

    def receive(self):
        size, = struct.unpack('!I', exact(self.sock, 4))
        if size > 4096:
            raise ValueError('control message too large')
        return json.loads(exact(self.sock, size))


def serve(probe, listener, nonce, result):
    try:
        sock, _ = listener.accept()
        sock.settimeout(35)
        with sock:
            control = Control(sock)
            if control.receive() != {'op': 'hello', 'nonce': nonce}:
                raise ValueError('wrong experiment')
            control.send({'op': 'hello'})
            while True:
                command = control.receive()
                if command['op'] == 'ping':
                    control.send({'op': 'pong', 'id': command['id']})
                elif command == {'op': 'download'}:
                    start = time.monotonic()
                    def upload(lane):
                        try:
                            for index in range(lane, 8, 4):
                                name = f'aws-batch90-{index}.bin'
                                uploaded = probe.put(name, 2097152)
                                control.send({'op': 'file', 'name': name, 'size': uploaded['size'],
                                              'sha256': uploaded['sha256']})
                        finally:
                            probe.close_connections()
                    with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
                        list(pool.map(upload, range(4)))
                    control.send({'op': 'uploaded', 'count': 8})
                    result['upload_batch_s'] = time.monotonic()-start
                    ack = control.receive()
                    if ack != {'op': 'verified', 'count': 8}:
                        raise ValueError('missing verified acknowledgement')
                    result['peer_verified'] = True
                    cleanup = probe.clear_batch('aws', 90, 12)
                    control.send({'op': 'cleaned', 'remaining': cleanup['remaining_objects']})
                elif command == {'op': 'close'}:
                    control.send({'op': 'closed'})
                    break
                else:
                    raise ValueError('unknown command')
    except Exception as error:
        result['error_type'] = type(error).__name__
    finally:
        listener.close()


def client(probe, port, nonce):
    sock = socket.create_connection(('127.0.0.1', 11080), timeout=25)
    with sock:
        sock.sendall(b'\x05\x01\x00')
        if exact(sock, 2) != b'\x05\x00':
            raise ValueError('SOCKS authentication')
        sock.sendall(b'\x05\x01\x00\x01\x7f\x00\x00\x01' + struct.pack('!H', port))
        header = exact(sock, 4)
        if header[:2] != b'\x05\x00':
            raise ValueError('SOCKS connect')
        address_size = {1: 4, 4: 16}.get(header[3])
        if header[3] == 3:
            address_size = exact(sock, 1)[0]
        if address_size is None:
            raise ValueError('SOCKS address')
        exact(sock, address_size+2)
        sock.settimeout(8)
        control = Control(sock)
        control.send({'op': 'hello', 'nonce': nonce})
        if control.receive() != {'op': 'hello'}:
            raise ValueError('control handshake')
        print(json.dumps({'stage': 'control_connected'}), flush=True)
        result = {}
        def pings():
            samples = []
            for index in range(12):
                start = time.monotonic()
                control.send({'op': 'ping', 'id': index})
                if control.receive() != {'op': 'pong', 'id': index}:
                    raise ValueError('echo mismatch')
                samples.append(time.monotonic()-start)
                time.sleep(0.5)
            return {'samples_s': samples, 'median_s': statistics.median(samples), 'max_s': max(samples)}
        result['before'] = pings()
        print(json.dumps({'stage': 'control_before', **result['before']}), flush=True)
        start = time.monotonic()
        control.send({'op': 'download'})
        received = []
        names = set()
        def download(message):
            try:
                row = probe.get(message['name'], message['size'], message['sha256'])
                if not row['verified']:
                    raise ValueError('hash mismatch')
                return row
            finally:
                probe.close_connections()
        with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
            while True:
                message = control.receive()
                if message == {'op': 'uploaded', 'count': 8}:
                    break
                if message.get('op') != 'file' or message.get('size') != 2097152:
                    raise ValueError('invalid file notice')
                if message['name'] in names or len(names) >= 8:
                    raise ValueError('duplicate notice')
                names.add(message['name'])
                received.append(pool.submit(download, message))
            rows = [future.result() for future in received]
        if len(rows) != 8:
            raise ValueError('missing file notice')
        elapsed = time.monotonic()-start
        result['download'] = {'bytes': 16777216, 'seconds': elapsed,
                              'Bps': 16777216/elapsed, 'verified': True}
        print(json.dumps({'stage': 'download', **result['download']}), flush=True)
        control.send({'op': 'verified', 'count': 8})
        sock.settimeout(30)
        if control.receive() != {'op': 'cleaned', 'remaining': 0}:
            raise ValueError('cleanup failed')
        sock.settimeout(8)
        result['after'] = pings()
        control.send({'op': 'close'})
        if control.receive() != {'op': 'closed'}:
            raise ValueError('close acknowledgement')
        return result


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--role', choices=('aws', 'pc2'), required=True)
    parser.add_argument('--nonce', required=True)
    parser.add_argument('--port', type=int, default=0)
    parser.add_argument('--netns-pid', type=int, default=0)
    args = parser.parse_args()
    probe = Probe('/var/lib/openflux-mailru-auth/browser.json', args.nonce, keepalive=True)
    result = {}
    try:
        probe.init()
        if args.role == 'aws':
            if args.netns_pid <= 0:
                raise ValueError('existing exit container namespace required')
            # Create only the listening socket in the exit container network.
            # Keep host filesystem, DNS and outgoing file API requests unchanged.
            original = os.open('/proc/self/ns/net', os.O_RDONLY)
            target = os.open(f'/proc/{args.netns_pid}/ns/net', os.O_RDONLY)
            libc = ctypes.CDLL(None, use_errno=True)
            try:
                if libc.setns(target, 0) != 0:
                    raise OSError('cannot enter exit network namespace')
                listener = socket.socket()
                listener.bind(('127.0.0.1', 0))
            finally:
                restored = libc.setns(original, 0)
                os.close(original)
                os.close(target)
                if restored != 0:
                    raise OSError('cannot restore probe network namespace')
            listener.listen(1)
            listener.settimeout(60)
            print(json.dumps({'ready': True, 'port': listener.getsockname()[1]}), flush=True)
            serve(probe, listener, args.nonce, result)
        else:
            result = client(probe, args.port, args.nonce)
    except Exception as error:
        result['error_type'] = type(error).__name__
        if type(error) is ValueError:
            result['reason'] = str(error)
    finally:
        result['cleanup'] = probe.cleanup()
        print(json.dumps({'stage': 'final', **result}), flush=True)
    return 1 if result.get('error_type') else 0


if __name__ == '__main__':
    raise SystemExit(main())
