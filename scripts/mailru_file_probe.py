#!/usr/bin/env python3
"""Bounded, opt-in file API experiment; JSON commands on stdin, safe JSON output.

No service/config changes. Only random bytes and uniquely named probe objects.
Credentials stay in memory on the node. Does not publish files or retry failures.
Run with --profile PATH --nonce UUIDHEX. Send init, put, get, remove, close.
"""
import argparse
import concurrent.futures
import hashlib
import http.client
import http.cookiejar
import json
import os
import re
import signal
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request


class Probe:
    def __init__(self, profile, nonce, keepalive=False):
        if not re.fullmatch(r'[a-f0-9]{32}', nonce):
            raise ValueError('invalid nonce')
        self.prefix = '/openflux-probe-' + nonce + '-'
        self.created = set()
        self.token = None
        self.keepalive = keepalive
        self.connections = threading.local()
        with open(profile) as source:
            p = json.load(source)
        jar = http.cookiejar.CookieJar()
        for name, value in p.get('cookies', {}).items():
            jar.set_cookie(http.cookiejar.Cookie(
                0, name, value, None, False, 'cloud.mail.ru', False,
                False, '/', True, True, None, True, None, None, {}))
        self.jar = jar
        self.opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar))
        self.headers = {'User-Agent': p.get('headers', {}).get('user-agent', 'Mozilla/5.0'),
                        'Referer': 'https://cloud.mail.ru/home/'}

    def request(self, url, data=None, method=None, headers=None, redirects=0):
        parsed = urllib.parse.urlsplit(url)
        if parsed.scheme != 'https' or not (parsed.hostname == 'cloud.mail.ru' or
                                           parsed.hostname.endswith('.cloud.mail.ru') or
                                           (data is None and parsed.hostname.endswith('.datacloudmail.ru'))):
            raise ValueError('non-cloud endpoint')
        req = urllib.request.Request(url, data=data, method=method,
                                     headers=dict(self.headers, **(headers or {})))
        if self.keepalive:
            self.jar.add_cookie_header(req)
            pool = getattr(self.connections, 'pool', None)
            if pool is None:
                pool = self.connections.pool = {}
            connection = pool.get(parsed.netloc)
            if connection is None:
                connection = pool[parsed.netloc] = http.client.HTTPSConnection(parsed.netloc, timeout=20)
            request_headers = dict(req.header_items())
            if data is not None and 'Content-type' not in request_headers:
                request_headers['Content-Type'] = 'application/x-www-form-urlencoded'
            try:
                connection.request(req.get_method(), urllib.parse.urlunsplit(('', '', parsed.path or '/', parsed.query, '')),
                                   body=data, headers=request_headers)
                response = connection.getresponse()
                self.jar.extract_cookies(response, req)
                if response.status in (301, 302, 303, 307, 308) and req.get_method() == 'GET' and redirects < 3:
                    target = urllib.parse.urljoin(url, response.getheader('Location', ''))
                    response.close()
                    connection.close()
                    forwarded = dict(headers or {})
                    if urllib.parse.urlsplit(target).netloc != parsed.netloc:
                        forwarded.pop('X-Csrf-Token', None)
                    # Revalidate HTTPS and cloud domain, and construct cookies
                    # for the destination rather than forwarding a Cookie header.
                    return self.request(target, headers=forwarded, redirects=redirects+1)
                if response.status >= 300:
                    status = response.status
                    response.close()
                    connection.close()
                    raise urllib.error.HTTPError(url, status, 'HTTP status', {}, None)
                return response
            except Exception:
                connection.close()
                raise
        return self.opener.open(req, timeout=20)

    def close_connections(self):
        for connection in getattr(self.connections, 'pool', {}).values():
            connection.close()
        self.connections.pool = {}

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
        if not re.fullmatch(r'(aws|pc2)-(small|large|batch[0-9]{1,2}-[0-9]{1,2})\.bin', name):
            raise ValueError('not a probe name')
        return self.prefix + name

    def put(self, name, size):
        path = self.path(name)
        if size not in (65536, 2097152, 16777216):
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

    def get(self, name, size, sha256=None, discover=False, stop=None):
        path = self.path(name)
        start = time.monotonic()
        polls = 0
        while True:
            if stop is not None and stop.is_set():
                raise InterruptedError('batch stopped')
            polls += 1
            try:
                meta = self.api('file', {'home': path})['body']
                break
            except urllib.error.HTTPError as error:
                if not discover or error.code != 404 or time.monotonic()-start > 25:
                    raise
                time.sleep(0.2)
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
        actual_hash = digest.hexdigest()
        ok = received == size and actual_hash == sha256 if sha256 else None
        return {'http': http, 'size': received, 'verified': ok,
                'sha256': actual_hash, 'discovery_polls': polls,
                'lookup_s': visible-start, 'first_block_s': first-start if first else None,
                'download_s': end-visible, 'get_s': end-start,
                'download_Bps': received/(end-visible)}

    def batch(self, direction, origin, batch_id, count, lanes, size):
        if direction not in ('put', 'get') or origin not in ('aws', 'pc2'):
            raise ValueError('invalid batch')
        if lanes not in (1, 4, 6) or count not in (1, 12) or not 0 <= batch_id <= 99:
            raise ValueError('invalid batch bounds')
        if size not in (65536, 2097152):
            raise ValueError('invalid block size')
        stop = threading.Event()
        started = time.monotonic()
        def lane(number):
            rows = []
            try:
                for index in range(number, count, lanes):
                    if stop.is_set():
                        break
                    name = f'{origin}-batch{batch_id}-{index}.bin'
                    begin = time.monotonic()
                    try:
                        if direction == 'put':
                            result = self.put(name, size)
                        else:
                            result = self.get(name, size, discover=True, stop=stop)
                    except Exception as error:
                        stop.set()
                        result = {'error_type': type(error).__name__}
                        if isinstance(error, urllib.error.HTTPError):
                            result['http'] = error.code
                    rows.append(dict(result, index=index, begin_s=begin-started,
                                     completed_s=time.monotonic()-started))
            finally:
                self.close_connections()
            return rows
        with concurrent.futures.ThreadPoolExecutor(max_workers=lanes) as pool:
            rows = [row for group in pool.map(lane, range(lanes)) for row in group]
        return {'elapsed_s': time.monotonic()-started, 'rows': sorted(rows, key=lambda row: row['index']),
                'failed': stop.is_set(), 'keepalive': self.keepalive}

    def clear_batch(self, origin, batch_id, count):
        if origin not in ('aws', 'pc2') or not 0 <= batch_id <= 99 or count not in (1, 12):
            raise ValueError('invalid cleanup bounds')
        removed = 0
        for index in range(count):
            name = f'{origin}-batch{batch_id}-{index}.bin'
            if self.path(name) in self.created:
                self.remove(name)
                removed += 1
        return {'removed': removed, 'remaining_objects': len(self.created)}

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
    parser.add_argument('--keepalive', action='store_true')
    args = parser.parse_args()
    signal.signal(signal.SIGALRM, timeout_handler)
    probe = Probe(args.profile, args.nonce, args.keepalive)
    try:
        for line in sys.stdin:
            try:
                command = json.loads(line)
                operation = command.pop('op')
                if operation == 'close':
                    break
                if operation not in ('init', 'put', 'get', 'remove', 'batch', 'clear_batch'):
                    raise ValueError('invalid operation')
                signal.alarm(90 if operation == 'batch' else 35)
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
