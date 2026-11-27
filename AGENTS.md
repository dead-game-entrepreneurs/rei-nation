# Working on Rei Nation

Rei Nation implements EA Nation online services for stock game clients. The
first adapter is the US PS2 release of NASCAR Thunder 2004. Read the
[README](README.md) for setup and the
[NASCAR 04 network map](docs/games/nascar04/online-map.md) before changing that
adapter's wire behavior.

## Where code goes

| Path | Owns |
| --- | --- |
| `cmd/rei-nation` | Process entry point and explicit title registration |
| `internal/app` | Configuration validation, listener setup, readiness, and shutdown |
| `internal/game` | The contract between the process and a title adapter |
| `internal/protocol/aries` | Shared Aries frame encoding, TCP transport, and wire primitives |
| `internal/connectivity` | Shared peer introduction mechanisms such as ProtoMangle |
| `internal/storage` | PostgreSQL schema, queries, and transactions |
| `internal/games/<title>` | Command handling, wire field choices, rooms, social behavior, races, and title-specific relay policy |

Start a new behavior in its adapter. Reuse an existing shared package when it
already implements the same contract. Extract new shared code when real callers
need the same behavior and you can test that common behavior independently.
Identical command names or similar menus can hide different field meanings,
reply order, or session lifetimes. Keep those differences visible in each adapter.

[Quetzal](https://github.com/dead-game-entrepreneurs/quetzal) and
[Angelware](https://github.com/dead-game-entrepreneurs/angelware) are references
for explicit adapter registration, shared protocol primitives, title-scoped
state, Docker setup, and documented DNS. Check the client protocol before reusing
a mechanism across repos. For example, NASCAR 04 selects one peer endpoint before
the race, so its relay decision belongs in the NASCAR adapter. Record the source
and preserve applicable notices when adapting code from another project.

Prefer adaptive relay for Internet play when the stock client can switch after
a direct failure. If the client lacks that path, provide a forced relay option
for Internet play and document how to enable it. Explain client limitations
that rule out either approach and identify which routes have been verified.

## Work from client evidence

1. Identify the title, region, disc revision, and menu action. Trace the ordinary
   path through the client and the server before editing either side of the flow.
2. Use Ghidra, a redacted packet timeline, or a reproducible retail test to check
   command names, directions, separators, reply order, and socket lifetime.
   Label what the client sent, what the server inferred, and what remains open.
3. Reproduce the problem through the usual handler or service. Add a focused
   regression case to the existing test suite where it can catch the failure.
   Use synthetic accounts and frames. Update the title's notes when you establish
   new protocol behavior or change a compatibility decision.

Treat login, persona selection, room membership, Messenger, and race admission
as distinct lifetimes. NASCAR 04 can replace its lobby socket before reporting a
race while its Messenger socket stays open. Preserve report deduplication and
test reconnects when changing ownership or cleanup. If code takes both locks,
the NASCAR lobby lock comes before the Messenger lock.

## Verify and document changes

Use the Go version in [go.mod](go.mod). For Go changes run:

```sh
gofmt -l cmd internal
go test -race ./...
go vet ./...
go build -o bin/rei-nation ./cmd/rei-nation
```

Format changed Go files before committing. Use
[`scripts/test-integration.sh`](scripts/test-integration.sh) on Linux with Docker
and OpenSSL for changes to PostgreSQL, listeners, or flows that cross services.
It starts its own loopback Compose project and database. Keep the live stack
available while testing changes. Coordinate any restart with the person running
the console test.

Keep new and edited handwritten code comments to at most two lines. Explain
why the code is needed or what constraint it preserves. Put longer explanations
in the title's notes and link to them from the code.

Update [`.env.example`](.env.example), [`compose.yaml`](compose.yaml), and the
[README](README.md) when an operator-facing setting changes. Put title DNS
examples in [`configs/`](configs/) with documentation addresses. List DNS names,
ports, and relay options that the title actually uses. Keep synthetic test
coverage separate from behavior verified on PCSX2 or physical hardware.

Track public source, synthetic fixtures, and sanitized protocol notes. Keep
local `.env` files, credentials, database contents, packet captures, disc images,
and decompiler exports outside commits. Use connection IDs, command names, and
status codes in logs and reports. Redact account details, session keys, and
message bodies from examples. Review the staged file list before publishing.
