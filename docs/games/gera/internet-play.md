# GoldenEye: Rogue Agent over the Internet

This covers running the `gera` title for players on other networks. The
protocol details are in the [network map](online-map.md).

## Status

Tested on 2026-11-26: two PCSX2 machines on one LAN signed in, hosted, joined
and played a full match. Play between different networks has not been tested
yet.

## How players reach each other

The server never carries match traffic. Consoles connect to each other
directly over UDP. Each console sends the server a UDP `ECHO`, learns its
public address and port from the reply, and the server passes those
endpoints to the other side when a guest joins. The consoles then punch
through their routers with short bursts of probes. In the same FESL library
(per the TimeSplitters audit), the host probes for about 0.8 seconds and the
guest for about 4 seconds.

This works through most home routers. It fails when both sides have strict
NATs that pick a new public port for every destination, or when a firewall
drops unsolicited UDP. For those cases Rei Nation has an optional UDP relay,
described below.

To make hosting reliable, the hosting player should forward **UDP 18040** to
the PC running PCSX2 (or to the PS2). The client uses that fixed port for all
theater and game UDP. A host with a forwarded port can be joined from behind
almost any NAT.

## Server

1. Use a host with a public IPv4 address, or forward the ports below to it.
2. Set `REI_NATION_PUBLIC_IP` to that public address. Clients on the
   Internet are told to reach the theater and messenger there. Clients the
   server sees from a private address are on its own network, so they are
   pointed at the address they already reached the server on and no router
   loopback is needed.
3. Consoles on the server's own network are seen by their private addresses.
   When a remote player joins one of them, the server substitutes the public
   address with the same port, so a host on the server's network must have
   UDP 18040 forwarded to it. Players on the same network as each other keep
   their private addresses and need no forwarding.
4. Set `REI_NATION_GAMES=gera` and `REI_NATION_BIND=0.0.0.0` (or the address
   that receives the forwarded traffic).
