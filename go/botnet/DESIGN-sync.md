# Multi-client sync: design pass

Status: **recommendation, not yet decided.** No code written. Companion to `schema.go`,
which remains the spec; this document is the reasoning behind a change feed that does not
exist yet.

Date: 2026-08-29. Author: agent design pass, requested by team lead on behalf of the user.

---

## TL;DR

1. **The storage layer is right. The sync API is absent rather than wrong.** Nothing about
   the data model has to change. That decides extend-vs-rebuild: extend.
2. **Borrow JMAP's sync model and vocabulary. Do not adopt JMAP.** Protocol adoption pays
   off through interoperability and there is nobody to interoperate with.
3. **Keep the REST surface.** Carry the state token in a response header and every endpoint
   the Swift client already speaks stays byte-identical. All new surface is additive.
4. **A change feed subsumes the push channel but not token streaming.** Build the feed
   first; token streaming sequences after it and becomes a decoration on one message.
5. **Enforce single-writer on the database before building the feed.** Not optional, not a
   separate cleanup — see §9.
6. **No CRDTs.** One writer serializes everything; there is nothing to converge.

---

## 1. Verdict

**Adequate-with-patches**, with a reframing that matters more than the label: the storage
layer is right, and the sync API is *missing* rather than *wrong*.

The data model — append-only messages, status as a mutable column, segments, denormalized
bot metadata — already expresses everything a second client needs to know. A change log is
purely additive: no existing table changes shape, no existing endpoint is retired. That is
the difference between "wrong at the foundation" (the model cannot express what is needed)
and "incomplete" (the model is fine, the read API does not expose change). This is the
latter.

But the fix is one coherent subsystem, not a series of patches. Bolted on incrementally it
gets built twice.

## 2. Confirmed problems

All of the following are real, as reported:

- The feed is **insert-only and per-bot**. `MessagesAfter` filters `rowid > cursor`; an
  `UPDATE` does not move a rowid, so **status flips, error text, and settle events are
  invisible**. This is the reproduced bug: a second client's copy of a message is
  permanently stale at `awaiting`.
- **N bots means N+1 polls per cycle** (one per bot, plus the bot list).
- **No tombstones.** `DeleteBot` hard-deletes; an observer never learns.
- **Compaction is invisible too** — sealing mutates a segment row and inserts another, and
  a message cursor sees neither, so a second client's details panel silently shows a stale
  chain. This was not on the original list.
- **Sends are not idempotent.** A POST that succeeds while its response is lost duplicates
  the turn on retry.
- **No optimistic concurrency on PATCH.** Two clients editing a system prompt silently
  last-write-wins.
- **No push**, so reply latency is bounded below by the poll interval.
- `ReadAt` is one global watermark — correct if "read" is a property of the user, wrong if
  it is a property of the device. Not yet decided; see §10.

## 3. One correction: rowid, and the AUTOINCREMENT constraint

The claim that `after` is "ordered by rowid, and rowid is never exposed" is right, but the
conclusion cuts the opposite way from expectation.

SQLite assigns rowid as `max(existing)+1`, so a new rowid always exceeds every surviving
row — **today's ordering is sound**. The trap is that rowids **are reused** once the top
rows are deleted, and `messages` is `id TEXT PRIMARY KEY` with no `AUTOINCREMENT`, so
`DeleteBot` frees them.

Taking a message *id* as the cursor is accidentally protective: ids are unique forever, so
a stale cursor 404s rather than silently resolving to the wrong position.

> **Constraint for the implementation:** the moment a numeric position is exposed, that
> protection is gone — position 47 could name two different messages across a delete, and a
> client caching it would silently skip or replay. **The internal sequence must be
> `AUTOINCREMENT` (which SQLite guarantees never reuses) or a dedicated counter. Never bare
> rowid.**

## 4. What is already in place (do not rebuild)

- **The one-awaiting-per-bot rule is already a multi-client primitive.** It is a partial
  unique index (`idx_messages_one_awaiting ON messages(bot_id) WHERE status = 'awaiting'`),
  not a per-process lock, so it holds across clients for free. Two clients cannot both start
  a turn on the same bot; the loser gets 409 carrying the in-flight message — already a
  "here is what to watch" pointer.
