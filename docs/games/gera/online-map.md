# GoldenEye: Rogue Agent network map

This adapter targets the US PS2 release of GoldenEye: Rogue Agent
(`SLUS-21064`, disc `VER 1.01`, main executable `GE2/GE2RDVD.ELF`). The game
uses EA FESL from the `feslbuild_1.2` "jabba" library, not the Aries lobby that
NASCAR 04 uses. The adapter key is `gera`, the clientString the game registers.

## Evidence levels

| Label | Meaning |
| --- | --- |
| Live, tested in game | Seen with the retail client on PCSX2 (first on 2026-11-26). |
| Static | Read from the retail executable in Ghidra. No live traffic has confirmed it yet. |
| Synthetic | Covered by automated tests with synthetic frames. |
| Open | Not yet established. |

Sign-in, the messenger, hosting, joining and a full LAN match have been seen
live on PCSX2. Each section labels what is still static or synthetic. Nothing
has been tried on a physical PS2 yet.

## FESL endpoint

- Static: the game starts its FESL hub with clientString `gera`, SKU
  `BASLUS-21064`, locale `en_US` and version `1.0`.
- Static: it connects to TCP `18040`. The same number appears as the game's
  own host port in the `INTHOST` game attribute.
- Static: the hostname is built as `<clientString>[.<locale>].fesl[.<env>].ea.com`.
  The first lookup is `gera.en_US.fesl.ea.com`. If that lookup fails, the hub
  retries `gera.fesl.ea.com`. Both names should resolve to the server.
