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
drops unsolicited UDP. Rei Nation has no relay for those cases yet. The
TimeSplitters audit notes that a relay sharing the server's public IP would
be mistaken by the client for server traffic, so a relay needs a separate
address.

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
5. Open these ports to the players:

   | Port | Protocol | Service |
   | --- | --- | --- |
   | 18040 | TCP | FESL login (SSL 2.0) |
   | 18041 | TCP | Theater game browser |
   | 18041 | UDP | Theater `ECHO` and endpoint keepalive |
   | 18042 | TCP | Messenger |

   PostgreSQL and the health endpoint stay on `127.0.0.1`; do not expose them.
6. Keep TCP keepalive and the UDP keepalive running (both are on by default).
   The server pokes each console's UDP port every 45 seconds, which keeps
   PCSX2 from closing it during a level load and keeps router mappings open.

To keep a log file when running outside Docker, set `REI_NATION_LOG_FILE`.

## Players

Each player needs:

- **DNS:** point `gera.en_US.fesl.ea.com` and `gera.fesl.ea.com` at the
  server's public address. In PCSX2, add both to the internal DNS hosts list
  and also add `gera.en_us.fesl.ea.com`: the internal DNS compares names in
  lowercase. A real PS2 needs a DNS server with the same entries, for example
  [`configs/gera.dns.yaml`](../../../configs/gera.dns.yaml).
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
- There is no rate limiting or abuse protection. Watch the logs during a
  test.
- Logs contain account and persona names, connection IDs and endpoints, but
  not passwords, session keys, secrets or tickets.

## Known gaps

- No UDP relay for strict NAT on both sides.
- Voice chat uses ProtoMangle at `demangler.ea.com` on port 3658 (feature
  `gera-ps2-2004`), which this title does not serve yet. Gameplay does not
  depend on it. NASCAR 04's demangler uses the same port, so the two titles
  cannot both serve it from one process as things stand.
- Stats, leaderboards and buddy lists are stored but have only synthetic
  tests so far. Players report their own stats, so leaderboards can be
  faked by a modified client. The account screen (`acct GetAccount`) is not
  answered.
- After a match without map rotation the host returns to the online menu and
  the joiner sees "Your connection to the game session has been lost". The
  joiner raises this itself when its session with the host is gone at the
  end-of-match step; the server is not involved.
