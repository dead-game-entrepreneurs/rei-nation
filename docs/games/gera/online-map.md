# GoldenEye: Rogue Agent network map

This adapter targets the US PS2 release of GoldenEye: Rogue Agent
(`SLUS-21064`, disc `VER 1.01`, main executable `GE2/GE2RDVD.ELF`). The game
uses EA FESL from the `feslbuild_1.2` "jabba" library, not the Aries lobby that
NASCAR 04 uses. The adapter key is `gera`, the clientString the game registers.

## Evidence levels

| Label | Meaning |
| --- | --- |
| Static | Read from the retail executable in Ghidra. No live traffic has confirmed it yet. |
| Synthetic | Covered by automated tests with synthetic frames. |
| Open | Not yet established. |

No retail client has reached this adapter yet, so every client behavior below
is static.

## FESL endpoint

- Static: the game starts its FESL hub with clientString `gera`, SKU
  `BASLUS-21064`, locale `en_US` and version `1.0`.
- Static: it connects to TCP `18040`. The same number appears as the game's
  own host port in the `INTHOST` game attribute.
- Static: the hostname is built as `<clientString>[.<locale>].fesl[.<env>].ea.com`.
  The first lookup is `gera.en_US.fesl.ea.com`. If that lookup fails, the hub
  retries `gera.fesl.ea.com`. Both names should resolve to the server.
- Static: the client turns on SSL for this connection and passes the SSL name
  `fesl.ea.com` to DirtySock (`GE2/IOP/DRTYSCKF.IRX`). Rei Nation wraps the FESL
  listener in the SSL 2.0 transport the client negotiates; see the SSL section.
- Open: DNAS runs before FESL (`GE2/IOP/DNAS280.IMG`) and needs its own
  replacement service.

## SSL

Before any FESL byte, the client runs an SSL 2.0 handshake with the cipher
`SSL_CK_RC4_128_WITH_MD5`. `internal/protocol/ssl2` implements only that
handshake and cipher as a `net.Listener` wrapper, so the FESL listener reads and
writes plaintext while records on the wire are RC4-MD5 encrypted.

