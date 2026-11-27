# TimeSplitters: Future Perfect on PlayStation 2

The opt-in `tsfp` adapter targets the US retail PS2 release (`SLUS-21148`). Its
reference is the [original server's main branch at 1de3a50](https://github.com/princebyyourlogic/tsfp-server-recreation/tree/1de3a50c04a5a3389f927db7ca63a4833771605d).

## Startup

1. Set `REI_NATION_GAMES=tsfp` or `REI_NATION_GAMES=nascar04,tsfp` and set
   `REI_NATION_PUBLIC_IP` to the IPv4 address reachable by the clients.
2. Redirect `tsfp.fesl.ea.com` to that address. Configure PS2 DNAS with a
   separate service, as for NASCAR.
3. The container image includes the PEM RSA certificate and key for FESL at
   `/tsfp-certs/fesl-cert.pem` and `/tsfp-certs/fesl-key.pem`. With the Go
   binary directly, use `REI_NATION_TSFP_CERT` and `REI_NATION_TSFP_KEY`.
4. Apply the settings with `docker compose up -d --build --wait`. Allow TCP
   18070, TCP and UDP 18071, and TCP 18072 from the trusted test network.

The retail game uses SSLv2 with RC4-MD5 and has separate trust requirements for
the `fesl.ea.com` identity. The Go server validates the supplied PEM and key at
startup. A synthetic client can use an ordinary test certificate. Confirm that
the retail client accepts the operator's certificate before testing the remaining
services. Use passwords dedicated to this service and keep legacy listeners
restricted to trusted clients.

## Service flow

- FESL replies to `Hello`, sends `MemCheck`, and advertises Theater and
  Messenger. Logging in as a persona issues a key those services can use
  independently. Accounts and personas use Rei Nation's shared identity
  tables. Keys live in the running process and expire after 24 hours.
- Theater uses `CONN` and `USER` before listing lobbies. `CGAM` returns a game
  ID and a secret for subsequent host-bound UDP `ECHO`. Anonymous `ECHO`
  reflects the client's translated port and dispatch `TYPE`. The adapter
  updates endpoint state only when it can identify the session.
- `EGAM` reserves a seat and sends `EGRQ` to the host. `EGRS` causes an
  `EGEG` approval or denial. `PENT` moves an approved entry from the joining
  count to the active count and makes it visible in `PDAT`. Guest cancellation
  or disconnect frees its seat and sends `KICK` to the host for an approved
  guest. Removing the game or losing its host sends `GREM` to affected peers.
- Messenger uses `AUTH` and `PSET` on an independent connection. Friendship
  requests and queued messages survive reconnects. The server removes a
  queued message after a successful socket write. The client does not send
  an application-level delivery receipt.
- FESL keeps map uploads, ratings, feedback and all-time persona stats in
  PostgreSQL. Map transfers and longer responses use FESL fragments within
  the SSL records. Ranking accepts persona-owned type-3 deltas and
  deduplicates identical retransmissions on the same login connection.
  The totals are client-reported.

## Peer routing

Gameplay goes directly between consoles by default, commonly on UDP 3000.
For a relay, set `REI_NATION_TSFP_RELAY_IP` to a distinct public IPv4 that
clients can reach and allow UDP 20000–20255 on it. The client treats packets
from the advertised Theater address as control traffic, so a container-only
address on the Theater host is insufficient. Each admitted relay member gets a
UDP port that represents them to the other players.

With an address configured, `REI_NATION_RELAY=always` uses the relay for joins
between distinct public addresses. In `auto`, Theater probes the host from the
relay address and uses the relay only after a verified response. Set `off` for
direct play only. The stock client has no automatic relay retry after Theater
hands off an endpoint.

The relay may run on a separate host. Configure the control certificate, key,
and hashed token registry on Theater, then run `cmd/tsfp-relay` with its
plaintext token file and pinned control certificate. The node initiates the TLS
connection on TCP 18073; Theater accepts only the registered name, token digest,
declared public IP, and source IP. Live rooms remain open across a brief control
reconnect and expire after three minutes without game traffic.

## Cutover and website

Use `scripts/migrate-tsfp-sqlite.py` for the original SQLite state or
`scripts/migrate-tsfp-django.sql` when Django PostgreSQL is authoritative. The
latter includes accounts, personas, friendships, queued messages, maps,
ratings, and all-time persona statistics. The commands and cutover order are
documented in the main README. Keep the original database backup until record
counts and the stock-client login agree.

For a future community site, start with public server status and read-only map
and leaderboard views backed by a dedicated API. Account linking, publication
controls and any browser-based editor need a separate design decision. The
existing site assets and administration code live in the original project.

The DNAS-to-FESL startup and an existing-account login have been verified on a
stock PS2. The port still needs a completed two-console match, a WAN relay test,
and retail-client map upload and download. Match telemetry and historical
dashboards from the optional Django stack are outside the current Go server.
