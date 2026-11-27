<p align="center">
  <img src="docs/assets/rei-nation-logo.png" alt="Rei Nation" width="360">
</p>

<h3 align="center">Bringing EA Nation games back online</h3>

<p align="center">
  A Go replacement for <b>EA Nation's Aries online services</b>,<br>
  with a tested <b>NASCAR Thunder 2004</b> adapter for PlayStation 2.
</p>

<p align="center">
  <img alt="Go" src="https://img.shields.io/badge/Go-1.27-00ADD8?logo=go&logoColor=white">
  <img alt="Docker Compose" src="https://img.shields.io/badge/Docker-Compose-2496ED?logo=docker&logoColor=white">
  <img alt="PlayStation 2" src="https://img.shields.io/badge/PlayStation%202-online-003791?logo=playstation&logoColor=white">
  <a href="https://github.com/dead-game-entrepreneurs/rei-nation/actions/workflows/go.yml"><img alt="Go tests" src="https://github.com/dead-game-entrepreneurs/rei-nation/actions/workflows/go.yml/badge.svg"></a>
</p>

<p align="center">
  <a href="#status">Status</a> ·
  <a href="#dns-redirects">DNS redirects</a> ·
  <a href="#running-the-server">Running the server</a> ·
  <a href="docs/games/nascar04/online-map.md">NASCAR 04 notes</a> ·
  <a href="docs/games/gera/online-map.md">GoldenEye notes</a> ·
  <a href="#credits">Credits</a>
</p>

---

## Status

**NASCAR Thunder 2004** (US PS2, `SLUS-20824`) is the first adapter. Testing used two PCSX2 clients on a LAN.

| Feature | Status |
| --- | --- |
| Account creation, persona selection and login | ✅ tested in game |
| Regional rooms, player previews and created rooms | ✅ tested in game |
| Head-to-head challenges and declines | ✅ tested in both directions |
| Direct and relayed race traffic | ✅ tested on a LAN |
| Ranked results after a lobby reconnect | ✅ a ranked result updated both drivers' standings |
| EA Messenger buddy presence and direct messages | ✅ tested between two clients after a race |
| Inbox replay, account recovery and relay ownership | 🧪 covered by automated tests |
| Physical consoles, other disc regions and Internet NAT combinations | 🧪 awaiting wider testing |
| Relay for EA SPORTS Talk | Unimplemented |

**GoldenEye: Rogue Agent** (US PS2, `SLUS-21064`) has a `gera` adapter that uses EA FESL. Two PCSX2 machines on one LAN have signed in, registered accounts and personas, come online in the messenger, hosted, joined and played a full match. Stats, leaderboards and buddy lists are implemented but not yet tried in game. Play between different networks has not been tested. An optional UDP relay for strict NATs (`cmd/rei-relay`, ported from the TimeSplitters server) has only loopback tests. Voice chat is not done. See the [GoldenEye notes](docs/games/gera/online-map.md) and [Internet play](docs/games/gera/internet-play.md).

The client owns race simulation. The server handles accounts, rooms, Messenger, peer setup, and ranked statistics. Message history is restored after the client loads its buddy and ignore lists. The game can display restored messages as unread again.

## How it fits together

```text
PS2 / PCSX2 ── DNS ──────────▶ your resolver
     ├── DNAS ───────────────▶ separately configured DNAS service
     ├── Aries lobby ────────▶ Rei Nation TCP 10600
     ├── EA Messenger ───────▶ Rei Nation TCP 5000
     └── Race traffic ───────▶ the other console directly or through Rei Nation's UDP relay
```

The lobby issues the key used to authenticate Messenger on its own connection. The game can close and reopen its lobby socket during a race while Messenger stays connected. The [NASCAR 04 network map](docs/games/nascar04/online-map.md) covers the wire protocol and result flow.

## DNS redirects

Point `ps2nascar04.ea.com` at the IPv4 address your PS2 or PCSX2 clients use to reach the server. Use that same address for `REI_NATION_PUBLIC_IP` in `.env`.

```text
address=/ps2nascar04.ea.com/192.0.2.10
```

Replace `192.0.2.10` with your server address. The line above is for dnsmasq. Use [configs/nascar04.dns.yaml](configs/nascar04.dns.yaml) for revive-dns.

For GoldenEye: Rogue Agent, add `gera` to `REI_NATION_GAMES` and point both FESL names at the server. Use [configs/gera.dns.yaml](configs/gera.dns.yaml) for revive-dns.

