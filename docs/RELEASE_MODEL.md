# Release model

## Purpose

`LevL-max/OpenFlux-mod` is the authoritative private downstream source repository. Public upstream remains review input only.

The release channel is intentionally separate from production deployment. Creating a GitHub release must never restart or modify AWS, Mini-PC1 or Mini-PC2.

## Current functional baseline

The first release candidate keeps the proven private Yandex v4 line as-is:

- canonical private ancestor: `22f29ab94ca2f5aa5b85956053b5c4cab1d1e31e`
- dynamic public Yandex document bootstrap
- signed Yandex editor identity
- dynamic OnlyOffice build detection
- current authentication flow
- private `4 packets / 1 ms` batching profile
- current reconnect, panic-containment and parser hardening

Volga, CUPS and other experimental upstream transports are not promoted into the release candidate. They remain review/test material only unless explicitly approved later.

## Release assets

Each approved release candidate should produce:

- `openflux-linux-amd64` - native Linux amd64 binary, primarily for client-side packaging and controlled testing
- `openflux-container-linux-amd64.tar.gz` - Docker image archive for the exit-node runtime
- `SHA256SUMS` - hashes of immutable release files
- `BUILD-METADATA` - source commit, Go version, build flags and container digest
- private GHCR image tagged by release version and exact source SHA

No `latest` container tag is published. Releases use immutable version/SHA identifiers only.

## Container policy

The exit node is containerized. The image contains only the runtime binary and required Linux runtime/network utilities. It does not contain Yandex session data, private keys, `.env` files or deployment credentials.

Production runtime isolation remains external to the image and must preserve the existing model:

- Docker bridge networking, not host networking
- no public container ports
- only the capabilities required by the existing exit-node runtime (`NET_ADMIN` and `NET_RAW`)
- host-wide RST suppression is prohibited
- any RST handling stays inside the container/network namespace
- Xray, x-ui, nginx, AmneziaWG and unrelated services remain untouched

## Release flow

1. Develop/review changes in the private repository.
2. CI runs tests and container build/smoke validation.
3. A version change in `release/VERSION` creates a **draft prerelease** only after the branch is merged to `main`.
4. The workflow builds a static Linux amd64 binary, packages the same binary into the runtime container, pushes immutable GHCR tags, writes checksums/metadata and creates the draft GitHub release.
5. No deployment happens automatically.
6. The release asset/container is tested in isolation.
7. Only after explicit approval is the AWS updater adapted to consume the approved GitHub release while retaining checkpoint, stage, health-gate and rollback behavior.
8. Mini-PC distribution is adapted afterward and continues to consume only an explicitly approved downstream release, never public upstream directly.

## Security rule

Runtime configuration is injected at deployment/startup. Secrets and temporary Yandex session/auth data must never be committed to Git, embedded into the release binary or baked into the container image.

## Current release candidate

`v4.0.0-rc1`

Status: preparation and CI validation only. Production remains unchanged.
