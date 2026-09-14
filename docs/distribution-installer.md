# OpenFlux distribution installer v1

This document defines the first distribution/install layer around the validated Yandex RC2 runtime. It deliberately does **not** add Volga/vyandex yet. The installer interface is transport-neutral so future transports can be added without redesigning deployment.

## Goals

- One installer/controller for both roles: `client` and `exit`.
- Anonymous installation from public release assets. No PAT, `docker login`, or GitHub account is required on a target host.
- Exact release artifact download with SHA256 verification before `docker load`.
- Fail-safe lifecycle with health gates and automatic rollback.
- OpenFlux owns only objects it creates and refuses to overwrite unrelated containers, networks, listeners, configs, or images.
- No host-wide firewall, DNS, or routing modifications in the core installer.
- Preserve the validated Yandex runtime profile:
  - queue `1024`
  - compression enabled
  - TCP buffers `1048576 / 4194304`
  - batching `4 packets / 1000 us`
  - existing downstream reconnect/backoff profile unchanged

## Current support matrix

| Platform | Role | Backend | v1 state |
| --- | --- | --- | --- |
| Ubuntu/Debian linux/amd64 + systemd | client | container | implemented |
| Ubuntu/Debian linux/amd64 + systemd | exit | container | implemented |
| OpenWRT | client | native | detected, not enabled yet |
| Entware | client | native | detected, not enabled yet |
| linux/arm64 | client | container/native | release artifact not built yet |
| linux/armv7 | client | native | not validated yet |
| mips/mipsle | client | native | not validated yet |

The installer must fail explicitly on an unsupported combination rather than pretending the platform is supported.

## Runtime model

### Client

```text
host application
    |
127.0.0.1:11080
    |
Docker localhost publish
    |
openflux-client container
    |
Yandex transport
```

The core client exposes SOCKS only on localhost. Transparent routing, TUN, Mihomo, sing-box, router mode integration, DNS, and policy routing are intentionally outside the generic client installer.

Future router/system integration is an optional adapter above this stable localhost SOCKS interface.

### Exit

```text
openflux-exit container
    |
isolated Docker bridge
    |
Yandex transport / Internet
```

The exit container publishes no host ports. It receives only the required `NET_ADMIN` and `NET_RAW` capabilities. TCP RST suppression is installed inside the exit container namespace only. The installer never adds a host-wide RST drop rule.

## Commands

Initial install:

```bash
sudo openflux-install install client --url <public-yandex-document-url>
sudo openflux-install install exit --url <public-yandex-document-url>
```

Planning:

```bash
sudo openflux-install preflight client
sudo openflux-install install client --url <url> --dry-run
```

Lifecycle after install:

```bash
sudo openfluxctl status
sudo openfluxctl test
sudo openfluxctl logs
sudo openfluxctl start
sudo openfluxctl stop
sudo openfluxctl restart
sudo openfluxctl update --version <version>
sudo openfluxctl rollback
sudo openfluxctl set-url <url>
sudo openfluxctl set-transport yandex
sudo openfluxctl uninstall
```

## Fail-safe rules

1. Existing non-OpenFlux container names are never removed or replaced.
2. Existing non-OpenFlux Docker network names are never removed or replaced.
3. Client install refuses a busy SOCKS port instead of killing the listener.
4. Docker subnet candidates are checked against host routes and existing Docker subnets before network creation.
5. Configuration is stored separately from the container image under `/etc/openflux` and survives runtime updates.
6. The currently working config is copied to rollback state before update, URL change, or transport change.
7. A new runtime is not accepted until its health gate passes.
8. Failed update/config change restores the previous config and starts the previous known-good image again.
9. Failed first install removes only objects created by that install attempt.
10. Uninstall removes only managed containers/networks and image tags recorded by the installer.
11. Docker or prerequisite packages installed by the installer are host prerequisites and are not automatically removed on uninstall.

## Health gates

Exit health requires:

- managed container is running;
- Yandex/OnlyOffice authentication succeeds within the startup deadline.

Client health requires:

- managed container is running;
- Yandex/OnlyOffice authentication succeeds;
- a real HTTPS request succeeds through `127.0.0.1:<SOCKS_PORT>`.

## Transport abstraction

The config already stores `TRANSPORT`. Installer v1 accepts `yandex` only. `vyandex` is reserved but rejected with an explicit message until Volga has its own isolated implementation and runtime validation.

Volga should be added after the installer lifecycle has been validated on clean Ubuntu hosts using the known-good Yandex RC2 baseline.

## Embedded Linux plan

OpenWRT/Entware should use a native backend rather than making Docker mandatory. The user-facing lifecycle should remain the same (`openfluxctl status/test/update/rollback/uninstall`), while service integration differs:

- OpenWRT: native static binary + `procd`.
- Entware: native static binary + `/opt/etc/init.d` service.

No native backend is advertised as supported until the required architecture artifacts and runtime tests exist.