```text
address=/gera.en_US.fesl.ea.com/192.0.2.10
address=/gera.fesl.ea.com/192.0.2.10
```

Configure the PS2 network profile or PCSX2 to use your resolver and keep DNAS routing configured separately.

## Running the server

Use Docker Compose on a Linux host with host networking.

```sh
git clone https://github.com/dead-game-entrepreneurs/rei-nation.git
cd rei-nation
cp .env.example .env
chmod 600 .env
```

Set `REI_NATION_PUBLIC_IP` in `.env` to the server's LAN or public IPv4 address. Choose a unique `POSTGRES_PASSWORD`, then start the stack.

```sh
docker compose up -d --build --wait
docker compose logs -f rei-nation
```

Allow these ports for the features you use.

| Port | Purpose |
| --- | --- |
| TCP 10600 | Aries directory and lobby |
| TCP 5000 | EA Messenger |
| UDP 3658, 3659, 9999 | Peer relay |
| TCP 3658 | ProtoMangle HTTP helper for compatible clients. This NASCAR race path does not use it. |

PostgreSQL and `/healthz` bind to the host loopback interface on ports `55434` and `8086` by default. `docker compose down` stops both services and keeps the database volume. The legacy client uses plaintext Aries on the network. Create an account password dedicated to the game.

### Relay

Set `REI_NATION_RELAY` in `.env` to choose the route before matchmaking hands off to the clients.

| Value | Behavior |
| --- | --- |
| `auto` | Uses direct play on a LAN and selects the relay for two distinct public addresses when a mapping is available. |
| `always` | Prefers the relay when both drivers have distinct source IPv4 addresses, including on a LAN. |
| `off` | Uses direct play. |

Each source IP can belong to one active relay pair. If both consoles share an address or a route is already in use, the server offers direct play. The stock client accepts one endpoint per attempt and has no automatic relay retry. The relay covers race traffic. EA SPORTS Talk uses UDP `6000` directly between peers.

### Messenger and account recovery

If the game's fast sign-in restores the lobby but Messenger still errors, sign out of EA Online and sign back in to rerun the game's Messenger startup.

To enable **Forgot Account Name** and **Forgot Password**, configure `REI_NATION_SMTP_ADDR` and `REI_NATION_SMTP_FROM`. The mail server must support STARTTLS. Set `REI_NATION_SMTP_USER` and `REI_NATION_SMTP_PASSWORD` if it requires authentication. A recovery password expires after 30 minutes or becomes the account password on first use. Players can then choose another password with **Edit Account**.

## Development

Use the Go version in [go.mod](go.mod). Run the same checks as GitHub Actions.

```sh
go test -race ./...
go vet ./...
go build -o bin/rei-nation ./cmd/rei-nation
```

Run `scripts/test-integration.sh` on Linux with Docker and OpenSSL to test both sockets, Messenger retention, UDP relay, and PostgreSQL-backed stats in a disposable Compose project. It binds the test stack to loopback. `REI_NATION_TEST_BIND`, `REI_NATION_TEST_PG_PORT`, and `REI_NATION_TEST_HEALTH_PORT` can move the test listeners if their defaults are already in use.

Title adapters live under `internal/games`. `internal/protocol/aries` handles wire framing and TCP connections, `internal/connectivity` handles peer introduction, and `internal/storage` handles PostgreSQL persistence.

## Credits

- [mrsavage0789](https://github.com/mrsavage0789) led the original local PS2 revival and NASCAR Thunder 2004 client research that made this port possible.
- [a-blondel and the EA Nation Server contributors](https://github.com/a-blondel/ea-nation-server) provided the GPLv3 Aries server implementation and codec reference. The Go SSC2 decoder adapts the upstream `CryptSSC2` and `LobbyTagField` code and uses a public upstream test vector.
- [mohh-master-server contributors](https://github.com/a-blondel/mohh-master-server) developed the predecessor to EA Nation Server.
- [riperiperi and Breakin-In contributors](https://github.com/riperiperi/Breakin-In) published the Apache-2.0 server for The Sims: Bustin' Out (PS2). Rei Nation's initial Go adapter targets NASCAR Thunder 2004.
- [Quetzal contributors](https://github.com/dead-game-entrepreneurs/quetzal) provided the reference for the Docker and DNS setup.

## License

Rei Nation is distributed under the [GNU General Public License, version 3](LICENSE). It is an independent preservation project. Game names belong to their respective owners.