5. With Docker Compose, the relay settings are passed through
   [compose.yaml](../../../compose.yaml). Set
   `REI_NATION_GERA_RELAY_DIR=/gera-relay` and set
   `REI_NATION_GERA_RELAY_HOST_DIR` to the host directory that
   `rei-relay setup` wrote. That directory is mounted read-only and must be
   readable by UID 65534; see [Relay](#relay).
6. Open these ports to the players:

   | Port | Protocol | Service |
   | --- | --- | --- |
   | 18040 | TCP | FESL login (SSL 2.0) |
   | 18041 | TCP | Theater game browser |
   | 18041 | UDP | Theater `ECHO` and endpoint keepalive |
   | 18042 | TCP | Messenger |
   | 18043 | TCP | Relay node sign-in (reserved only when `REI_NATION_GERA_RELAY_DIR` is set; open it to the node's IP) |

   PostgreSQL and the health endpoint stay on `127.0.0.1`; do not expose them.
7. Keep TCP keepalive and the UDP keepalive running (both are on by default).
   The server pokes each console's UDP port every 45 seconds, which keeps
   PCSX2 from closing it during a level load and keeps router mappings open.

To keep a log file when running outside Docker, set `REI_NATION_LOG_FILE`.

## Relay

The relay is a port of the TimeSplitters server's relay, which serves the
same FESL library. It runs as a separate program, `rei-relay`, on a machine
with **its own public IP**: the client treats every datagram from the
theater's address as server traffic, so a relay on the server's IP would not
work. A small cloud VM is enough.

1. On the server, register the node:

   ```sh
   rei-relay setup -dir relay -name relay1 -ip <node public IPv4>
   ```

   This writes `relay/control-cert.pem`, `relay/control-key.pem`,
   `relay/tokens.txt` and `relay/relay1-token.txt`. Keep the directory out
   of version control (it is in `.gitignore`). Node names may use 1 to 32
   ASCII letters, digits, `.`, `_` and `-`.
2. Point the server at the directory and restart it. For a direct binary
   run, set `REI_NATION_GERA_RELAY_DIR=relay`. With Docker Compose, set
   `REI_NATION_GERA_RELAY_DIR=/gera-relay` and
   `REI_NATION_GERA_RELAY_HOST_DIR` to the host path of `relay`. The container
   runs as UID 65534, and setup writes `tokens.txt` and `control-key.pem` as
   0600 in a 0700 directory, so give that user the directory:
   `sudo chown -R 65534:65534 relay`. Otherwise the server logs
   `relay hub not started` and runs without the relay. Open **TCP 18043** to
   the node's IP; nodes sign in there over TLS.
3. Copy `control-cert.pem` and `relay1-token.txt` to the node, delete the
   token file on the server, and run:

   ```sh
   rei-relay -server <server address>:18043 -cert control-cert.pem \
       -token-file relay1-token.txt -ports 20000-20999
   ```

   Open the UDP port range on the node. Each relayed player uses one port.

The node pins the server's certificate, and the server accepts a token only
from the IP registered with it, which is also the address players are sent
to. The node relays only between the public addresses of a match's players,
forwards one datagram per datagram, limits each player to 512 KB/s, and
closes a room after three minutes without traffic.

How a join uses the relay (logged as `relay decision`):

- **Direct** is the default. Players on the same public IP go direct unless a
  force rule applies.
- **Forced relay**: personas in `REI_NATION_GERA_RELAY_FORCE` (or every join
  with `*`) are always relayed, including on a shared public IP. The relay is
  chosen before the handoff. If no relay is configured or connected, the join
  is refused with `EGAM jden` and logged, rather than sent direct. If the
  relay room then cannot be opened, the attempt ends with a declined `EGEG`
  and the host never sees it. How the game words either refusal is untested.
- **Relay after a failed join**: a joiner whose last direct join failed
  (approved but left without entering within 30 seconds) gets relay addresses.
  That memory lasts 24 hours and is lost on restart.
- **Fallback**: when the relay's probe after `CGAM` shows the host's router
  accepts packets from the relay, the joiner gets the host's direct address
  plus the host's relay port as its alternate. The client itself switches to
  the alternate only within that attempt, if its direct probes fail. A
  fallback is offered only while the host's current endpoint matches the one
  probed, and only on a relay that passed the probe. A match's existing room on
  another relay is not reused for fallback.
- A host created before any relay connected, or whose endpoint has changed, is
  probed on the next join. That join stays direct; later ones can use the
  result. One probe per match runs at a time, and a probe is skipped if the
  host left during its 2-second delay.

Not yet tested in game: relay rooms, probes and all relayed join modes are
covered by loopback tests only.

### Relay directory

`tokens.txt` has one line per node: `name sha256:<token hash> <public ip>`. The
server keeps only the hash. A node is accepted only from its registered IP,
which is also the address players are sent to.

The hub stops cleanly on a fatal accept error. A node whose control link is
replaced by a reconnect loses its old rooms, which are marked closed. If a new
room fails partway through opening, the hub closes it on the node, which
releases any ports it bound. A node that loses its control link backs off up to
one minute between attempts. The backoff starts over at one second after a link
that stayed up longer than that.

### Shared public IPs

Players behind one router share a public IP and may use new source ports toward
the theater or a relay. The theater records an anonymous `ECHO` only for a
connection already seen on that IP and port. A new port could belong to a seen
console whose NAT moved, so it is not given to a connection that has not been
seen. The console still gets the reply and reports its own port in `EGAM`.

A relay node picks a datagram's sender by exact endpoint first, then by IP. A
datagram sent to X's port is not from X, and beyond that a member not yet heard
from is preferred.

Same-router joins stay direct. The exception is two PCSX2 instances in Sockets
mode after a failed join. They share one virtual address and would need the
router to hairpin, while both sending out to a relay needs no hairpin.

## Players

Each player needs:

- **DNS:** point `gera.en_US.fesl.ea.com` and `gera.fesl.ea.com` at the
  server's public address. A real PS2 needs a DNS server with both entries,
  for example [`configs/gera.dns.yaml`](../../../configs/gera.dns.yaml). In
  PCSX2's internal DNS hosts list, add **only** `gera.fesl.ea.com`. PCSX2
  matches names in lowercase, and with a `gera.en_us.fesl.ea.com` entry it
  answers, yet the game asks four more times over about 15 seconds before
  falling back to `gera.fesl.ea.com` (seen in PCSX2's log on 2026-11-26;
  probably the answer carries the lowercased name, which the game does not
  accept). Without that entry the lookup fails at once and the game falls
  back immediately.
- **DNAS:** the client passes DNAS before FESL. Rei Nation does not serve
  DNAS; players need their own DNAS replacement or bypass, as for other
  revived PS2 titles.
- **PCSX2 network mode:** Sockets. PCAP Bridged cannot reach a server on the
  same PC and floods the log with dropped jumbo frames.
- **Hosts:** forward UDP 18040 when possible.

## Security

Treat this as a test service.

- The client only speaks SSL 2.0 with RC4. That protects nothing against a
  network observer, so login passwords are effectively sent in the clear.
  Tell players to use a unique throwaway password.
- The theater and messenger are plaintext. Session keys authenticate players
  there and are random per login, but anyone who can see the traffic can
  reuse them.
- Connections that have not signed in are closed after 10 minutes (FESL) or
  one minute (messenger and theater). Session keys are revoked at logout and
  expire 10 minutes after their FESL connection closes.
- Relay control sign-in is throttled after repeated failures. Otherwise there
  is no rate limiting or abuse protection. Watch the logs during a test.
- Logs contain account and persona names, connection IDs and endpoints, but
  not passwords, session keys, secrets or tickets.

## Known gaps

- The relay has been tested only on loopback, not with consoles.
- Voice chat uses ProtoMangle at `demangler.ea.com` on port 3658 (feature
  `gera-ps2-2004`), which this title does not serve yet. Gameplay does not
  depend on it. NASCAR 04's demangler uses the same port, so the two titles
  cannot both serve it from one process as things stand.
- Stats, leaderboards and buddy lists are stored but have only synthetic
  tests so far. Players report their own stats, so leaderboards can be
  faked by a modified client. The account screen (`acct GetAccount` and
  `UpdateAccount`) has synthetic tests only.
- After a match without map rotation the host returns to the online menu and
  the joiner sees "Your connection to the game session has been lost". The
  joiner raises this itself when its session with the host is gone at the
  end-of-match step; the server is not involved.
