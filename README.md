# nodemesh

A small distributed node-status service. Every node records its own network
state into a **hash-chained append-only log** and replicates every peer's log
by pull gossip — so **any node can answer for any other node**, including full
history, even while the subject is offline.

Single Go binary, standard library only, no database, no agent/server split.

```
┌────────┐   pull gossip    ┌────────┐
│ node A │ ◄──────────────► │ node B │      each node holds every node's log
└────┬───┘                  └───┬────┘      → ask anyone, get the whole mesh
     │      ┌────────┐          │
     └────► │ node C │ ◄────────┘
            └────────┘
```

## Why

Monitoring usually dies exactly when you need it: the node that went down is
the one holding the answer, and a central collector is one more thing that can
fail. nodemesh inverts it — state is replicated everywhere, so a node being
unreachable is itself a fact the rest of the mesh recorded and can report,
with the history that led up to it.

It also refuses to trust a single signal. A node is only "gone" when every
independent path says so — see *Cross-checked reachability* below.

## The log

One record per node, appended only on state change plus a heartbeat every 10
minutes:

```json
{"node":"a","seq":412,"ts":1756742400,"netType":"wifi","ssid":"...",
 "localIP":"192.168.1.20","tsIP":"100.x.y.z","uptimeSec":98120,
 "prevHash":"9f2c…","hash":"41ab…"}
```

Each `hash` is `sha256` over the record plus `prevHash`, so a log is
tamper-evident and a peer can verify a chain it did not produce.
`GET /api/verify/{node}` walks the chain and reports validity and length.

Freshness is derived, not stored: `online` under 16 min, `stale` under 45 min,
`offline` beyond that — and then corrected by the cross-check below, which can
both bring a verdict forward and hold one back.

## Gossip

Pull-only anti-entropy every 2 minutes: each node asks its peers
`/api/log/{node}?after={seq}` for everything newer than what it has, and
appends. There is no leader, no consensus and no write path between nodes —
a node is the only writer of its own chain, which is what makes plain
append-and-verify safe.

An offline node simply stops extending its chain and catches up when it
returns. Weeks offline is a normal case, not an error path.

## Cross-checked reachability

A node that vanishes from the overlay network has not necessarily vanished.
nodemesh checks independent paths and reports them separately:

- **Overlay** — whether the node reports an overlay (Tailscale) address.
- **LAN** — if `lanPeer` is set, a plain ICMP ping to that sibling's mDNS
  name on the shared physical LAN. No credentials, no HTTP.
- **Bluetooth** — if `bleStatePath` is set, the last state written by a
  companion BLE beacon: a direct radio link that survives both the overlay
  and the LAN being down.

So the dashboard can say *"off the overlay, but answering on the LAN"*
instead of a misleading *"offline"*. These signals are deliberately **not**
chained — they are observations by the observing node, not facts about the
subject — but they do decide the state:

| overlay | LAN | BLE | state |
|---|---|---|---|
| answers | — | — | `online` — it just talked to us |
| silent | reachable | — | `stale`, never `offline` — alive, just off the overlay |
| silent past grace | unreachable past grace | silent | `offline` — two paths agree |
| silent past grace | unreachable past grace | alive | `isolated` — powered on, nothing reaches it |
| silent | no LAN sibling | — | falls back to the time-only verdict |

**Why not go by the age of the chain.** That was the obvious rule and it is
wrong in both directions. The chain only grows on state changes and on the
10-minute heartbeat, so a perfectly healthy node routinely sits ten minutes
without writing: "hasn't reported in a while" does not distinguish a dead node
from a quiet one, and using it as the trigger marked *live* nodes down. In the
other direction it is far too slow — on 2026-09-07 a power cut took
`steven-mini` down for six minutes (its chain jumps from 13 days of uptime
straight to 31 seconds) and nodemesh reported `online` the whole time, because
the gap fit between two heartbeats.

What is fast and honest is whether anyone can still *talk* to the node. A
successful `/api/nodes` fetch is immediate proof of life; a failed one used to
be a silent `continue` in the gossip loop and is now half the evidence of a
death. The ping to the LAN sibling is the other half. Both run every cycle, and
both must have been failing for `graceSecs` before a node is called down — a
single lost packet proves nothing.

Three guards keep it honest:

- **`graceSecs`** (180) — three consecutive failures at the default cadence,
  not one blip.
- **`obsTTLSecs`** (300) — an observation expires. Otherwise one stale negative
  would keep a node marked down long after it came back.
- **the observer must be online itself** — if bga loses its WiFi, its ping to
  mini fails even though mini is fine. Without this, bga would report its own
  outage as its neighbour's. The observer is judged by chain age alone, never
  by cross-check, so two nodes that watch each other cannot reason in a circle
  or take each other down in one blackout.

And one deliberate asymmetry: **a node is never condemned by a single path.**
"I can't reach it" is not "nobody can reach it" — the broken thing might be the
route, the mDNS name, or the observer's own Local Network permission. With only
one negative signal the time-only verdict stands.