The client's SSL module (`DRTYSCKF.IRX`) carries the OTG and OTG3 certificate
authorities but, per the
[old ProtoSSL analysis](https://github.com/Aim4kill/Bug_OldProtoSSL), skips
signature verification when the outer certificate-signature algorithm is one it
does not recognize. `GenerateCertificate` builds a throwaway certificate for
`fesl.ea.com` each time the process starts and rewrites its DER so the inner
signature algorithm reads MD5-with-RSA and the outer reads rsaEncryption. The
same approach is confirmed working for the TSFP PS2 client, which shares this
SSL module family. The key is never written to disk.

- Static: the client supplies the SSL name `fesl.ea.com`, so the certificate
  uses that name. DNS redirection changes where the name resolves, not the name.
- Open: no retail GoldenEye client has completed this handshake against Rei
  Nation yet. It needs a PCSX2 or console test with the DNS names pointed here.

SSL 2.0 and RC4 are obsolete and provide no real transport security. They exist
only to match an unmodified retail client on an isolated test network.

## Framing

FESL uses the Aries 12-byte header. Rei Nation reads and writes it with
`internal/protocol/aries` and decodes the body with `internal/protocol/fesl`.

| Bytes | Meaning |
| --- | --- |
| 0-3 | Service, such as `fsys` or `acct` |
| 4-7 | Big-endian code word. The top byte is the message kind and the low 24 bits are the client's transaction sequence number. |
| 8-11 | Big-endian total size, including the header |
| 12- | Newline-separated `key=value` text ending in NUL |

Replies echo the request's code word and put `TXN` first. FESL has no server
heartbeat command, so the listener sends none and closes a socket that stays
silent for three minutes. The client's own keepalive interval is open.

Open: the client advertises `fragmentSize=2048`. Messages larger than that are
split into fragments in later FESL versions. Rei Nation does not yet join or
split fragments.

## fsys transactions

| TXN | Direction | Status |
| --- | --- | --- |
| `Hello` | Client sends clientString, clientPlatform, clientVersion, SDKVersion, protocolVersion and fragmentSize. | Static field names. Synthetic reply. |
| `Hello` reply | Server returns `theaterIp`, `theaterPort`, `messengerIp` and `messengerPort`. | Static field names read by the client. Synthetic reply. |
| `Ping` | Echoed with the same code word. | Static name. Synthetic reply. The sender is open. |
| `Goodbye` | The server closes the socket. | Static name. Synthetic. |

The executable has no `MemCheck` or `activityTimeoutSecs` strings, which later
FESL clients use, so the Hello reply sends neither.

The Hello reply advertises theater on `18041` and messenger on `18042` at
`REI_NATION_PUBLIC_IP`. Rei Nation does not listen on either port yet.

## acct service

Confirmed against a live client: after SSL the game registers and logs in
through the `acct` service, implemented in `accounts.go` over the shared
PostgreSQL identity store.

| TXN | Purpose | Status |
| --- | --- | --- |
| `GetTos` | Terms of service text. | Live. |
| `GetCountryList` | One entry, US. | Live. |
| `SuggestScreenNames`, `SuggestSubScreenNames` | Name suggestions (`names.[]`). | Live. |
| `AddAccount` | Create an account (bcrypt password). | Tested in game. |
| `Login` | Authenticate, return `lkey` and `userId`. | Tested in game. |
| `AddSubAccount`, `GetSubAccounts`, `LoginSubAccount` | Personas. | Tested in game. |
| `Logout` | Clear the session. | Synthetic. |

`Login` and `LoginSubAccount` issue an `lkey` recorded in a server-wide store so
the messenger can resolve it to the player. Keys are not yet expired.

## Messenger

Right after persona login the client opens the messenger on `messengerPort`
(TCP 18042) and will not show itself as online until `AUTH` succeeds. It uses the
Aries framing in plaintext (no SSL), with the command in the frame tag and a
four-character status in the code word. Implemented in `messenger.go`.

| Command | Purpose | Status |
| --- | --- | --- |
| `AUTH` | Present the login `lkey`; reply carries `USER` (persona). | Live. |
| `PSET` | Set presence. | Acknowledged. |
| `PING` | Keepalive. | Acknowledged. |
| `RGET` | Roster. | Empty until buddy lists are stored. |
| `USCH` | Persona search. | Empty for now. |
| `DISC` | Disconnect. | Closes the socket. |

Open: buddy lists, presence broadcast, and messages are not stored yet. The
connection has no server keepalive and a long idle timeout; a dropped console
lingers until it expires.

## Theater (game browser)

The client opens the theater on `theaterPort` (TCP 18041) to list, host and join
games, and uses the matching UDP port to learn its public endpoint. Plaintext
Aries framing, with a server PING heartbeat. Implemented in `theater.go`. This
first pass targets a LAN of separate machines with direct peer connections; it
has no relay or internet NAT traversal.

| Command | Purpose | Status |
| --- | --- | --- |
| `CONN` | Protocol negotiation. | Live. |
| `USER` | Authenticate with the login `lkey`. | Live. |
| `LLST`/`LDAT` | Two lobbies, `NorthAmerica` and `Asia`, each with `FAVORITE-PLAYERS=1`. The client picks the first lobby whose name matches the region string it derives from the console language, or, in some modes, whose `FAVORITE-PLAYERS` is non-zero; with no match it fails with `nrom` and never sends `CGAM`. Two consoles on one network can pick different lobbies, so every lobby lists and admits every game. | Tested in game. |
| `CGAM` | Host a game; reply carries GID and a UDP secret. | Tested in game. |
| `GLST`/`GDAT`/`GDET`/`PDAT` | List games and their details. | Implemented. |
| `EGAM`→`EGRQ` | Join request, forwarded to the host. | Implemented. |
| `EGRS`→`EGEG` | Host's answer, forwarded to the joiner with the host endpoint. | Implemented. |
| `ECNL`, `PLVT`, `PENT`, `UGAM`/`UGDE`/`UPLA`/`KEEP`, `RGAM` | Cancel, departures, entry, host reports, remove. | Implemented. |

### UDP endpoint exchange

The theater UDP port receives `ECHO` datagrams and replies with the source
`ADDR`/`PORT`, so each console learns its public endpoint. The server records it
and gives it to peers. A host's ECHO is bound by the `SECRET` from `CGAM`; a
joiner's is attributed by source IP. The exchanged addresses prefer the observed
public endpoint. PCSX2's sockets adapter reports the same internal address
(`192.0.2.100`) for every console, so that internal address is never sent to a
different console — only to the host's own self-join.

Open: no relay or NAT traversal, so this needs direct reachability (a LAN or
port forwarding). Verified by tests, not yet in game.

## Other services named by the client

- Static: `GameSpyPreAuth` returns a `ticket` for a GameSpy match browser. Not
  implemented; may not be needed when the EA theater carries the browser.
- Static: ProtoMangle at `demangler.ea.com` with game feature `gera-ps2-2004`.
- Static: `rank` stats (GetTopNAndStats, GetRankedStats) and `acct GetAccount`
  back the leaderboard, stats and account screens. Not implemented.
- Static: EA voice chat through `VOIPF.IRX`.
