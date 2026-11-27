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

The client's messenger tag table (built near `0x4c5944`) lists `AUTH`, `DISC`,
`USCH`, `USER`, `PADD`, `PDEL`, `PSET`, `PGET`, `RADD`, `RADM`, `RRSP`, `RDEL`,
`RDEM`, `RNOT`, `RGET`, `ROST`, `MLST`, `TCKL`, `GINV`, `GRSP`, `GRVK`, `GNOT`,
`SEND`, `RECV`, `BRDC`, `EPST`, `EPGT`, `ADMN` and `PING`. The field names in
its string table (`USER`, `DOMN`, `RSRC`, `SHOW`, `STAT`, `GROUP`, `CHNG`,
`ATTR`, `TYPE`, `SUBJ`, `BODY`, `NOREPLY`, ...) match the TimeSplitters build of
the same library, so the roster rules below follow that server where the
GoldenEye binary does not say otherwise. First console test: 2026-11-26.

The client's reply handler (`FUN_004ce578`) matches several replies by the
transaction `ID` it sent, not by tag alone: `RGET` must carry ID 2 (buddy list)
or 1, `USCH` ID 3 and `EPGT` ID 4, and every `ROST` and `USER` entry must carry
the same `ID` as its request or the client ignores it. Request fields
(resolved from the client's builders): `USCH` sends `USER`, `DOMN`, `RSRC`,
`DIST` (`T`/`F`) and `MAXR`; `RADM` sends `USER`, `GROUP`, `LSRC` and `PRES`;
`RRSP` sends `USER` and `ANSW` (`Y` accept, `N` decline, `B` block) and `PRES`
on accept. `ATTR` letters map to entry flags by letter (`S` 0x40000, `T`
0x80000; `R` sets nothing). An `RNOT CHNG=A ATTR=S` on an entry holding `T`
replaces `T` with `S`, and `CHNG=D` clears only `S`, `T` and flag 4, deleting
the entry when nothing else is left. Entries that came from the sign-in
`RGET` keep flag 0x200, so a `D` does not remove them until the next sign-in.

| Command | Purpose | Status |
| --- | --- | --- |
| `AUTH` | Present the login `lkey`; reply carries `USER` (persona). | Live. |
| `PSET` | Set presence. The first one marks the connection ready; friends get `PGET`, then queued messages are delivered. | Synthetic test. |
| `PING` | Keepalive. | Acknowledged. |
| `RGET` | Roster: `SIZE`, then one `ROST` per friend (`ATTR` `S` friend, `R` request received, `T` request sent). `PEND=Y` includes requests; `PRES=Y` adds `PGET` for online friends. The ignore list (`LIST=I`) is returned empty, as TimeSplitters does. | Synthetic test. |
| `USCH` | Persona search by prefix, up to 20 `USER` replies. | Synthetic test. |
| `RADM` | Friend request. Both sides get `RNOT CHNG=A`. A request to someone who already asked accepts it. | Synthetic test. |
| `RRSP` | Answer a request (`ANSW=Y` accepts). On accept both sides get `RNOT CHNG=A ATTR=S` with no `D` first; a decline sends `D`. The first console test sent a `D` first and lost the requester's entry. | Synthetic test. |
| `RDEL`, `RDEM` | Remove a friend. `RNOT CHNG=D` only for a friendship that existed. | Synthetic test. |
| `RADD`/`RDEL` with `LIST=I` | Block and unblock. A block also ends the friendship. | Synthetic test. |
| `PADD`, `PDEL` | Watch a friend's presence; `PADD` answers with `PGET`. | Synthetic test. |
| `SEND`, `BRDC` | Message (`TYPE` `C` or `A`, `SUBJ` up to 31 bytes, `BODY` up to 255). Stored, then delivered as `RECV` to a ready connection and removed. | Synthetic test. |
| `DISC` | Disconnect. Friends get `PGET SHOW=DISC` when the persona's last connection closes. | Synthetic test. |
| `EPGT`, `EPST`, `GINV` and the rest | Acknowledged with an empty reply. `EPGT` is sent at sign-in. | `EPGT` live. |

Friendships live in `eanation.buddies` under title `gera` and messages in
`eanation.mailbox`; NASCAR 04 rows are separate.

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
| `GLST`/`GDAT`/`GDET`/`PDAT` | List games and their details. | Tested in game. |
| `EGAM`→`EGRQ` | Join request, forwarded to the host. | Tested in game. |
| `EGRS`→`EGEG` | Host's answer, forwarded to the joiner with the host endpoint. | Tested in game. |
| `ECNL`, `PLVT`, `PENT`, `UGAM`/`UGDE`/`UPLA`/`KEEP` | Cancel, departures, entry, host reports. | Tested in game. |
| `RGAM`→`GREM` | Host removes its game; every other player and pending joiner gets `GREM` in the lobby it joined from. Also sent when the host disconnects or creates a new game. | Synthetic. |

### UDP endpoint exchange

The theater UDP port receives `ECHO` datagrams and replies with the source
`ADDR`/`PORT`, so each console learns its public endpoint. The server records it
and gives it to peers. A host's ECHO is bound by the `SECRET` from `CGAM`; a
joiner's is attributed by source IP. The exchanged addresses prefer the observed
public endpoint. PCSX2's sockets adapter reports the same internal address
(`192.0.2.100`) for every console, so that internal address is never sent to a
different console — only to the host's own self-join.

Open: no relay or NAT traversal, so this needs direct reachability (a LAN or
port forwarding). A full match between two PCSX2 machines on one LAN was
played on 2026-11-26.

## Other services named by the client

- Static: `GameSpyPreAuth` returns a `ticket` for a GameSpy match browser. Not
  implemented; may not be needed when the EA theater carries the browser.
- Static: ProtoMangle at `demangler.ea.com` with game feature `gera-ps2-2004`.
- `rank`: the host asks for `GetRankedStats` when it enters its match. The
  string table has the same rank fields as TimeSplitters (`u.%d.o`, `u.%d.ot`,
  `u.%d.s.%d.k/v/ut/t`, `keys.%d`, `minRank`, `maxRank`, `periodId`,
  `periodPast`, `stats.%d.key/value/rank/text/owner/name` and
  `stats.%d.addStats.%d.*`). `rank.go` stores persona stats in
  `eanation.player_stats`: `UpdateStats` adds the value for update type 3 and
  replaces it for type 5 (GoldenEye sends 5 for current loss streaks, which
  reset) and type 0 (assumed). In the first console match the host sent one
  report for its guest and one for itself, and the guest sent none, so a
  report for another persona is accepted when that persona was admitted to
  one of the reporter's games in the last three hours.
  The screens read `<MODE>_RANK`, `<MODE>_W_PERC` and `<MODE>_K` (modes seen:
  `DM`, `TOW`, `D`, `CD`, `TD`) but the game reports only totals such as
  `DM_W`, `DM_T_G`, `DM_K`, `DM_DTHS` and `DM_CUM_PLACE`. The server derives
  `<MODE>_W_PERC` as 100 times wins over games and `<MODE>_RANK` as the win
  count; the original formula is unknown.
  `GetStats` and `GetRankedStats` return each requested key with its
  competition rank (highest first, 0 when the persona has no value);
  `GetTopN` and `GetTopNAndStats` return up to 16 rows, the client's capacity in
  the shared library. Periods are ignored: every board is all-time. Which keys
  GoldenEye reports, and whether a lower value should rank higher for any of
  them, still needs a live match; each report is logged with its keys and
  update types. `GetDateRange` gets an error reply so the screen does not wait
  out its timeout.
- Static: `acct GetAccount` backs the account screen. Not implemented.
- Static: EA voice chat through `VOIPF.IRX`.
