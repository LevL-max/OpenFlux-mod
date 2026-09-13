# OpenFlux-mod DevOps model

## Repository role

`LevL-max/OpenFlux-mod` is the private authoritative downstream repository for the production OpenFlux fork.

Public upstream remains `p1neappleXpress/OpenFlux` and is treated as an input for review and selective reuse, not as the production source of truth.

## Branch model

- `main` - stable downstream development trunk after bootstrap validation.
- `canonical-v4` - immutable bootstrap reference pointing at the exact production v4 source commit imported from AWS.
- `upstream-main` - tracking branch for reviewed public upstream state.
- `feature/*` - private development work.
- `backport/*` - selective upstream integrations.
- `test/*` - isolated experiments such as new transports.

## Flow

```text
p1neappleXpress/OpenFlux
        |
        | monitor / compare / assess
        v
upstream-main
        |
        | selective backport or feature work
        v
feature/* or backport/*
        |
        | PR + CI
        v
main
        |
        | approved tag/release
        v
GitHub Actions build artifacts
        |
        +--> AWS production deploy pipeline
        |
        +--> approved Mini-PC client installer/release
```

## Upstream policy

Upstream changes are never deployed automatically. Every upstream change is classified and either ignored, watched, selectively backported, or tested in isolation.

The existing production lifecycle remains conceptually:

`status -> check -> assess -> explicit stage -> explicit deploy`

GitHub becomes the source/build/release control plane. AWS remains the deployment safety gate during transition and then becomes a production consumer of approved private GitHub releases.

## Build/release model

GitHub Actions builds Linux amd64 from the exact downstream commit and records:

- commit SHA
- Go version
- binary SHA256
- build artifact

Approved releases should contain immutable versioned artifacts, checksums, and release metadata. Production hosts must consume an explicit approved release/tag, not a mutable branch head.

## Mini-PC distribution

Mini-PCs must never independently follow public upstream. They consume only approved downstream artifacts. Direct upstream-to-client updates are prohibited.

Private repository access for deployment should use minimum-scope read-only credentials. Personal write-capable GitHub credentials must not be stored on production clients.

## Production safety

Repository/bootstrap operations do not imply deployment. No GitHub merge, tag, CI build, or upstream update changes production until the explicit deployment path passes staging, health gates, and rollback safeguards.
