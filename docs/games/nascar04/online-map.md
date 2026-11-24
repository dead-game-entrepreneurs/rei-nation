# NASCAR Thunder 2004 online protocol

The addresses below refer to the US PS2 executable `SLUS_208.24` (`SLUS-20824`). The flow combines Ghidra analysis with two-client PCSX2 tests.

| Stage | Retail flow |
| --- | --- |
| Startup | The client handles its DNAS check separately, resolves `ps2nascar04.ea.com` and queries the Aries directory over TCP 10600. `@dir` returns the lobby address. |
| Login | A lobby socket sends `addr`, `skey`, `auth` and `pers`. The `pers` reply supplies `LKEY`. |
| Room browser | `sele ROOMS=1 USERS=1 RANKS=1 MESGS=1` allocates client lists and subscribes to their snapshots. `+rom` updates a room and `+usr` updates a driver. `peek` previews a room. `room` creates one and `move` joins one. |
| Messenger | `news NAME=0` supplies `BUDDY_URL` and `BUDDY_PORT`. The client opens its own TCP 5000 socket, sends `AUTH` with the persona's key prefix, and loads its buddy and ignore rosters with `RGET`. The visible Online News requests document 1. |
| Invite | Private `mesg` frames carry a race offer, `ACPT`, `DECL` or `BLOC`. When both drivers register `chal`, `+ses` supplies the chosen peer endpoint, seed and match time. |
| Race | The stock game passes one endpoint to DirtySock's CommSRP transport. The Go relay can forward UDP 3658, 3659 and 9999. The game opens a separate EA SPORTS Talk channel on UDP 6000. |
| Report | The clients can reconnect to the lobby solely to send `rank`. Go matches that report to the admitted race across reconnections and stores one receipt and both drivers' totals. The client uses `snap` to query cards and leaderboards. |

## Rooms and challenges

The room browser at `0x207700` groups names by the `E`, `W`, `B` and `C` prefixes. The stock game puts user-created rooms in `C`. The room list decoder at `0x31bdf0` reads the viewer's current room name from the `R` field of `+who`. Previewing a room requires that viewer context before `+usr` supplies the occupants.

The buddy menu at `0x282c50` uses the presence state set by the Messenger decoder at `0x3617e8`. A buddy must be in `SHOW=CHAT` with the product bit set to expose Challenge. When a driver is signed in to the lobby without Messenger, Rei Nation publishes lobby-derived presence to that driver's buddies.

The receiver at `0x288a40` can send `BLOC` when it already has a challenge in progress or has blocked that player. Rei Nation tracks pending offers and forwards `DECL` when an offer is canceled or its peer leaves. The lobby omits the local driver's selectable row and rejects self-challenges.

## Messenger sockets and inbox

The bootstrap path at `0x206bd0` reads the Messenger address and port from the lobby. `0x327708` starts Messenger authentication and `0x326be8` marks the client ready after the reply. The lobby uses `~png` keepalives. Messenger uses `PING` and echoes it on its own connection. A fast lobby reconnect can leave Messenger disconnected if the game does not rerun the bootstrap.

Before a race, `0x327600` frees the local Messenger roster and inbox and sends `PSET SHOW=PASS`. On return, `0x327678` recreates the inbox, sets `SHOW=CHAT`, and reloads rosters if Messenger survived. Rei Nation holds deliveries during `PASS` and replays recent direct messages after roster sync so the client knows which senders are buddies.

The retail inbox keeps up to five messages per regular contact and one per temporary contact. `0x327c48` consumes a read message in local memory. The game sends no read acknowledgment. The server replays recent direct messages with their original timestamps and consumes buddy-added notices after delivery.

## Peer traffic and statistics

The lobby's peer ping and the disc's ProtoMangle library are separate from the race transport. The race code at `0x28aa18` selects CommSRP on one endpoint. In a blocked-direct PCSX2 test, the client timed out and returned to the lobby. A second handoff did not occur. Relay selection therefore happens before `+ses`. EA SPORTS Talk has its own transport through the `VOIPF.IRX` SIF RPC service `0x237`.

The finish path at `0x2991d8` calls the report writer at `0x298488`. The report includes a UTC match timestamp derived from the lobby handoff. The server checks the authenticated reporter and both admitted drivers, then deduplicates the result before updating standings. A saved result may describe a disqualification or disconnect. The other driver can still be finishing, so the relay remains available briefly after the first recorded report.

Rei Nation awards 10 points per head-to-head win. The original ranking formula has not been recovered. The server records starts, wins, losses, laps, track popularity, and the available disconnect and disqualification counters from accepted reports.