- Live: the client turns on SSL for this connection and passes the SSL name
  `fesl.ea.com` to DirtySock (`GE2/IOP/DRTYSCKF.IRX`). Rei Nation wraps the FESL
  listener in the SSL 2.0 transport the client negotiates; see
  [SSL transport](#ssl-transport).
- Open: DNAS runs before FESL (`GE2/IOP/DNAS280.IMG`) and needs its own
  replacement service.

## SSL transport

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
- Static: the parser also needs both validity dates as ASN.1 UTCTime (a year
  before 2050) and the subject and issuer as PrintableString. Go emits both for
  the values used, and a test checks the validity tags.
- Live: the retail client accepted the generated certificate and completed the
  handshake on PCSX2 on 2026-11-26.
- Synthetic: a ClientMasterKey whose RSA padding is invalid does not end the
  handshake early. The server continues with a random key, and the mismatch
  surfaces at CLIENT-FINISHED, so padding validity is not revealed.
- Synthetic: the handshake has its own 20-second limit, then restores the
  caller's read deadline, so the first FESL frame is still bounded.

`ssl2test` is an independent client implementation used by the tests. It reads
the public key straight from SubjectPublicKeyInfo because `crypto/x509` rejects
the deliberately mismatched signature algorithms.

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

Replies echo the request's code word and put `TXN` first. Values are
percent-encoded, and a value containing a space is quoted. Messenger and theater
frames carry the command in the tag and a status in the code word, with no `TXN`
line. FESL has no server heartbeat command, so the listener sends none. See
[Connection lifetime](#connection-lifetime).

## Connection lifetime

The client goes silent for minutes while it loads a level and expects its
FESL, messenger and theater sessions to survive. Signed-in connections
therefore have no application read deadline. TCP keepalive (30 seconds) closes a
peer that is gone without a FIN.

Connections that have not signed in are bounded. FESL must log in within 10
minutes, which leaves time for account registration with the on-screen keyboard.
The messenger must `AUTH` and the theater must `USER` within one minute, because
the client sends both as soon as it connects. These limits are synthetic. The
registration limit has not been timed against a slow player.

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

The Hello reply advertises theater on `18041` and messenger on `18042`, and Rei
Nation serves both. A client the server sees from a private address gets the
LAN address it reached the server on, so no router hairpin is needed. Everyone
else gets `REI_NATION_PUBLIC_IP`.

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
| `Logout` | Clear the session and revoke its keys. | Synthetic. |

`Login` and `LoginSubAccount` issue an `lkey` recorded in a server-wide store so
the messenger and theater can resolve it to the player. Every key belongs to the
FESL connection that received it. `Logout` revokes that connection's keys at
once. When the FESL connection closes, its keys stay valid for 10 more minutes
so the messenger and theater can reconnect during a brief FESL reconnect. Each
revocation prunes expired keys. The reconnect grace is an inference; the
client's FESL reconnect behavior is open.

`UpdateAccount` validates every field before it writes anything. It then
applies the password, email and profile changes in one transaction, so a
rejected email leaves the old password in place.

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
| `DISC` | Disconnect. When a ready connection closes, friends get the newest remaining ready presence, or `PGET SHOW=DISC` when none is left. An authenticated connection that never sent `PSET` does not keep the persona online. | Synthetic test. |
| `EPGT`, `EPST`, `GINV` and the rest | Acknowledged with an empty reply. `EPGT` is sent at sign-in. | `EPGT` live. |

Friendships live in `eanation.buddies` under title `gera` and messages in
`eanation.mailbox`; NASCAR 04 rows are separate.

A queued message is deleted only after one of the recipient's ready
connections has written its `RECV`. A frame that is dropped, for example on a
closing connection or a full send queue, leaves the row queued. The message is
offered again after 30 seconds or on the next delivery attempt.

An `AUTH` for a different persona on an authenticated connection starts a fresh
session. That connection gets no pushes until it sends its own `PSET`.

## Theater (game browser)

The client opens the theater on `theaterPort` (TCP 18041) to list, host and join
games, and uses the matching UDP port to learn its public endpoint. Plaintext
Aries framing, with a server PING heartbeat. Implemented in `theater.go`. Joins
are direct by default. The optional relay and its join modes are described in
[Internet play](internet-play.md#relay).

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

### Lobbies

After each lobby list the client's online manager (`FUN_00338920`) picks the
first lobby that passes one of two checks, depending on its mode flags. Either
`NAME` equals its region string from `FUN_00331a98` (`NorthAmerica` on this
disc, `Asia` otherwise), or `FAVORITE-PLAYERS` (lobby field `+0x34`) is
non-zero. With no match it records `nrom`, never sends `CGAM`, and the host
times out after the level loads.

### Join bookkeeping

- Synthetic: an approved guest that cancels or drops is reported to the host
  with `KICK`, because the host keeps the seat reserved until told. A pending
  guest is not reported, because the host may never have allocated it.
- Synthetic: when the guest cancels after the host received `EGRQ` but before
  its `EGRS`, the server remembers the PID for two minutes. A late approval then
  gets a `KICK`, and no `EGEG` is sent.
- Synthetic: `EGRQ` uses the identity admitted at `EGAM`, even if the same
  connection sends `USER` again while a relay room opens.
- Synthetic: an `EGEG` delayed for a relay room is sent only if the game and
  the player are still current. It is queued under the same lock that removes
  the game, so it can never follow a `GREM`.
- Synthetic: merged `UGAM`, `UGDE` and `UPLA` attributes are capped at 16 KiB
  encoded per map, so `GDAT`, `GDET` and `PDAT` replies stay well below the
  64 KiB Aries frame limit. An update that would exceed the cap is dropped and
  logged, and the previous state is kept.

### UDP endpoint exchange

The theater UDP port receives `ECHO` datagrams and replies with the source
`ADDR`/`PORT`, so each console learns its public endpoint. The server records it
and gives it to peers. A host's ECHO is bound by the `SECRET` from `CGAM`; a
joiner's is attributed by source IP. The exchanged addresses prefer the observed
public endpoint. PCSX2's sockets adapter reports the same internal address
(`192.0.2.100`) for every console, so that internal address is never sent to a
different console. It goes only to the host's own self-join.

When several consoles share a public IP, an anonymous `ECHO` is recorded only for
the connection already seen on that source port. The server still replies, so
each console learns its own port and reports it in `EGAM`. See
[shared public IPs](internet-play.md#shared-public-ips).

The server also `ECHO`es each observed endpoint every 45 seconds. PCSX2
otherwise unbinds a fixed UDP port after about 120 seconds of silence, which
drops the session during a level load.

A full match between two PCSX2 machines on one LAN was played on 2026-11-26.
Play between different networks has not been tested.

## Rank

The host asks for `GetRankedStats` when it enters its match. GoldenEye uses the
same FESL rank library as TimeSplitters: Future Perfect, so `rank.go` follows
that server's handling. The string table has the same rank fields as TimeSplitters (`u.%d.o`, `u.%d.ot`,
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
`<MODE>_W_PERC` as wins over games (0 to 1; the leaderboard shows it times
100) and `<MODE>_RANK`, which orders the leaderboards, as the win count. The
original formula is unknown.
`GetStats` and `GetRankedStats` return each requested key with its
competition rank (highest first, 0 when the persona has no value);
`GetTopN` and `GetTopNAndStats` return up to 16 rows, the client's capacity in
the shared library. Periods are ignored: every board is all-time. Which keys
GoldenEye reports, and whether a lower value should rank higher for any of
them, still needs a live match; each report is logged with its keys and
update types. `GetDateRange` gets an error reply so the screen does not wait
out its timeout.

## Messenger implementation notes

The roster flag letters and the notification order follow the TimeSplitters
server, which uses the same library. The GoldenEye binary's tag table and string
table match it. An accept needs no `RNOT CHNG=D` first. The client's `RNOT`
handler (near `0x4ccf88`) replaces `T` with `S` on an `A` carrying `S`, and a
`D` would delete the entry the client keeps from its own request.

## Other services named by the client

- Static: `GameSpyPreAuth` returns a `ticket` for a GameSpy match browser. Not
  implemented; may not be needed when the EA theater carries the browser.
- Static: ProtoMangle at `demangler.ea.com` with game feature `gera-ps2-2004`.
- `acct GetAccount` backs Account Options (seen live 2026-11-26; it was
  unanswered, so the screen waited). `accountinfo.go` answers with `name`,
  `email`, `countryCode`, `countryDesc`, `userId`, `DOBDay`, `DOBMonth`,
  `DOBYear`, `eaMailFlag`, `thirdPartyMailFlag` and `parentalEmail`, the
  account field names in the client's string table; the exact set the screen
  reads is not traced yet. `UpdateAccount` and `UpdatePassword` save email,
  those details, and a new password (`newPassword`) when `password` matches.
  Both log their request field names. Unknown `acct` transactions now get an
  error reply instead of none.
- Static: EA voice chat through `VOIPF.IRX`.
