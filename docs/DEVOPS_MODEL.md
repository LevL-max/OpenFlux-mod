# OpenFlux-mod DevOps model

## Goal

`LevL-max/OpenFlux-mod` is the private downstream source of truth for our OpenFlux variant.

Public upstream remains:

`p1neappleXpress/OpenFlux`

We follow upstream for useful changes, but our repository owns integration, testing, release and deployment decisions.

## Source model

```text
p1neappleXpress/OpenFlux
        |
        | review / compare / selected backport
        v
LevL-max/OpenFlux-mod
        |
        | PR / CI / explicit approval
        v
GitHub Release
        |
        +--> AWS exit node
        +--> Mini-PC client package / installer
```

No machine follows upstream directly.

No upstream commit is deployed merely because it is newer.

## Branch model

Recommended steady-state branches:

- `main` - our approved downstream trunk
- `upstream-main` - tracking branch for the latest reviewed public upstream state
- short-lived feature/backport branches - one change or integration at a time

Current `bootstrap` branch exists only for repository migration and documentation before the exact AWS Git history is imported.

## CI

Every pull request and every change to our trunk should run at minimum:

- `go test ./...`
- Linux amd64 build
- SHA256 generation
- build metadata with commit and Go version

The build produced by CI is a candidate artifact, not automatically a production deployment.

## Release model

Production installation should come from an explicit GitHub release/tag, never directly from a moving branch such as `main`.

A release should contain at minimum:

- `openflux-linux-amd64`
- `SHA256SUMS`
- `BUILD-METADATA`
- release notes / Delta reference

Future release naming should be deliberate and versioned. Existing historical tags `yandex-working-v1` through `yandex-working-v4` are preserved as migration history.

## Deployment model

### AWS

AWS remains the first production approval/deployment point.

Target lifecycle:

```text
GitHub release
  -> download explicit version
  -> verify SHA / metadata
  -> isolated test
  -> stage
  -> explicit deploy
  -> health gate
  -> automatic rollback on failure
```

The current `/usr/local/sbin/openflux-update` safety model remains valid and should be adapted to consume an approved GitHub release artifact rather than a locally rebuilt moving upstream tree.

### Mini-PC clients

Mini-PCs must not independently follow upstream.

They should consume only a release already approved for the private fork. Distribution may be either:

1. a GitHub release asset downloaded with read-only repository credentials, or
2. a self-contained installer generated from the approved GitHub release and copied by the management path.

The exact credential/distribution choice is separate from source control and must not reintroduce a Mini-PC -> AWS control dependency.

## Private repository access

The repository remains PRIVATE.

Do not commit:

- Yandex session JSON
- live JWT/session tokens
- private keys
- GitHub tokens
- AWS credentials
- machine-specific secrets

Runtime URLs/config defaults that are not secrets may be versioned when appropriate, but live operational state stays outside Git.

## Upstream policy

Upstream monitoring answers:

1. What changed since the last reviewed upstream cursor?
2. Is it relevant to our Linux/Yandex/private-fork path?
3. Do we already have equivalent behavior?
4. Is integration clean or manual?
5. Is there enough value to test it?

Possible outcomes:

- `ACTION / HIGH VALUE`
- `REVIEW / POTENTIALLY USEFUL`
- `WATCH`
- `IGNORE FOR OUR DEPLOYMENT`
- `ALREADY BACKPORTED`

Selected changes are integrated into our branch, tested in our CI, and released under our versioning. We do not deploy the public project's release binary directly to production.

## Production principle

GitHub becomes the source of truth only after the exact current AWS v4 history has been imported and validated.

Until then the AWS canonical tree at `/opt/openflux/src/openflux-yandex` remains authoritative.
