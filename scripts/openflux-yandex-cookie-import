#!/usr/bin/env python3
"""Import Chrome/Chromium "Copy as cURL (bash)" cookies into an OpenFlux cookie store.

Cookie values are never printed. The store is replaced atomically with mode 0600.
"""

import argparse
import json
import os
import shlex
import tempfile


def parse_args():
    p = argparse.ArgumentParser(description="Import browser cookies into OpenFlux Yandex cookie store")
    p.add_argument("--store", required=True, help="OpenFlux --yandex-cookie-store JSON path")
    p.add_argument("--url", required=True, help="Exact document URL used by OpenFlux as the store key")
    p.add_argument("--curl-file", required=True, help="File containing Chrome 'Copy as cURL (bash)' output")
    return p.parse_args()


def extract_cookie_header(text):
    # Chrome emits backslash-newline continuations in Copy as cURL (bash).
    text = text.replace("\\\r\n", " ").replace("\\\n", " ")
    argv = shlex.split(text, posix=True)

    cookie = None
    i = 0
    while i < len(argv):
        arg = argv[i]
        if arg in ("-b", "--cookie") and i + 1 < len(argv):
            cookie = argv[i + 1].strip()
            i += 2
            continue
        if arg in ("-H", "--header") and i + 1 < len(argv):
            header = argv[i + 1]
            if header.lower().startswith("cookie:"):
                cookie = header.split(":", 1)[1].strip()
            i += 2
            continue
        i += 1

    if not cookie:
        raise SystemExit("ERROR: no Cookie header/-b value found in cURL input")
    return cookie


def parse_cookie_map(header):
    out = {}
    for part in header.split(";"):
        part = part.strip()
        if not part or "=" not in part:
            continue
        name, value = part.split("=", 1)
        name = name.strip()
        if name:
            out[name] = value
    if not out:
        raise SystemExit("ERROR: Cookie header contained no usable cookies")
    return out


def load_store(path):
    try:
        with open(path, "r", encoding="utf-8") as f:
            data = json.load(f)
    except FileNotFoundError:
        return {}
    except json.JSONDecodeError as e:
        raise SystemExit(f"ERROR: existing store is invalid JSON: {e}")
    if not isinstance(data, dict):
        raise SystemExit("ERROR: existing store root must be a JSON object")
    return data


def atomic_save(path, data):
    directory = os.path.dirname(os.path.abspath(path))
    os.makedirs(directory, mode=0o700, exist_ok=True)

    fd, tmp = tempfile.mkstemp(prefix=".openflux-cookie-", dir=directory)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as f:
            json.dump(data, f, indent=2, sort_keys=True)
            f.write("\n")
            f.flush()
            os.fsync(f.fileno())
        os.chmod(tmp, 0o600)
        os.replace(tmp, path)
        os.chmod(path, 0o600)
    except Exception:
        try:
            os.unlink(tmp)
        except FileNotFoundError:
            pass
        raise


def main():
    args = parse_args()
    with open(args.curl_file, "r", encoding="utf-8") as f:
        cookie_header = extract_cookie_header(f.read())

    cookies = parse_cookie_map(cookie_header)
    store = load_store(args.store)
    store[args.url] = cookies
    atomic_save(args.store, store)

    print(f"COOKIE_IMPORT=PASS count={len(cookies)}")
    print(f"STORE={args.store}")
    print("Cookie values were not printed.")


if __name__ == "__main__":
    main()
