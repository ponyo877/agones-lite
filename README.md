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

Everything planned for the first version is in place and tested, but nothing
has run in production yet. It has been tried on macOS with Colima, and on
Linux (Ubuntu 24.04, Docker 29, arm64 and amd64): five machines in two
regions, a hundred real game servers, agents and the control plane killed
and cut off on purpose. The sample images of the C++, Node.js, Rust and
Unity SDKs run unchanged, and so do upstream's conformance clients for
Python and C#.

- `GameServer`, `Fleet` (with rolling updates), `GameServerSet`
- Allocation over gRPC and REST, on the same API as the Agones allocator
- `FleetAutoscaler`: Buffer, Counter, List, Webhook, Schedule and Chain
- The SDK: every stable and beta call (Counters and Lists included), over
  gRPC, REST and WebSocket
- Several nodes: agents on other machines join with a token; a node that
  goes quiet has its idle game servers replaced and its sessions kept
- TLS and tokens, Prometheus metrics, an event history, logs through the CLI
- Images from private registries, with the credentials `docker login` stored

What is left out on purpose, and what Docker cannot express, is listed in
[docs/planning.md](docs/planning.md).

## Try it

You need Go, Docker and `make`.

```sh
git clone --recurse-submodules https://github.com/ponyo877/agones-lite.git
cd agones-lite
make build sidecar-image

bin/agones-lite server
```

In another terminal, apply the sample Fleet from the Agones repository and
take a game server from it, as a matchmaker would:

```sh
examples=third_party/agones/examples/simple-game-server
bin/agones-lite apply -f $examples/fleet.yaml
bin/agones-lite get gameservers
bin/agones-lite allocate --fleet simple-game-server
```

```text
NAME                             STATE   ADDRESS     PORT   NODE      AGE
simple-game-server-96p7v-dt658   Ready   127.0.0.1   7000   my-host   9s
simple-game-server-96p7v-ggpt7   Ready   127.0.0.1   7001   my-host   9s
simple-game-server-96p7v-dt658 127.0.0.1 7000
```

Let the Fleet grow and shrink by itself, then look at what happened:

```sh
bin/agones-lite apply -f $examples/fleetautoscaler.yaml
bin/agones-lite get gameservers -w          # every change, until Ctrl-C
bin/agones-lite describe fleet simple-game-server
bin/agones-lite logs simple-game-server-96p7v-dt658
```

If Docker does not listen on `/var/run/docker.sock` (Colima, for example),
point `DOCKER_HOST` at its socket first:

```sh
export DOCKER_HOST="unix://$HOME/.colima/default/docker.sock"
```

## More machines

The control plane listens for agents once it is given an address for them.
The join token carries the fingerprint of the control plane's certificate
authority, so an agent cannot be pointed at an impostor.

```sh
# on the control plane's machine
bin/agones-lite server --node-listen 0.0.0.0:9445 --tls-san cp.example.com
bin/agones-lite token show join

# on every further machine
bin/agones-lite agent --server cp.example.com:9445 --token-file join-token \
    --address 203.0.113.11
```

`agones-lite node cordon`, `drain` and `delete` take a machine out of
service. [deploy/](deploy) has systemd units and example configuration files.

Two things have to get through firewalls: players to the game server ports
of every machine (7000 to 8000 unless set otherwise), and agents to the
control plane's `--node-listen` port. On Linux the sidecars reach their
agent through a unix socket, so the host's own firewall is not in the way.

## Reading the code

The code is meant to be read. Functions are kept small (cognitive complexity
of 10 or less, checked in CI), and packages are named after their Agones
counterparts so the two can be read side by side. `third_party/agones` holds
the matching Agones source for that purpose; the build does not use it.

Start with `pkg/store`, then `pkg/gameservers/state.go`. The design document
has a full reading order.

- [docs/planning.md](docs/planning.md): goals, scope and decisions (Japanese)
- [docs/design.md](docs/design.md): architecture and reading guide (Japanese)
- [formal/README.md](formal/README.md): what is proved about the port
  allocator, Counters, the state machine and rolling updates (Japanese)

## Development

```sh
make test   # all tests; the Docker tests run when a daemon is reachable
make lint   # golangci-lint: the complexity limits, mistakes, security
make check  # what CI checks before a change reaches main, as far as one machine can
make proto  # regenerate code for the protocols in proto/
make dist   # release binaries for every platform, into dist/

# 1,000 game servers on 10 simulated nodes, 50 allocations a second
AGONES_LITE_LOAD=1 go test ./pkg/controlplane/ -run TestLoad -v
```

`test/conformance` drives the sidecar with the official Go SDK and over
REST; `pkg/controlplane` has the tests that tell the whole story, including
the ones that break things on purpose.

Beyond tests of examples there are three kinds that look for the input
nobody thought of:

```sh
# Properties: what must hold for any input, tried on random ones.
# The files are named property_test.go and model_test.go.
go test ./pkg/fleets/ -run Rollout -rapid.checks=100000

# Fuzzing: what comes in from outside, as bytes.
go test ./pkg/apiserver/ -run '^$' -fuzz FuzzAdmit -fuzztime 5m

# Proofs about models of the small pure functions, in Lean 4.
# Go tests replay what the models answer against the real functions.
cd formal && lake build
```

## License

Apache-2.0, the same as Agones. agones-lite uses the Agones API types and
generated SDK code as a Go module, and parts of its logic are ported from
Agones; those places name the upstream file they come from.
