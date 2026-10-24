# Network binding

Optional, per-protocol binding of bitnews's outbound sockets to a network interface and/or a local address. It is a **kill switch**: when the bound interface (a VPN, for instance) disappears, the protocol has no route and stops, instead of sending its traffic through the default route.

bitnews does not set up a VPN (see [deploy.md](deploy.md#vpn)). Binding is an additional safety net on top of whatever VPN you run, not a replacement for the container-level setup recommended there.

## Settings

Two bindings, one per protocol, both empty (off) by default. They live in the settings document (`PUT /api/v1/settings`) under `network`:

```json
{
  "network": {
    "usenet":  { "interface": "wg0", "address": "10.2.0.2" },
    "torrent": { "interface": "wg0", "address": "" }
  }
}
```

| Field | Meaning |
|---|---|
| `interface` | Interface name, at most 15 characters of `[A-Za-z0-9_.:-]`. On Linux every socket gets `SO_BINDTODEVICE`. |
| `address` | A local IP address (no port, no zone, not unspecified, not multicast). Sockets listen on it and connect from it. |

Either, both or neither may be set. Settings stored before the key existed load with both off. If both are set, the address should belong to the interface; the monitor treats an address that is not assigned to that interface as unavailable.

**The API and UI listener is never bound.** The settings only affect outbound NNTP connections and BitTorrent sockets. A bitnews whose VPN is down is still reachable from your LAN.

## What is covered

| Traffic | Usenet binding | Torrent binding |
|---|---|---|
| NNTP connections (TCP, then TLS) | yes (`Config.Dialer`) | n/a |
| BitTorrent peer listeners (TCP) | n/a | yes |
| Outgoing peer connections (TCP) | n/a | yes |
| uTP socket (listener and outgoing) | n/a | yes, unless uTP is disabled |
| DHT sockets | n/a | yes, when DHT is on (they share the uTP socket, or get their own bound UDP socket) |
| HTTP tracker announces and metainfo fetches | n/a | yes (`TrackerDialContext`, `HTTPDialContext`) |
| UDP tracker sockets | n/a | yes (`TrackerListenPacket`) |

How it is done for torrents: anacrolix/torrent's built-in listeners and dialers cannot take a `Control` hook, so with a binding the engine starts the client with its own peer and DHT sockets switched off (`DisableTCP`, `DisableUTP`, `NoDHT`) and attaches sockets it opened itself through `netbind`. Nothing is left on the unbound defaults, and a socket that cannot be bound makes `torrent.New` fail instead of falling back. UPnP/NAT-PMP port mapping, web seeds and WebTorrent are off for every engine whether or not a binding is set.

## What is NOT covered

- **DNS lookups.** Host names of NNTP servers, trackers, DHT bootstrap nodes and peers are resolved by the system resolver, whose sockets are not bound. A DNS query can therefore leave through the default route, and it reveals which hosts you are about to contact. Only the connections that follow are bound. Run bitnews in the network namespace of a VPN container (gluetun or similar) if DNS leaks matter; that is the recommended setup and the only one that covers everything.
- **Fetches made by the API** for the user: downloading an NZB or `.torrent` from a URL, indexer requests and similar. They are one-off requests on behalf of an API call and use the normal route.
- Anything the container or host does outside bitnews (updates, health checks).

## Per-OS support

| OS | `address` | `interface` |
|---|---|---|
| Linux | yes | yes, `SO_BINDTODEVICE`. Since Linux 5.7 an unprivileged process may bind its own sockets to an interface. On older kernels it needs `CAP_NET_RAW` (in Docker's default capability set; lost with `cap_drop: ALL`). |
| macOS, BSD, Windows | yes | **not supported**: any socket with an interface binding fails with `netbind.ErrUnsupported`. macOS has `IP_BOUND_IF`; it is out of scope. Use `address` instead. |

If the binding cannot be applied the dial or listen fails with an error wrapping `netbind.ErrBind`. It never falls back to an unbound socket.

## Monitor and kill switch

`netbind.Monitor` checks the binding every 5 seconds (configurable): the interface must exist and be up, and the address, if set, must be assigned to that interface (to any interface that is up when none is named). It reports the first result and every change after that, with a reason such as `interface "wg0" does not exist`.

`netbind.Killswitch` connects a monitor to two callbacks:

- `Pause(reason)`: once per outage, including when the binding is unavailable at start-up.
- `Resume()`: once when the binding is available again after a pause.

The daemon is meant to connect them as follows (that wiring lives in `internal/daemon`, not in `netbind`):

- Pause: pause the protocol's Queue with reason `bind_unavailable` and close its sockets. For torrents the engine is closed; for Usenet the pools' connections are closed.
- Resume: reopen the sockets (a new torrent engine, new pool connections) and resume the Queue.
- A change of the settings restarts the affected engine or pools. The torrent engine has no live rebind.
- Starting while the binding is unavailable: `torrent.New` returns an error wrapping `torrent.ErrBindUnavailable` and Usenet dials fail. The daemon starts paused and lets the kill switch resume it.

### Plugging into the code

- Usenet: `nntp.Config.Dialer = netbind.Binding{...}.Dialer(&net.Dialer{...})`. In the runner, set `runner.Config.Factory` to a function that builds `nntp.DialFactory` with that Dialer.
- Torrents: `torrent.Config.Binding = netbind.Binding{...}`. The legacy `Config.BindAddr` is the same as `Binding.Address`.

## Tests

Everything except the real interface binding runs on any OS. Linux-only tests bind to the loopback interface (`lo`) and skip when `SO_BINDTODEVICE` is not permitted.

### Network namespace test

`internal/netbind/netbind_netns_linux_test.go` (build tag `netns`, skipped unless `NETBIND_NETNS=1` and root) builds a veth "VPN" link to a peer namespace plus a second, worse route to the same peer, then:

1. binds to the VPN interface and checks that traffic flows;
2. deletes the interface and checks that the kill switch pauses within 5 s;
3. checks that bound dials fail, that the packet counter of the other interface does not move, and that an unbound dial (the control) does get through the other route;
4. recreates the interface and checks that the kill switch resumes within 5 s and bound dials work again.

It needs root, `ip` (iproute2) and the right to create network namespaces. The CI `test` job does not run it. To run it on a Linux machine:

```sh
sudo NETBIND_NETNS=1 "$(command -v go)" test -tags netns -run Netns -v ./internal/netbind/
```

From another OS, build the test binary and run it in a privileged container:

```sh
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go test -tags netns -c -o /tmp/nb/netbind.test ./internal/netbind
docker run --rm --privileged -e NETBIND_NETNS=1 -v /tmp/nb:/nb:ro debian:13-slim sh -c \
  'apt-get update -qq && apt-get install -y -qq iproute2 && cd /tmp && /nb/netbind.test -test.run Netns -test.v'
```

(`GOARCH=amd64` on an x86 machine.)

## Implementation notes (M5-07)

- **Package `internal/netbind`** (standard library only; `syscall.SetsockoptString` with `SO_BINDTODEVICE`, no `x/sys`): `Binding{Interface, Address}` with `Enabled`, `Validate`, `Control`, `Dialer`, `ListenConfig`, `ListenAddr`, `Check`/`CheckNow`; `Monitor`, `Killswitch`, `SystemLookup`. Interface names are validated again right before the setsockopt (`core.ValidInterfaceName`), so a hostile name never reaches the kernel.
- `Dialer` is a small type with `DialContext` (it satisfies `nntp.Dialer`) rather than a bare `*net.Dialer`, because `LocalAddr` has to match the network of each dial (`tcp`, `udp`, `ip`) and a `net.Dialer` has one. The base dialer's `Control` hook still runs, after the binding's.
- An address of the wrong family for the network (`tcp4` with an IPv6 address) is an error, not a fallback.
- The torrent engine's `Binding` replaces the old `BindAddr`-only path: with any binding, all peer, uTP and DHT sockets are opened by the engine (see above). The IP families follow the address (one family) or are both (interface only, the IPv6 socket is skipped if the host has none). A random listen port is shared by all sockets and retried when its UDP twin is taken.
- A pure-Go uTP socket (`anacrolix/utp`) is built on the bound UDP socket; `anacrolix/utp` is used directly. It is an indirect dependency in `go.mod` that `go mod tidy` will promote.
- `Config.DisableUTP` keeps its meaning with a binding; DHT then gets its own bound UDP socket.
- Sockets are not rebound live. After an outage the daemon creates a new engine, so existing connections never survive onto another interface.
