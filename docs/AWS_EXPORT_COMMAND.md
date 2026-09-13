# Safe AWS canonical source export

This is a read-only export step. It does not restart or modify production.

Run on the AWS host only when source import is required:

```bash
cd /opt/openflux/src/openflux-yandex && \
  test "$(git rev-parse HEAD)" = "22f29ab94ca2f5aa5b85956053b5c4cab1d1e31e" && \
  test -z "$(git status --porcelain)" && \
  git bundle create ~/openflux-v4-canonical.bundle --all && \
  sha256sum ~/openflux-v4-canonical.bundle
```

Expected precondition:

- HEAD equals production v4 commit
- working tree is clean

Output file:

`~/openflux-v4-canonical.bundle`

The bundle contains Git history and refs. It contains source history, not the live `/etc/openflux/yandex.env` runtime file or Yandex session JSON.

Production service is untouched.
