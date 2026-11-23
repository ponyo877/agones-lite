# agones-lite

> **Unofficial.** agones-lite is not affiliated with or endorsed by the
> [Agones](https://github.com/agones-dev/agones) project.

agones-lite runs the core of Agones without Kubernetes. It is for small
production setups: one to a few VMs with Docker, and no cluster to operate.

What was built for Agones works here unchanged:

- **Game servers.** A container image that uses an official Agones SDK runs
  as it is. The sidecar speaks the same gRPC and REST protocol.
- **Manifests.** `GameServer` YAML written for Agones is accepted, for the
  subset of the Pod definition that Docker can express.

Compatibility is pinned to Agones v1.62.0.

## Status

Early. The first milestone works: a single `GameServer` is placed on a node,
started in Docker, becomes `Ready` through the SDK, and is removed on
`Shutdown`. Fleets, allocation and autoscaling come next; see the roadmap in
[docs/planning.md](docs/planning.md).

## Try it

You need Go, Docker and `make`.

```sh
git clone --recurse-submodules https://github.com/ponyo877/agones-lite.git
cd agones-lite
make build sidecar-image

bin/agones-lite server
```

In another terminal, apply the sample game server from the Agones repository:

```sh
bin/agones-lite apply -f third_party/agones/examples/simple-game-server/gameserver.yaml
bin/agones-lite get gameservers
```

```text
NAME                       STATE   ADDRESS     PORT   NODE     AGE
simple-game-server-5xbmz   Ready   127.0.0.1   7000   node-1   8s
```

If Docker does not listen on `/var/run/docker.sock` (Colima, for example),
point `DOCKER_HOST` at its socket first:

```sh
export DOCKER_HOST="unix://$HOME/.colima/default/docker.sock"
```

## Reading the code

The code is meant to be read. Functions are kept small (cognitive complexity
of 10 or less, checked in CI), and packages are named after their Agones
counterparts so the two can be read side by side. `third_party/agones` holds
the matching Agones source for that purpose; the build does not use it.

Start with `pkg/store`, then `pkg/gameservers/state.go`. The design document
has a full reading order.

- [docs/planning.md](docs/planning.md): goals, scope and decisions (Japanese)
- [docs/design.md](docs/design.md): architecture and reading guide (Japanese)

## Development

```sh
make test   # unit tests; the Docker tests run when a daemon is reachable
make lint   # golangci-lint, including the complexity limits
make proto  # regenerate code for the protocols in proto/
```

## License

Apache-2.0, the same as Agones. agones-lite uses the Agones API types and
generated SDK code as a Go module, and parts of its logic are ported from
Agones; those places name the upstream file they come from.