- **`CompleteTurn` is one transaction**, so no client can observe a reply without its user
  turn settled. A whole class of torn read is already impossible.
- **`Store.tx(func(dbtx) error)` is the hook** the change log needs. The change row *must*
  be written in the same transaction as the mutation, or a crash between them loses the
  notification permanently.

## 5. Payloads versus pointers: return ids, not objects

The log stores **pointers, not payloads** — entity, id, op, seq. That keeps it small, makes
it self-coalescing (five edits to one message collapse to one id), and means replay from an
old cursor yields *current state* rather than a history of intermediate states.

`/changes` should return **ids only**, and the client then fetches. This reverses an earlier
recommendation to hydrate server-side. The reasoning: hydration saves a round trip that
costs ~1ms on localhost, and pays for it with flexibility — the split lets a client skip
objects it already has, request a subset of properties, and batch fetches across types.
Hydration optimizes a network that is not in the picture.

CouchDB states the same coalescing guarantee explicitly: "Only the most recent change for a
given document is guaranteed to be provided"
([changes docs](https://docs.couchdb.org/en/stable/api/database/changes.html)).

## 6. The protocol question

### 6.1 Recommendation: borrow the model and vocabulary, keep the REST surface

Not wholesale adoption. **Protocol adoption pays off through interoperability, and there is
nobody to interoperate with.** Building an email client, JMAP would buy existing servers,
client libraries, and a test corpus. For a private bot net, nothing else speaks it — you
take on the whole envelope (Session resource, capability negotiation, batched method-call
arrays, back-references) for zero ecosystem.

The independent convergence on JMAP's shape means *both* things: it is the obvious design,
**which is exactly why it is worth taking their vocabulary rather than minting new terms.**
Two people arriving at the same shape is weak evidence; a working group publishing it as an
RFC with the failure modes named is strong evidence. What you gain is their names and their
list of what goes wrong.

### 6.2 What to take from JMAP

Reference: [RFC 8620](https://www.rfc-editor.org/rfc/rfc8620.html) §5.1–5.3, §7.

**Per-type opaque state string.** "A (preferably short) string representing the state on the
server for *all* the data of this type in the account… If the data changes, this string MUST
change." Clients never introspect it.

> Opacity is strictly better than exposing a monotonic integer (an earlier recommendation,
> now withdrawn). It lets the implementation change without touching a client, and it
> removes the bug class where a client does arithmetic on the cursor — computes `state + 1`
> or infers gaps by subtraction. **Keep the `AUTOINCREMENT` counter internally; serialize it
> as an opaque token at the boundary.** Server treats it as a number, client as a token.

**Per-type rather than global.** Three types here — Bot, Message, Segment — so three tokens.
A client watching the sidebar should not be woken by message churn.

**`/changes` returns `created` / `updated` / `destroyed` as id arrays**, then the client
calls a `/get`-equivalent.

**`cannotCalculateChanges`.** When the server cannot compute changes from a state that old,
return this error; "the client MUST invalidate its cache" and resync. Servers SHOULD support
~30 days.

> This is the escape hatch that makes pruning safe, and it is not optional. CouchDB is the
> cautionary tale: after `_purge` a document "will not be available through `_all_docs` or
> `_changes` endpoints, **as though this document never existed**"
> ([purge docs](https://docs.couchdb.org/en/stable/api/database/misc.html)). A consumer that
> had not caught up never learns the document existed *or* was deleted. `cannotCalculateChanges`
> is the difference between pruning safely and silently lying to a stale client.

**`ifInState`** — the client asserts the state has not moved; mismatch returns `stateMismatch`
and aborts. That is the PATCH conflict story wearing an `If-Match` header.

### 6.3 What JMAP does not give you: idempotent writes

RFC 8620 defines **no** retry, deduplication, or at-most-once mechanism. Creation ids (the
`#` prefix) are *request-scoped* — they let one object reference another created in the same
call — not durable idempotency keys. A `/set` whose response is lost cannot be safely
replayed. This is a genuine gap in the spec, not an oversight in reading it.

Take the **discipline** from Replicache
([server-push reference](https://doc.replicache.dev/reference/server-push)): the effect and
the idempotency record must commit **in the same transaction**, and the record must advance
even when the mutation *fails*, or the client retries forever.

Take the discipline, **not the mechanism**. Replicache's per-client monotonic mutation id
with a server-side `lastMutationID` is built for replaying a queue of arbitrary offline
mutations. The write shape here is overwhelmingly "append one message."

> **Recommended: accept a client-supplied ULID on `POST /v1/bots/{id}/messages`.** The
> existing unique primary key makes a replay a no-op. Same at-most-once property, no
> per-client bookkeeping. This also cashes in the "generatable client-side with no
> coordinator" property `schema.go` already claims but never uses, and it lets the Swift
> client drop its `pending_` placeholder because the optimistic render can use the real id
> immediately.
>
> On id conflict: if the stored row's `botId` matches, return the existing message unchanged
> (idempotent replay). Otherwise 409. Validate prefix, length, and charset — this is client
> input reaching a primary key.
>
> For PATCH, `ifInState`/`If-Match` covers replay correctly and differently: a replayed
> PATCH carries a now-stale version and fails, which is the right answer.

### 6.4 CouchDB: take two contracts, leave the replicator

**Take:** the coalescing guarantee (§5), and the **idempotent-consumer rule** — clients must
tolerate seeing the same change twice. CouchDB requires this because replica failover can
replay changes; a single-writer daemon makes duplicates unlikely, but adopting the contract
costs nothing and makes retrying a poll thought-free.

**Leave:** everything else. A correct replicator needs eleven moving parts including
revision trees, `_revs_diff`, `_bulk_docs`, `_ensure_full_commit`, and checkpoint documents
at both ends ([replication protocol](https://docs.couchdb.org/en/stable/replication/protocol.html)).
That machinery exists for multi-master reconciliation that does not apply here.

### 6.5 Matrix: ruled out

Sharper reason than "too heavy": **the part that looks domain-relevant is the part Matrix is
actively replacing.** MSC4186's own motivation states `/sync` "scales badly as the number of
rooms on an account increases… incremental syncs are unbounded and slow down based on how
long the user has been offline," and that on large accounts "the initial sync operation can
take tens of minutes." The sliding-sync proxy was decommissioned in November 2024 for a
native replacement. Copying `/sync` means copying a design its own community has diagnosed.

The remainder — federation, state resolution (v2.1 exists to fix a state-corruption
vulnerability), room versions, event DAGs and auth chains — exists so mutually-distrusting
servers can agree on shared state. There is one writer here.

### 6.6 Replicache / Zero / ElectricSQL: ruled out as runtimes

- **Replicache**: repo archived June 2026, superseded by Zero.
- **Zero** (1.0, June 2026): server requires direct Postgres logical replication across
  three separate databases (`ZERO_UPSTREAM_DB`, `ZERO_CVR_DB`, `ZERO_CHANGE_DB`). No
  SQLite-as-source-of-truth mode.
- **ElectricSQL**: Postgres-only and **read-path only** — defines no write protocol at all.
  Runs as a separate service tailing Postgres's replication stream.

Adopting any of them means running Postgres to serve a localhost SQLite daemon. Take
Replicache's pattern as a design (§6.3); take no runtime.

### 6.7 CRDTs: ruled out, explicitly

A CRDT reconciles updates processed **independently on several nodes**, ensuring convergence
through commutativity
([Kleppmann, *Convergence*, ACM Queue 2022](https://martin.kleppmann.com/papers/convergence-acm-queue.pdf)).
This daemon is the single node that writes; clients are readers and request-senders. There
is no second independently-processed update to converge with, so the merge machinery has
nothing to do.

Worth noting the local-first authors' own empirical finding after building three CRDT
prototypes, under a heading reading "Conflicts are not a significant problem as we feared" —
"conflicts arise only if users concurrently modify the same property of the same object"
([Local-First Software, Onward! 2019](https://martin.kleppmann.com/papers/local-first.pdf)).

**cr-sqlite specifically collides with code that already exists**, which is more concrete
than an argument about complexity:

- It **forbids foreign keys** on replicated tables. This schema has `REFERENCES nets(id)`
  and `REFERENCES bots(id)`.
- Its design doc concedes "transaction semantics may violate during sync when two
  transactions partially intersect." That would **undo `CompleteTurn`'s atomicity** — the
  exact property that guarantees the transcript cannot interleave.
- The extension must be loaded on **every connection**, unenforceable across two binaries.
- Low velocity: last tagged release v0.16.3 (January 2024), a build-toolchain commit burst
  in August 2026, no new features.

**Steelman, so this is not a strawman.** A CRDT earns its place only if a device's offline
copy becomes a genuinely *authoritative second writer* — the phone commits real edits while
partitioned and you want both merged rather than one clobbering the other. An **unsent
draft is not that**: it is provisional, the server never saw it, and two drafts racing to be
admitted by one arbiter is resolved by last-writer-wins or a "you have two unsent drafts"
prompt. The test is not "multiple devices" or "offline" — it is whether the local copy keeps
making **committed** writes while disconnected. This one does not.

## 7. Sequencing against the queued SSE work

**A change feed subsumes the push channel. It does not subsume token streaming.** Conflating
those is the trap.

JMAP separates them correctly: its push notification carries a `StateChange` object — account
id plus new state strings per type — and **nothing else**. Neither PushSubscription (§7.2)
nor EventSource (§7.3) delivers changed data; both say "this type moved, come fetch." Push
and poll therefore share one cursor and one payload shape, so adding push later is a
transport swap with **no client re-cut**. Build the feed and the push channel is already
designed.

Token streaming is a different animal: ephemeral, high-frequency, sub-entity (partial content
of one message), inherently droppable. Putting tokens in a durable log means thousands of
rows per reply and replay semantics that mean nothing.

> **Order: feed first, tokens second.** Ship the token stream first and it becomes the only
> push channel available; the pull to make it the sync mechanism will be strong, and that
> bakes in a channel that cannot express replay, deletes, or mutations to other entities —
> then it gets re-cut, which is the outcome to avoid.
>
> Once the feed exists, token streaming has an obvious shape: the feed says message M is
> awaiting → open a token channel for M → the feed says M settled → take the authoritative
> content from the feed. The durable path stays correct when the token stream drops.
>
> Eventually both can multiplex over one SSE connection. Keep the seq on entity changes
> only; mark token frames explicitly droppable.

**Long-poll vs SSE:** start with long-poll (`GET /v1/changes?since=…&wait=30s`) because it
reuses the exact same endpoint, cursor, and payload as the plain poll — one shape, one code
path, trivially debuggable with curl. Move to SSE when multiplexing token streams makes a
persistent connection worth its lifecycle handling. Neither changes the client's cursor
logic.

## 8. Cost of the migration

Cheaper than feared, because **taking JMAP's model does not require taking JMAP's envelope.**

> **Carry the state token in a response header (`X-BotNet-State`), not by reshaping bodies
> into `{state, list}`.** Then `GET /v1/bots` and `GET /v1/bots/{id}/messages` keep returning
> bare arrays and **every endpoint the Swift client already speaks stays byte-identical.**

All new surface is additive:

| Addition | Shape |
|---|---|
| `GET /v1/changes?since={token}` | `{oldState, newState, hasMoreChanges, changed: {bots: {created,updated,destroyed}, messages: {…}, segments: {…}}}` |
| `GET /v1/messages?ids=a,b,c` | batch fetch by id |
| `If-Match` on `PATCH /v1/bots/{id}` | 412 on mismatch |
| optional `id` in `POST …/messages` body | client-supplied ULID |
| `X-BotNet-State` response header | on collection GETs |

Nothing shipped gets restructured. The client migrates when it wants the feed, not when the
server ships it. `?after=` stays for transcript pagination but stops being the sync
mechanism.

Real work: the log table, a write inside every mutating transaction, the `/changes` query,
and the resync error. **Estimate: a day or two**, plus token streaming sequenced after.

## 9. Single-writer enforcement is a prerequisite

**Confirmed hazard, reproduced by the team lead**, and worse than first described:

- Two processes, different ports, same DB: process 2's startup sweep marks process 1's live
  turn `failed`.
- Two processes, **same port**: process 2 cannot bind and exits with `bind: address already
  in use` — **but it has already swept**. `cmd/botnetd/main.go` calls `botnet.Open` (line 29)
  before `http.ListenAndServe` (line 47). A botnetd that fails to start still destroys the
  running server's in-flight work, and the operator sees only "address already in use."

Live on this machine today: the LaunchAgent holds 8730 against `~/.botnet/net.db`, and
`swift/botnet/botnetd` defaults to the same path. `dev/seed-demo.sh` is safe only because it
sets `BOTNET_DB` — luck, not design. `botnetsvc` already binds first; `cmd/botnetd` never
got the same treatment, so the two entry points disagree.

### Why this blocks the change feed rather than sitting beside it

Not primarily the sequence. Two processes sharing a SQLite file would keep the counter
monotonic — writes serialize on the file lock and `AUTOINCREMENT` lives in `sqlite_sequence`,
read and written inside the write transaction. The durable half survives.

**The push half does not.** Waking a long-poll or SSE subscriber is an **in-process signal**:
a process knows when *it* wrote, not when another process did. With two processes, clients
attached to A would never learn about changes made through B except by falling back to
polling — and they would have no way to know they were missing anything, because their state
token would look current. Silent divergence the feed's own design cannot detect.

Second reason: the sweep would now write change-log rows saying "these messages were updated
to failed," so the corruption propagates instantly to every connected client and arrives
looking authoritative. **The feed amplifies the existing bug.**

### Recommended mechanism

**`flock(2)` with `LOCK_EX|LOCK_NB` on a sidecar file** next to the resolved DB path, taken
in `Open`, released on `Close`.

The decisive property over a PID lockfile: **the kernel releases it when the process dies** —
no stale-lock problem, no PID-liveness heuristic to get wrong. That matters precisely because
the failure found involves a process exiting unexpectedly. Write the holder's PID into the
file so the error is legible: `another botnetd (pid 4821) is using ~/.botnet/net.db`.

Two things to decide:

1. It is **advisory** — it binds only openers that take it. Fine, since both binaries are
   in-tree.
2. A **read-only open path** will be wanted before any inspector tool needs to read the DB
   while the server runs.

Bind-first in `cmd/botnetd` is still worth doing as defense in depth and to stop the two
entry points disagreeing with `botnetsvc`, but it fixes only the same-port case. **The lock
retires the assumption; the reorder only narrows it.**

## 10. What NOT to do

- **No CRDTs / cr-sqlite / vector clocks.** §6.7.
- **No Matrix `/sync` shape.** §6.5.
- **No Postgres-based sync engine.** §6.6.
- **No per-device read state yet.** For one user with a Mac and a phone,
  read-anywhere-is-read-everywhere is what people expect, and the current single watermark
  is exactly that. It is the degenerate case of a `(bot_id, device_id)` table, so the door
  stays open. Decide deliberately rather than drifting.
- **No generic subscription/filter language.** `since` plus an optional type filter is enough.
- **No ETags on every endpoint.** Only `Bot` is genuinely contended; messages have a
  constrained status lifecycle with no user-versus-user conflict.

## 11. Recommended order of work

1. **Single-writer enforcement** (`flock` in `Open`, + bind-first in `cmd/botnetd`). Blocks
   everything else. §9.
2. **Change log + `/changes` + state tokens + tombstones.** One unit; ships together. §5, §6.2.
3. **Long-poll on `/changes`.** Same endpoint, same cursor. §7.
4. **Token streaming.** After the feed exists. §7.

Independent of the above, land whenever: **client-supplied message ids** (§6.3) and
**`If-Match` on PATCH** (§6.2). Both are write-path only.

## 12. Second pass: "should we adopt an existing system instead?"

Asked after §1–11 were written, and researched separately. Short answer: **no at the
substrate layer, no at the sync-engine layer, and partly YES at the change-capture layer —
where the thing to adopt is SQLite itself.**

### 12.1 Adopting a chat substrate (Matrix, XMPP, Mattermost, Zulip, MQTT): no

The capability argument is genuinely strong — Matrix's spec gives multi-device sync, read
markers/receipts, message edits, and threads as first-class primitives, and XMPP's MAM
(XEP-0313) is a real cursor-addressable archive with server-assigned non-reused ids and an
`item-not-found` on a stale cursor, which is structurally the same design as §6.2.

The operational argument runs the other way, and every first-hand report found at *this
scale* describes regret driven by footprint and unwanted ceremony rather than by missing
features: a five-year single-user Synapse operator migrating away over database bloat and
room-state growth ([yaky.dev](https://yaky.dev/2025-11-30-self-hosting-matrix/)); a measured
27.7% vs 1.4% memory comparison against XMPP
([gist](https://gist.github.com/farribeiro/9ee07a5c200ecd366710c2b1fc986b3f)); a team
building a secure messenger rejecting Matrix because "federation wasn't a feature request,
it was complexity we didn't need"
([post](https://medium.com/@anrysys/the-art-of-saying-no-why-we-didnt-use-matrix-for-our-secure-messenger-8ff7f5eb2099)).
The homeserver landscape is also unsettled: conduwuit archived May 2026, its successor
disputed between Tuwunel and Continuwuity; Dendrite in maintenance mode; Synapse the only
clearly-maintained option and the heaviest.

Domain mismatch is the deciding factor: rooms, membership, and per-bot user provisioning
imposed on a model that is "N independent permanent 1:1 conversations with a model."
Mattermost is the most plausible adopt candidate (Go-native bot SDK, single binary +
Postgres, real WebSocket event API) and still forces a full team-member account per bot.
MQTT is disqualified outright — retained messages are one latest value per topic, not
history; it is a transport, not a store.

### 12.2 Adopting a sync engine (Turso, PowerSync, Electric, Ditto, NATS JetStream): no

None lets the server stay plain self-hosted SQLite:

- **PowerSync**: Postgres/MongoDB/MySQL/SQL Server only. SQLite-as-backend is an open
  roadmap request, not a feature.
- **ElectricSQL**: Postgres-only, read-path only, no write protocol.
- **Turso Sync** (Beta, Oct 2025): genuinely does local-first writes, but the remote **must
  be Turso Cloud**. Self-hosted libSQL/`sqld` gives read replicas only. Also likely means
  leaving `modernc.org/sqlite`.
- **Litestream / LiteFS**: backup-DR and server-to-server read replicas. Wrong problem —
  they do not sync to client devices.
- **Ditto**: commercial mesh platform with per-device billing. Overkill for two devices you own.

**NATS JetStream** deserved the closest look and is the most interesting rejection. It is
genuinely embeddable in a Go binary (`server.NewServer(&server.Options{JetStream: true,
StoreDir: …})`), gives per-stream monotonic sequences, resumable consumers
(`DeliverByStartSequencePolicy` + `OptStartSeq`), and publish dedup via `Nats-Msg-Id`. Two
findings kill it anyway:

1. **The dual-write problem.** A JetStream publish and a SQLite commit cannot be atomic —
   even embedded, JetStream keeps its own store under `StoreDir`. The documented remedy is a
   transactional outbox: write the domain row *and* an outbox row in one SQLite transaction,
   then relay. **That outbox is the change-log table.** JetStream would add a second durable
   store without removing the first.
2. **Stale cursors fail silently.** When a consumer's position falls below the stream's
   `first_seq`, JetStream "silently skips forward… and resumes delivery from the stream's
   current first sequence"
   ([Synadia](https://www.synadia.com/insights/checks/nats-consumer-delivered-below-stream-first-sequence)).
   That is strictly worse than `cannotCalculateChanges` (§6.2), which tells the client to
   resync. We would have to build the gap detection in front of JetStream anyway.

### 12.3 Where adoption DOES win: change capture, from SQLite itself

This is the part §11 got wrong. The appendix originally asked an implementer to remember a
change-row write at eleven call sites — exactly the kind of rule that gets violated on the
twelfth.

**Recommended: `AFTER INSERT/UPDATE/DELETE` triggers appending to the change log.** They run
inside the mutation's own transaction by construction, cannot be forgotten, and capture
writes that never go through the Go helpers. This turns "every mutation must be logged" from
a convention into a property of the schema — the same move the partial unique index made for
the one-awaiting invariant.

Two related mechanisms considered and not chosen:

- **`RegisterPreUpdateHook`** — already present in `modernc.org/sqlite`, no dependency
  change, but it is a Go callback (awkward to write back to the DB from inside), and only
  fires for writes through that connection.
- **The SQLite session extension** (`sqlite3session_*`, changesets/patchsets) — genuinely
  built into SQLite and reachable pure-Go via `zombiezen.com/go/sqlite` (itself built on
  `modernc.org/sqlite`). Rejected **for the current client architecture**, for two reasons
  the research did not weigh: changesets are binary blobs designed for SQLite→SQLite
  replication, and they are session-scoped rather than cursor-addressable, so serving a
  client that was offline for a day means storing them in a keyed log — a change-log table
  again. It also means moving `store.go` off `database/sql`.

### 12.4 The one adoption worth revisiting later

**Make the clients hold SQLite replicas and ship them changesets** (session extension +
`zombiezen.com/go/sqlite`). This is a coherent, genuinely different architecture: the phone
gets offline reads and local queries for free, and change capture *and* application are both
SQLite's problem rather than ours.

The cost is a different client: `Store.swift` today states it "holds NO durable state: every
read and every mutation is an HTTP call to botnetd." This would invert that, and couple
client and server schema versions so migrations must be coordinated.

**Revisit if** offline read access on the phone becomes a requirement, or if a third and
fourth client appear. **Not now** — it is a large client rewrite to solve a problem
(a stale `awaiting` status) that §5–6 solve additively.

### 12.5 On "building our own means discovering mistakes"

Worth separating honestly. Of the defects found while building this:

- The **stale-`awaiting`** bug *was* a design mistake, and it is exactly the one JMAP's shape
  prevents. That is the argument for borrowing the design — which §6 does.
- The **two-process sweep** (§9) is an operational bug that no adopted protocol prevents.
  Matrix, NATS, or Mattermost would each have *more* processes and more of this class.
- The **rowid-reuse trap** (§3) is SQLite-specific and would survive any protocol choice.

Adopting a system does not remove mistakes; it exchanges yours for integration mistakes
inside someone else's model, which are harder to diagnose because you do not control the
code. The defensible middle is what §6 recommends: **borrow proven designs, own the
implementation** — take JMAP's vocabulary and failure modes, CouchDB's coalescing and
idempotent-consumer contracts, Replicache's transactional-idempotency discipline, and
SQLite's own change capture, while keeping a surface small enough to debug.

---

## 13. Memory writes (added 2026-08-30)

Per-bot editable memory (`bots.memory`, `Bot.Memory`) added two write paths, both
captured by the existing bots `AFTER UPDATE` trigger — no new trigger, no new entity:

- **User path**: `PATCH /v1/bots/{id}` with a `memory` field, through `UpdateBot`.
  Conditional under `If-Match` like any PATCH.
- **Model path**: `Store.SetMemory`, called mid-turn by the `memory` tool's
  `replace` / `clear` commands. Deliberately unconditional — a tool execution is
  one atomic store write.

Two rules the feed relies on: memory is **excluded from the derived `Bot.Version`**
(a model taking notes mid-chat must never 412 a user's in-flight edit — same
reasoning as list metadata), and a memory-only UPDATE **still emits a bot-updated
change row** (SQLite fires the row trigger for any UPDATE of the row), so a second
client sees the model take notes. User-vs-model memory writes are last-write-wins
for now; recorded as OPEN in `schema.go`.

The tool definitions themselves are NOT synced data: `GET /v1/tools` serves
`toolWireDefs()` straight from the binary's `memoryCommands` registry — deploy-static,
unversioned, and outside the change feed.

---

## Appendix: store/tx-layer facts the implementation will need

Recorded here so this survives outside any one session's context.

**Transaction hook.** `Store.tx(fn func(dbtx) error) error` wraps `db.Begin`/`Commit` with
rollback on error. `dbtx` is the interface shared by `*sql.DB` and `*sql.Tx`
(`Exec`/`Query`/`QueryRow`), so helpers run standalone or inside a transaction. Package-level
helpers already take `dbtx`: `appendMessage`, `setStatus`, `claimBot`, `ensureOpenSegment`,
`openSegment`. **Every change-log write goes through this**, in the same transaction as its
mutation.

> **REVISED 2026-08-29 (second pass, "should we adopt something instead?").** The
> call-site table below is now a *reference for what changes*, **not** an instruction to
> hand-write a change row at each site. Prefer **SQLite `AFTER INSERT/UPDATE/DELETE`
> triggers** that append to the change log. Triggers run inside the same transaction as the
> mutation by construction, cannot be forgotten when a twelfth call site is added, and catch
> writes that bypass the Go helpers entirely (a migration, a future entry point, a manual
> `sqlite3` session). The list below then becomes a test oracle — "does every one of these
> produce the expected change rows" — rather than a checklist someone has to honour. See §12.
>
> Note the driver already in use, `modernc.org/sqlite`, also exposes `RegisterPreUpdateHook`
> / `SQLitePreUpdateData` / `RegisterCommitHook`. That is a weaker option than triggers:
> it is a Go callback, writing back to the DB from inside it is awkward, and it only fires
> for writes through that connection.

**Mutating call sites (what changes — the trigger/test oracle):**

| Function | Entity | Op |
|---|---|---|
| `CreateBot` | bot | created (also creates segment 0 → segment created) |
| `UpdateBot` | bot | updated (memory field included — a memory-only PATCH still emits) |
| `SetMemory` | bot | updated (the model's memory tools, mid-turn) |
| `MarkRead` | bot | updated |
| `DeleteBot` | bot + its messages + its segments | destroyed (tombstones) |
| `AppendMessage` | message | created (+ bot updated — list metadata changes) |
| `SetMessageStatus` / `setStatus` | message | updated |
| `CompleteTurn` | message | created (reply) + updated (user turn settles) |
| `ClaimRetry` | message | updated |
| `Seal` | segment | updated (sealed) + created (next segment) |
| `failInterruptedSends` (startup sweep) | messages | updated |
| `markExistingBotsRead` (one-shot backfill) | bots | updated |

Note `AppendMessage` also updates `bots.last_message_at`/`last_message_text`, so it emits
**two** change rows (message created, bot updated). Easy to miss; the sidebar depends on it.

The startup sweep **should** emit change rows — a client reconnecting after a restart needs
to see those failures, and the state token will have moved regardless.

**`migrate` step ordering** (in `store.go`, called only from `Open`):

1. `CREATE TABLE IF NOT EXISTS` for nets, bots, messages, segments, meta (+ change log, new).
2. `addColumn` for post-release columns (idempotent via `pragma_table_info`).
3. `backfill()` — segment 0 per bot, message→segment assignment, list metadata. Each step
   guarded by the state it writes.
4. `once("read_at_backfill", markExistingBotsRead)` — a **one-shot** keyed on the `meta`
   table, not a data guard, because a post-upgrade unread bot is indistinguishable from a
   legacy one.
5. `failInterruptedSends()` — **must precede step 6**; a crashed process could have left two
   awaiting rows for one bot and the index cannot be built over them.
6. `CREATE UNIQUE INDEX idx_messages_one_awaiting ON messages(bot_id) WHERE status = 'awaiting'`.

A change-log table joins step 1. The sequence column must be `INTEGER PRIMARY KEY
AUTOINCREMENT` — see §3.

**Existing invariants the feed must not break:**

- At most one `awaiting` message per bot (partial unique index).
- `CompleteTurn` appends the reply and settles the user turn in one transaction, which is
  what makes out-of-order replies impossible. Documented as `DECISION (ordering under async)`
  in `schema.go`.
- The startup sweep is safe **only** at startup, because nothing is in flight in a process
  that has not begun serving. Called from `migrate`, which is called only from `Open`.

**Test suite:** 26 tests, `go test ./go/botnet/...` ≈0.4s, full gate (build + vet + test)
≈2.3s, `-race -count=3` ≈5s. No network, no external fixtures; migration tests build their
own in `t.TempDir()`.