**LAN and BLE observations relay second-hand.** Only bga pings mini, so that
verdict used to be bga's private knowledge: ask entry and you still got the
time-only answer. They now travel over gossip exactly like specs, and for the
same reason — `checkedAt`/`lastSeen` let the receiver keep whichever copy is
newer and drop the stale one. They carry a `by` field naming the observer,
which is what makes the "observer must be online" rule enforceable downstream.
The overlay check is the exception: it stays **first-hand only**, because
relaying "I couldn't reach it" would turn one node's routing problem into a
fleet-wide verdict.

## API

Any node answers for all nodes.

| Endpoint | Returns |
|---|---|
| `GET /` | Web UI — node cards, history drill-down, chain badge |
| `GET /api/nodes` | Latest record per node plus freshness and site |
| `GET /api/history/{node}?limit=N` | Newest-first history |
| `GET /api/log/{node}?after=SEQ` | Chain segment (what gossip consumes) |
| `GET /api/verify/{node}` | Walk the chain, report valid + length |
| `GET /api/specs` | This node's hardware: model, CPU, cores, RAM, disk, load |
| `GET /api/capacity` | **Fleet totals** — cores, RAM and disk summed across every reachable node |
| `GET /api/host` | This node only: battery (`present`/`percent`/`charging`) |
| `GET /api/ports` | This node's listening ports |
| `GET /metrics` | Prometheus: this node's specs (+ BLE beacon where configured) |
| `POST /api/restart` | Localhost only — exit so the supervisor restarts |

`charging` means *on external power*, not "the percentage is rising": a laptop
plugged in at 100% is `charging: true`.

## Specs and capacity

Every node reports what it is made of — model, CPU, physical and logical cores,
total and used RAM, disk size and free space, and 1-minute load — and those
specs ride along in the gossip, so any node can answer *how much machine does
this fleet actually have* without anyone SSHing anywhere:

```
$ curl -s node:7777/api/capacity | jq '{nodes, coresLog, memTotalGB}'
{ "nodes": 9, "coresLog": 78, "memTotalGB": 106 }
```

`isolated` nodes are excluded along with `offline` ones: the machine is powered
on, but if no network reaches it there is no work you can send it.

Three decisions worth knowing about:

**Specs are not in the hash chain.** The chain records *network state
transitions* and only grows when `sameStatus()` says something changed. Used
RAM changes every minute, so putting it in `Record` would turn a transition log
into an unbounded time series. Specs travel as a live field on `/api/nodes`,
carrying a `collectedAt` so a stale copy is visible as stale — the same call
`version` makes, for the same reason.

**Unlike `version`, specs relay second-hand.** A peer telling us about a third
node it can reach but we cannot is accepted, because `collectedAt` lets the
receiver keep whichever copy is newer. That timestamp is exactly what `version`
lacks, which is why versions are first-hand only.

**Disk is measured at `DataDir`, not at `/`.** On Android the root filesystem
is the read-only system partition — permanently 100% full at 1.2 GB on the R1 —
while the real storage (104 GB) is mounted elsewhere. Measuring where the
process actually writes is both correct there and the more useful number
everywhere else: it is the disk that can fill up.

The Linux collector uses only `/proc`, `/sys` and syscalls — **never `exec`**.
On Android `exec.LookPath` reaches `faccessat2()`, which the system seccomp
policy blocks, killing the process with an uncatchable `SIGSYS` (see `termuxBin`
in `host.go`). Specs are collected every 30s on every node, so that path has to
be structurally incapable of triggering it.

Load average is unix-only; Windows reports `0` rather than substituting a
CPU-percentage that would not mean the same thing.

## Configure

`nodemesh.json` next to the binary:

```json
{
  "node": "gateway",
  "port": 7777,
  "peers": ["node-b", "node-c"],
  "dataDir": "/var/lib/nodemesh",
  "collectSecs": 60,
  "gossipSecs": 120,
  "locations": { "gateway": "Cloud", "node-b": "Office" },
  "networks": { "aa:bb:cc:dd:ee:ff": "Home WiFi" },
  "lanPeer": "node-c.local",
  "graceSecs": 180,
  "obsTTLSecs": 300,
  "tsIP": ""
}
```

`node` defaults to the short hostname, `port` to 7777, `dataDir` to `./data`.
`peers` may include `:port`. `locations` also defines the set of nodes the
dashboard knows about, so a node with no records yet still shows as offline
rather than disappearing.

`networks` maps a **default-gateway MAC** to a friendly name, for platforms
that hide the SSID from background services (see below).

## Build & run

```sh
./build.sh          # cross-compiles darwin/linux (amd64+arm64) and windows into dist/
./nodemesh -config nodemesh.json
```

Go standard library only, `CGO_ENABLED=0`. Deploy is "copy the binary and
restart the service"; `deploy/nodemesh.service` is a systemd unit, and macOS
runs it fine as a root LaunchDaemon. Data is append-only JSONL per node —
safe to read, never edit by hand (it breaks the chain).

