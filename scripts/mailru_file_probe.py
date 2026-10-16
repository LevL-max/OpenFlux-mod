#!/usr/bin/env python3
"""Bounded, opt-in file API experiment; JSON commands on stdin, safe JSON output.

No service/config changes. Only random bytes and uniquely named probe objects.
Credentials stay in memory on the node. Does not publish files or retry failures.
Run with --profile PATH --nonce UUIDHEX. Send init, put, get, remove, close.
"""
import argparse
import hashlib
import http.cookiejar
import json
import os
import re
import signal
import sys
import time
import urllib.error
import urllib.parse
import urllib.request


class Probe:
    def __init__(self, profile, nonce):
        if not re.fullmatch(r'[a-f0-9]{32}', nonce):
            raise ValueError('invalid nonce')
        self.prefix = '/openflux-probe-' + nonce + '-'
        self.created = set()
        self.token = None
        with open(profile) as source:
            p = json.load(source)
        jar = http.cookiejar.CookieJar()
        for name, value in p.get('cookies', {}).items():
            jar.set_cookie(http.cookiejar.Cookie(
                0, name, value, None, False, 'cloud.mail.ru', False,
                False, '/', True, True, None, True, None, None, {}))
        self.opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar))
        self.headers = {'User-Agent': p.get('headers', {}).get('user-agent', 'Mozilla/5.0'),
                        'Referer': 'https://cloud.mail.ru/home/'}

    def request(self, url, data=None, method=None, headers=None):
        parsed = urllib.parse.urlsplit(url)
        if parsed.scheme != 'https' or not (parsed.hostname == 'cloud.mail.ru' or
                                           parsed.hostname.endswith('.cloud.mail.ru')):
            raise ValueError('non-cloud endpoint')
        req = urllib.request.Request(url, data=data, method=method,
                                     headers=dict(self.headers, **(headers or {})))
        return self.opener.open(req, timeout=20)

    def api(self, endpoint, params=None, post=False):
        data = urllib.parse.urlencode(dict(params or {}, token=self.token, api=2)).encode()
        url = 'https://cloud.mail.ru/api/v2/' + endpoint
        with self.request(url if post else url + '?' + data.decode(),
                          data if post else None,
                          headers={'X-Csrf-Token': self.token,
                                   'Origin': 'https://cloud.mail.ru',
                                   'Content-Type': 'application/x-www-form-urlencoded'}) as r:
            result = json.load(r)
        if result.get('status') != 200:
            raise ValueError('API status ' + str(result.get('status')))
        return result

    def init(self):
        with self.request('https://cloud.mail.ru/home/') as r:
            page = r.read(4 * 1024 * 1024).decode(errors='replace')
        for match in re.finditer(r'"tokens"\s*:\s*', page):
            try:
                self.token = json.JSONDecoder().raw_decode(page[match.end():])[0].get('csrf')
            except (ValueError, AttributeError):
                pass
            if self.token:
                break
        if not self.token:
            raise ValueError('no CSRF')
        folder = self.api('folder', {'home': '/', 'limit': 0})
        email = folder.get('email')
        if not isinstance(email, str) or not email:
            raise ValueError('no account identity')
        self.email = email
        self.dispatcher = self.api('dispatcher')['body']
        return {'account_tag': hashlib.sha256((self.prefix + email.lower()).encode()).hexdigest(),
                'upload_available': bool(self.dispatcher.get('upload')),
                'download_available': bool(self.dispatcher.get('get'))}

    def path(self, name):
        if not re.fullmatch(r'(aws|pc2)-(small|large)\.bin', name):
            raise ValueError('not a probe name')
        return self.prefix + name

    def put(self, name, size):
        path = self.path(name)
        if size not in (65536, 16777216):
            raise ValueError('unsupported size')
        data = os.urandom(size)
        digest = hashlib.sha256(data).hexdigest()
        url = self.dispatcher['upload'][0]['url']
        url += ('&' if '?' in url else '?') + urllib.parse.urlencode(
            {'cloud_domain': 2, 'x-email': self.email})
        start = time.monotonic()
        with self.request(url, data, method='PUT', headers={'Content-Type': 'application/octet-stream'}) as r:
            body = r.read(4096).decode().strip()
        uploaded = time.monotonic()
        # The web uploader returns a content hash, optionally followed by size.
        match = re.fullmatch(r'([a-fA-F0-9]{40})(?:[;\s]+\d+)?', body)
        if not match:
            raise ValueError('unexpected upload response')
        # Track before registration so a lost response still gets cleanup.
        self.created.add(path)
        self.api('file/add', {'home': path, 'hash': match[1], 'size': size,
                              'conflict': 'strict'}, post=True)
        end = time.monotonic()
        return {'size': size, 'sha256': digest, 'upload_s': uploaded-start,
                'register_s': end-uploaded, 'put_s': end-start,
                'upload_Bps': size/(uploaded-start)}

    def get(self, name, size, sha256):
        path = self.path(name)
        start = time.monotonic()
        meta = self.api('file', {'home': path})['body']
        visible = time.monotonic()
        if meta.get('size') != size:
            raise ValueError('metadata size mismatch')
        url = self.dispatcher['get'][0]['url'].rstrip('/') + '/' + urllib.parse.quote(path.lstrip('/'))
        digest = hashlib.sha256()
        received = 0
        first = None
        with self.request(url) as r:
            http = r.status
            while True:
                chunk = r.read(65536)
                if not chunk:
                    break
                if first is None:
                    first = time.monotonic()
                received += len(chunk)
                if received > size:
                    raise ValueError('oversized response')
                digest.update(chunk)
        end = time.monotonic()
        ok = received == size and digest.hexdigest() == sha256
        return {'http': http, 'size': received, 'verified': ok,
                'lookup_s': visible-start, 'first_block_s': first-start if first else None,
                'download_s': end-visible, 'get_s': end-start,
                'download_Bps': received/(end-visible)}

    def remove(self, name):
        path = self.path(name)
        if path not in self.created:
            raise ValueError('not created by this worker')
        self.api('file/remove', {'home': path}, post=True)
        self.created.remove(path)
        return {'removed': True}

    def cleanup(self):
        failures = 0
        for path in list(self.created):
            try:
                self.api('file/remove', {'home': path}, post=True)
                self.created.remove(path)
            except Exception:
                failures += 1
        return {'remaining_objects': len(self.created), 'cleanup_failures': failures}


def timeout_handler(*_):
    raise TimeoutError('operation deadline')


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--profile', required=True)
    parser.add_argument('--nonce', required=True)
    args = parser.parse_args()
    signal.signal(signal.SIGALRM, timeout_handler)
    probe = Probe(args.profile, args.nonce)
    try:
        for line in sys.stdin:
            try:
                command = json.loads(line)
                operation = command.pop('op')
                if operation == 'close':
                    break
                if operation not in ('init', 'put', 'get', 'remove'):
                    raise ValueError('invalid operation')
                signal.alarm(35)
                result = getattr(probe, operation)(**command)
            except Exception as error:
                result = {'error_type': type(error).__name__}
                if isinstance(error, urllib.error.HTTPError):
                    result['http'] = error.code
            finally:
                signal.alarm(0)
            print(json.dumps(result), flush=True)
    finally:
        signal.alarm(30)
        print(json.dumps(probe.cleanup()), flush=True)


if __name__ == '__main__':
    main()
