#!/usr/bin/env python3
"""Single-TCP-connection SHA-256 probe; no HTTP retry or resume.

Run origin only on a private address reachable from the exit container.
This script does not change services, routes, firewall rules or credentials.
"""
import argparse
import hashlib
import http.client
import http.server
import json
import random
import socket
import struct
import time
import urllib.parse

CHUNK = random.Random(20261011).randbytes(1 << 20)


def digest_for(size):
    digest = hashlib.sha256()
    while size:
        part = CHUNK[:min(size, len(CHUNK))]
        digest.update(part)
        size -= len(part)
    return digest.hexdigest()


def read_exact(sock, size):
    data = b''
    while len(data) < size:
        part = sock.recv(size - len(data))
        if not part:
            raise EOFError('SOCKS connection closed')
        data += part
    return data


class Origin(http.server.BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'

    def log_message(self, *args):
        pass

    def do_GET(self):
        query = urllib.parse.parse_qs(urllib.parse.urlsplit(self.path).query)
        size = int(query.get('n', ['1048576'])[0])
        rate = int(query.get('rate', ['0'])[0])
        if not 0 < size <= 512 << 20 or rate < 0:
            self.send_error(400)
            return
        self.connection.settimeout(600)
        self.send_response(200)
        self.send_header('Content-Length', str(size))
        self.end_headers()
        sent, start = 0, time.monotonic()
        try:
            while sent < size:
                part = CHUNK[:min(size - sent, len(CHUNK))]
                self.wfile.write(part)
                self.wfile.flush()
                sent += len(part)
                if rate:
                    time.sleep(max(0, sent / rate - (time.monotonic() - start)))
        except (OSError, TimeoutError):
            pass

    def do_POST(self):
        size = int(self.headers.get('Content-Length', '0'))
        if not 0 < size <= 512 << 20:
            self.send_error(400)
            return
        self.connection.settimeout(600)
        digest, received = hashlib.sha256(), 0
        while received < size:
            part = self.rfile.read(min(65536, size - received))
            if not part:
                return
            received += len(part)
            digest.update(part)
        payload = json.dumps({'bytes': received, 'sha256': digest.hexdigest()}).encode()
        self.send_response(200)
        self.send_header('Content-Length', str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)


def probe(args, direction):
    class Connection(http.client.HTTPConnection):
        def connect(self):
            sock = socket.create_connection((args.socks_host, args.socks_port), args.timeout)
            self.sock = sock
            sock.sendall(b'\x05\x01\x00')
            if read_exact(sock, 2) != b'\x05\x00':
                raise RuntimeError('SOCKS authentication failed')
            sock.sendall(b'\x05\x01\x00\x01' + socket.inet_aton(self.host) + struct.pack('!H', self.port))
            reply = read_exact(sock, 4)
            if reply[1] != 0:
                raise RuntimeError('SOCKS connect status ' + str(reply[1]))
            size = {1: 4, 4: 16}.get(reply[3])
            if reply[3] == 3:
                size = read_exact(sock, 1)[0]
            if size is None:
                raise RuntimeError('Invalid SOCKS address type')
            read_exact(sock, size + 2)

    size, rate = args.mib << 20, args.rate_kib << 10
    conn = Connection(args.host, args.port, timeout=args.timeout)
    start, transferred = time.monotonic(), 0
    row = dict(direction=direction, requested=size, rate_limit_Bps=rate,
               connections=1, started_at=time.time(), integrity=False)
    try:
        if direction == 'download':
            conn.request('GET', '/download?n=' + str(size) + '&rate=' + str(rate))
            response = conn.getresponse()
            if response.status != 200:
                raise RuntimeError('HTTP status ' + str(response.status))
            digest = hashlib.sha256()
            while transferred < size:
                part = response.read(min(65536, size - transferred))
                if not part:
                    raise EOFError('Incomplete HTTP body')
                transferred += len(part)
                digest.update(part)
            row.update(sha256=digest.hexdigest(), bytes=transferred)
        else:
            conn.putrequest('POST', '/upload')
            conn.putheader('Content-Length', str(size))
            conn.endheaders()
            while transferred < size:
                part = CHUNK[:min(size - transferred, len(CHUNK))]
                conn.send(part)
                transferred += len(part)
                if rate:
                    time.sleep(max(0, transferred / rate - (time.monotonic() - start)))
            response = conn.getresponse()
            if response.status != 200:
                raise RuntimeError('HTTP status ' + str(response.status))
            reply = json.loads(response.read())
            row.update(sha256=reply['sha256'], bytes=reply['bytes'])
        row['integrity'] = row['bytes'] == size and row['sha256'] == digest_for(size)
    except Exception as exc:
        row['error'] = type(exc).__name__ + ': ' + str(exc)
    finally:
        conn.close()
    elapsed = time.monotonic() - start
    # Bytes accepted by the local socket are NOT proof of an upload reaching the origin.
    row.update(socket_bytes=transferred, seconds=round(elapsed, 3), finished_at=time.time(),
               mbps=round(size * 8 / elapsed / 1e6, 3) if row['integrity'] else None)
    print(json.dumps(row), flush=True)
    return row['integrity']


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('mode', choices=('origin', 'client'))
    parser.add_argument('--host', required=True, help='Private IPv4 origin bind/target address')
    parser.add_argument('--port', type=int, default=18767)
    parser.add_argument('--socks-host', default='127.0.0.1')
    parser.add_argument('--socks-port', type=int, default=18788)
    parser.add_argument('--mib', type=int, default=16)
    parser.add_argument('--rate-kib', type=int, default=0)
    parser.add_argument('--timeout', type=float, default=600)
    parser.add_argument('--direction', choices=('download', 'upload', 'both'), default='both')
    args = parser.parse_args()
    if not 1 <= args.mib <= 512 or args.rate_kib < 0 or args.timeout <= 0:
        parser.error('Invalid size, rate or timeout')
    if args.mode == 'origin':
        http.server.ThreadingHTTPServer((args.host, args.port), Origin).serve_forever()
    else:
        directions = ('download', 'upload') if args.direction == 'both' else (args.direction,)
        return 0 if all(probe(args, direction) for direction in directions) else 1


if __name__ == '__main__':
    raise SystemExit(main())