`go.mod` pins **Go 1.20** on purpose. Go 1.21 raised the Windows minimum to
Windows 10, and `rigby` — an HP Stream 7 tablet on 32-bit Windows 8.1 — is a
node. Dropping support means untested, not refusing to start: a `windows/386`
binary from a current toolchain does run there, verified. But "works today,
untested by upstream" is a bad thing to depend on for a service meant to be
believed when everything else is down, and the code is plain standard library,
so staying on the last supported toolchain costs nothing. `build.sh` uses 1.20
for the `windows/386` target only and builds everything else with whatever `go`
is on PATH:

```sh
go install golang.org/dl/go1.20.14@latest && go1.20.14 download
```

Without it `build.sh` still succeeds and just skips that one target, with a
warning — the other six nodes are not held hostage by a tablet.

### A node that is not on the overlay

nodemesh never binds `0.0.0.0`. It listens on loopback and on the node's
Tailscale address, which is what keeps a status service with no authentication
off the physical LAN. `rigby` cannot join the tailnet (Windows 8.1 stopped
getting patches), and is reachable only over the LAN from `athena`, the one
node that shares it. For that case `bindIP` adds a **second** listening
address:

```json
{ "node": "rigby", "bindIP": "192.168.101.88", "peers": ["192.168.101.93"] }
```

The obvious shortcut — putting the LAN address in `tsIP` — is wrong: `tsIP` is
not only the bind address, it is written into the chain and gossiped, so the
whole mesh would report an overlay that does not exist. With `bindIP` the node
honestly reports no overlay address and the cross-check keeps working.

Reachability is one-way and that is fine. Only `athena` can pull rigby's chain;
everyone else gets it **second-hand through athena**, which is exactly what
pull gossip is for — the log replicates, the node does not have to be routable
from every peer. The cost is that rigby is only observable while athena is up.
Its firewall rule is scoped to athena's address alone, not to the subnet:

```
netsh advfirewall firewall add rule name="nodemesh" dir=in action=allow \
  protocol=TCP localport=7777 remoteip=192.168.101.93 profile=any
```

### Windows

`deploy/nodemesh-task-windows.xml` registers it as a boot-triggered scheduled
task running as SYSTEM:

```
schtasks /create /tn nodemesh /xml C:\ProgramData\nodemesh\task.xml /f
```

Five of its settings are not cosmetic — Windows' defaults will silently kill a
long-running task, and on a laptop they do it in ways that look like the
program crashed:

| Setting | Default | Why it must change |
|---|---|---|
| `ExecutionTimeLimit` | `PT72H` | Windows kills the process at 72 h and reports `267014` — "terminated by user" |
| `RestartOnFailure/Count` | none | nothing revives it |
| `DisallowStartIfOnBatteries` | `true` | on battery the task sits in `Queued`; `schtasks /Run` still reports success |
| `StopIfGoingOnBatteries` | `true` | unplugging kills it |
| `MultipleInstancesPolicy` | `IgnoreNew` | two writers on one node silently reset the chain |

The XML must be **UTF-16LE with a BOM** or `schtasks` answers `The task XML is
malformed` without saying why. `iconv -t UTF-16LE` omits the BOM and
`iconv -t UTF-16` writes a big-endian one; neither works:

```sh
printf '\xff\xfe' > task.xml && iconv -f UTF-8 -t UTF-16LE source.xml >> task.xml
```

## Notes from production

Things that cost real debugging time, kept here because they generalize:

- **Recent macOS redacts the SSID even for root daemons.** `wdutil` and
  `system_profiler` return `<redacted>`; there is no root loophole any more.
  The fallback is to identify a network by its **default-gateway MAC**, which
  is readable everywhere — hence the `networks` map.
- **On Android, never exec by PATH.** `exec.Command("foo")` makes Go call
  `LookPath`, which uses the `faccessat2` syscall — blocked by Android's
  seccomp filter, killing the process with an uncatchable `SIGSYS`. The
  supervisor then revives it only to die on the next request: a crash-loop
  triggered by a single HTTP call. Resolve an absolute path yourself and pass
  that; with a slash in the name, `exec.Command` skips `LookPath` entirely.
- **`ping -W` is milliseconds on macOS and seconds on Linux.** The same flag,
  three orders of magnitude apart.
- **A CLI can exit 0 while printing an error.** The overlay's macOS CLI does
  exactly that when it cannot reach its GUI helper, so its output is validated
  as a CGNAT address before being believed.
- **Known fragility — two writers on one node silently reset the chain.**
  `Store.Append` validates against its in-memory head and never re-reads the
  file. If two processes for the same node run at once (a deploy where the old
  process was not killed first), both append the same `seq`. `verifyChain`
  then rejects the whole file on the next load and the chain restarts from
  seq 1, appending to the same file forever after — discarding history on
  every restart, undetected, because a skipped chain does not crash anything.
  Fixing it properly needs a file lock, or validating against a fresh read of
  the last line instead of trusting memory. Until then: confirm the old
  process is dead before starting a new one, and watch `/api/verify/{node}`
  length against what you expect.

## License

MIT — see [LICENSE](LICENSE).
