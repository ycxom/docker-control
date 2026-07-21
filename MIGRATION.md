# Migration to generic docker-control v3

v3 removes framework-specific naming from new resources while preserving a narrow compatibility layer.

## Compatibility retained

- Former `/v1/containers/*` routes remain aliases of `/v1/sandboxes/*`.
- Containers with former `astrbot.plugin.docker_sandbox.*` labels remain discoverable and manageable.
- `CONTROLLER_TOKEN`, `CONTROLLER_LISTEN`, `DOCKER_SOCKET`, `SANDBOX_IMAGE`,
  `CONTROLLED_DOCKER_ENDPOINT`, and `CONTROLLER_RUNTIME_FILE` remain accepted.
- The AstrBot plugin adapter has been changed to the canonical `/v1/sandboxes/*` interface.

## New naming

```text
Program:        docker-control
systemd unit:   docker-control.service
config:         $PWD/.docker-control/docker-control.env
labels:         io.github.ycxom.docker-control.*
container name: sandbox-<key> (configurable)
```

New sandboxes only receive generic labels. Legacy labels are read but never written.

## systemd migration

```bash
sudo ./docker-control-v3.4.0-linux-amd64 install --migrate-legacy
```

This explicitly imports the old environment file, disables the former service, installs the new unit,
and starts `docker-control.service`. Without this flag, the installer does not alter the former unit.
