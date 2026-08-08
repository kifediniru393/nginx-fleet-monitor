# nginx Fleet Exporter — Design Plan

**Status:** greenfield design, pre-PoC
**Revision:** 3 — refocused as a self-contained standard exporter with an optional keepalived module; adoption of external agents dropped; config-hygiene goals added
**Date:** August 2026

---

## 1. Goals

The exporter exists to do six things:

1. **Efficient nginx monitoring, scrapeable like any standard Prometheus exporter.** One HTTP `/metrics` endpoint, standard exposition format, negligible scrape cost. No sidecar platform, no pipeline.
2. **Always identify the current active node.** In a keepalived pair, which node holds the VIP right now — from the wire, not from self-report.
3. **Identify all backends currently receiving traffic, per vhost.** Not the intended routing — the observed one.
4. **Capture failover as a first-class event.** When the keepalived master changes, the new master is identifiable within one master-down-interval, and the transition itself is a metric.
5. **Keepalived support is an optional module.** Many nginx deployments have no VIP and no keepalived. The exporter must be fully useful without it — VRRP is a collector you can turn off (or that turns itself off), never a dependency.
6. **Empirical config hygiene.** Surface vhosts and upstreams that serve no traffic (decommission candidates), verify nginx actually delivers traffic to every configured upstream member, and flag upstreams that are down — all from observed evidence, not configuration claims.

### Motivating questions

These are the concrete questions the fleet cannot answer today, and each maps onto the goals above:

1. **Per-vhost volume accounting.** Multiple `server_name`s share one backend IP (`10.1.1.100`). At L3/L4 everything collapses onto that tuple, so we cannot say which site generates which share of the load. *(goals 1, 3)*
2. **Backend degradation attribution.** When the shared backend degrades, we cannot say which vhost is responsible. *(goals 3, 6)*
3. **Cluster capacity.** `nginx-1` and `nginx-2` are an active/standby pair fronted by a keepalived VIP. Prometheus sees two unrelated hosts, so we cannot reason about headroom or about moving a vhost to another cluster. *(goals 2, 4)*

Today the only way to answer any of this is to SSH into the box.

### Design constraints

- **Self-dependent.** The exporter depends on no external exporter or agent at runtime. External tools (OBI, Coroot, VTS, nginxlog-exporter) are prior art and accuracy benchmarks in the test harness — never runtime dependencies.
- **Minimal nginx config coupling.** Observability should not live in `nginx.conf`. Strong preference, not absolute: the log-tailing attribution option requires one documented `log_format`.
- **No trust in self-report.** The node's opinion of its own VRRP role is an input, not the truth.
- **Dynamic cluster discovery.** Cluster membership is inferred from the wire where possible, cross-checked against local config — never declared in the exporter's own config.

---

## 2. Prior art — benchmark references, not candidates

Rev 2 framed Layer 1 as adopt-vs-build. Rev 3 drops adoption: the exporter is built in-house, and the Phase 1 decision is now **between our own implementation options** (access-log tailing vs eBPF extraction vs both). External tools remain useful in two ways:

**As accuracy benchmarks in the harness.** `nginx-module-vts` and `prometheus-nginxlog-exporter` are mature per-vhost attributors; running them beside our implementation in the Tier 2 harness gives an independent check on our numbers (in addition to raw access-log truth).

**As prior art on the hard problems.** OBI (OpenTelemetry eBPF Instrumentation, ex-Beyla) and Coroot's node agent demonstrate eBPF L7 parsing techniques worth studying if the eBPF path wins Phase 1 — study, not link.

**VRRP cluster identity remains a genuine gap.** Every keepalived exporter surveyed (`mehdy/keepalived-exporter`, `gen2brain/keepalived_exporter`, `cafebazaar/keepalived-exporter`, textfile scripts) reads keepalived's **self-report** — SIGUSR1/2 dumps or the JSON interface requiring `--enable-json`. None observe VRRP on the wire. The §5 design is still unbuilt in open source.

**The composition is ours:** vhost-attributed traffic joined to wire-derived cluster identity, plus empirical config hygiene — packaged as one boring, standard exporter.

---

## 3. Exporter shape

**One Go binary. One systemd unit. One `/metrics` endpoint on a configurable port.**

Internally, collectors register modularly via the Prometheus `Collector` interface:

| Collector | Status | What it emits |
|---|---|---|
| `config` | always on | `nginx -T` topology parse → `vhost_info`, expected-vhost and expected-upstream lists (§4.1) |
| `workers` | always on | `/proc`-derived worker capacity metrics (§4.4) |
| `ingress` | always on | vhost/backend runtime attribution, built in-house — mechanism chosen in Phase 1 (§4.2–4.3) |
| `vrrp` | **optional** | passive VRRP cluster identity (§5) |

**The `vrrp` module** is enabled by `--vrrp`, or auto-detected: a keepalived process present, or protocol-112 adverts heard within N seconds of startup. When disabled or keepalived is absent, the exporter runs with zero VRRP series and emits:

```
nginx_fleet_vrrp_enabled 0|1
```

so dashboards can adapt. This is goal 5, and the harness proves it (§8).

**Failure isolation:** collectors degrade independently. If the ingress mechanism can't load (no BTF, unreadable logs) or the VRRP socket can't open, the remaining collectors keep serving. A collector failure is itself a metric, never an exporter crash.

**Efficiency (goal 1):** scrape-time cost is reads of in-memory state only. Config re-parses on mtime change / SIGHUP detection with a 60s tick — never `nginx -T` per scrape. VRRP state updates event-driven off the socket. Log tailing (if chosen) is a continuous consumer, not a scrape-time read.

**Label contract:** every collector emits a `node` label carrying the machine hostname exactly as `os.Hostname()` returns it (short name, not FQDN — pick one and enforce it). The §6 joins depend on this being identical across collectors and across the fleet's scrape configs; a mismatch silently empties every cluster rollup.

### Architecture layers

```
┌──────────────────────────────────────────────────────┐
│ L3  Aggregation                                      │
│     per-vhost / per-cluster rollups, capacity model  │
│     recording rules in VictoriaMetrics — not a svc   │
├──────────────────────────────────────────────────────┤
│ L2  Cluster identity            [vrrp — OPTIONAL]    │
│     passive VRRP listener → who is master, per VRID  │
├──────────────────────────────────────────────────────┤
│ L1  Per-node signal                                  │
│     ingress attribution   [in-house, Phase 1 picks]  │
│     config topology parse           [config]         │
│     worker capacity metrics         [workers]        │
└──────────────────────────────────────────────────────┘
```

Each layer is independently useful. Without `vrrp`, L3 cluster rollups degrade to per-node rollups and everything else stands.

---

## 4. Layer 1 — Per-node signal

### 4.1 Config topology parse (`config` collector)

Parse the *running* config — resolve `include` globs, prefer `nginx -T` output so we get the config as nginx actually assembled it. Note `nginx -T` re-reads TLS key files referenced in the config, so the exporter needs read access to them or a sudo rule for that one command — document whichever the fleet standardizes on.

Extract per `server` block:

| Field | Source |
|---|---|
| vhost | `server_name` (all names, incl. wildcards) |
| listener | `listen` — addr, port, `ssl`, `default_server` |
| upstream target | `proxy_pass` / `fastcgi_pass` / `grpc_pass` |
| upstream members | resolved `upstream {}` block, or literal addr |
| source | file + line, for debuggability |

```
nginx_fleet_vhost_info{vhost, listen_addr, listen_port, tls, upstream, upstream_addr, config_file} 1
```

This is the **intended** routing graph. It supplies the *expected* vhost and upstream-member lists that the hygiene metrics (§4.5) diff against runtime observation.

### 4.2 Ingress attribution (`ingress` collector) — the Phase 1 decision

Two in-house mechanisms, evaluated head-to-head in Phase 1:

**(a) Access-log tailing.** A documented `log_format` exposing `$host`, `$upstream_addr`, `$bytes_sent`, `$request_length`, `$upstream_response_time`, `$status`. The exporter tails and aggregates in memory. Per-*request* truth: immune to connection-reuse ambiguity, gives upstream attribution and per-backend status codes for free. Costs: one `log_format` change (the config-coupling exception) and log I/O at volume.

**(b) eBPF SNI/Host extraction.** SNI from the TLS ClientHello (cleartext, pre-crypto; ECH would break this — flag for later) and `Host` from the first plain-HTTP request block, with socket-cookie labelling. Zero nginx config coupling; kernel + BTF floor required.

**Known limitation of (b), by design:** connection-level attribution mis-attributes under **HTTP/2 connection coalescing** (browsers reuse one connection — one SNI — for multiple vhosts sharing a cert/IP, with the real vhost in per-stream `:authority`, which is encrypted) and under plain-HTTP keep-alive carrying different `Host` values per request on one socket. This is a structural argument for (a) as at least a fallback, and it is measured explicitly in the harness (§8): drive coalesced h2 traffic and quantify the error.

**(c) Both**, with logs as ground-truth fallback where BTF/kernel support is missing or coalescing error is unacceptable.

**Required label set, whichever wins:**

```
nginx_fleet_ingress_connections_total{vhost, listen_port, tls}
nginx_fleet_ingress_connections_active{vhost}
nginx_fleet_ingress_bytes_total{vhost, direction}
nginx_fleet_ingress_conn_duration_seconds{vhost}   # histogram
nginx_fleet_ingress_unattributed_total{reason}     # no SNI, no Host, truncated, parse_fail, coalesced
```

`unattributed_total` is non-negotiable: silent mis-attribution is worse than no attribution.

### 4.3 Upstream correlation

The goal: carry the vhost label onto upstream traffic so per-backend volume is attributable.

- **Log path:** `$upstream_addr` + `$host` per request — exact, free. This alone may decide Phase 1 in favor of (a) or (c).
- **eBPF path:** nginx workers multiplex connections, and upstream `keepalive` pools reuse one upstream socket across requests for *different* vhosts. Approaches in order of preference: task-local request context (accurate when keepalive is off/short), statistical attribution (explicitly labelled estimated), log-based ground truth for validation. The `keepalive 0` vs `keepalive 32` accuracy delta is measured in the harness before any correlation logic is written.

### 4.4 Worker capacity metrics (`workers` collector)

From `/proc` and the config, for the headroom math in §6:

```
nginx_fleet_worker_processes                       # configured
nginx_fleet_worker_connections_limit               # configured
nginx_fleet_worker_fds_open{pid}
nginx_fleet_worker_fds_limit{pid}
nginx_fleet_worker_cpu_seconds_total{pid}
nginx_fleet_worker_rss_bytes{pid}
nginx_fleet_listen_backlog_overflows_total
```

### 4.5 Empirical config hygiene (goal 6)

Joins §4.1 intent against §4.2–4.3 observation. All evidence-based.

**Backends actually receiving traffic (goal 3):**

```
nginx_fleet_backend_active{vhost, upstream_addr}    # 1 if observed receiving traffic in the last interval
```

Two drift alerts fall out of the join: configured backends never seeing traffic, and observed traffic to backends absent from config.

**Dead-config detection:**

```
nginx_fleet_vhost_last_traffic_timestamp_seconds{vhost}
nginx_fleet_upstream_last_traffic_timestamp_seconds{vhost, upstream_addr}
```

Absent/0 = never observed. Recording rules turn these into "idle for N days" decommission-candidate lists. **The observation window must span batch and monthly traffic patterns** — do not decommission on a two-week window.

**Upstream delivery verification:** the per-member counters above, split by backend, confirm nginx actually sends traffic to each configured member. A member at zero while its peers serve is itself a signal (down, `backup`, or weight-starved).

**Down-upstream flagging:**

```
nginx_fleet_upstream_up{vhost, upstream_addr}                       # gauge, from empirical failure evidence
nginx_fleet_upstream_failures_total{vhost, upstream_addr, reason}   # connect_error, http_502, http_503, http_504, timeout
```

Derived from per-backend connect errors and 502/503/504s (logs or eBPF, per the Phase 1 winner), plus nginx's passive health state (`max_fails` trips) where observable. Alertable. Whether a mechanism can expose **per-backend** errors and latency — not just per-vhost — is a Phase 1 evaluation criterion.

---

## 5. Layer 2 — Cluster identity via VRRP (`vrrp` module, optional)

**The piece nobody has built. Also the smallest.** Everything in this section is internal to the optional module; with the module off, none of these series exist and the exporter is unaffected.

### 5.1 Why passive observation

| Source | Event-driven | Queryable | Trustworthy | Dependency |
|---|---|---|---|---|
| `notify_*` scripts | yes | **no** | self-report | none |
| keepalived JSON / DBus | no | yes | self-report | `--enable-json` build flag |
| **passive VRRP on the wire** | yes | yes | **ground truth** | raw socket |

Notify scripts are fire-and-forget; the control interface reports only what keepalived believes about itself. **Chosen design:** passive VRRP as authoritative, control interface (where available) as secondary fast signal and cross-check. Disagreement between the two is alertable — usually split brain or a misconfigured VRID.

**Threat-model note:** "ground truth" holds only on a trusted L2 segment. VRRP has no meaningful authentication; protocol-112 packets are forgeable by anything on the segment. That is the standard operating assumption for keepalived itself, but state it: this design detects *keepalived's* misbehavior, not an on-segment attacker.

### 5.2 Wire format and listener

VRRP rides on IP as **protocol 112**, normally multicast to **224.0.0.18** — but keepalived also supports **`unicast_peer` mode**, common where multicast is filtered, and a multicast group join hears nothing there. **Listener design: capture protocol 112 via AF_PACKET/pcap filter rather than a multicast-group raw socket** — it covers both modes for adverts addressed to or through this node. Whether the fleet runs multicast or unicast is open question 3, answered by the same five-second tcpdump as the version question.

Key property: **only the master advertises.** Backups are silent.

**VRRPv2 packet:**

| Offset | Size | Field | Use |
|---|---|---|---|
| 0 | 4 bits | version | **branch on this first** |
| 0 | 4 bits | type | 1 = advertisement |
| 1 | 1 B | **Virtual Router ID** | **the cluster key** |
| 2 | 1 B | **priority** | 255 = address owner, 0 = graceful stepdown |
| 3 | 1 B | addr count | how many VIPs follow |
| 4 | 1 B | auth type | v2 only |
| 5 | 1 B | **advert interval** | seconds (v2) |
| 6 | 2 B | checksum | |
| 8 | 4n B | **VIP list** | which floating IPs this master owns |
| — | 8 B | auth data | v2 only |

**VRRPv3 differs:** IPv6-capable, no auth fields, interval is a 12-bit field in **centiseconds**, and the checksum includes an IP **pseudo-header** (v2's does not). Parsing v3 with v2 offsets yields plausible-looking garbage; validating a v3 checksum with v2 math rejects valid packets. Branch on the version nibble before trusting any offset, and Tier 0 fixtures include a bad-checksum case per version.

### 5.3 Cluster inference and membership

- Nodes advertising the **same VRID on the same segment are one cluster.** Per VRID, track: last advertiser, priority, VIP set, advert interval, last-heard timestamp.
- **Membership needs a second input.** Passive observation reveals *masters* — a backup that has never held the VIP emits nothing, and merely hearing VRID 51's adverts is not membership (an unrelated node on the segment hears them too). `cluster_info` is therefore populated from **each node's own keepalived config / self-report** (which VRIDs this node participates in), **cross-checked against the wire** (which node currently masters them). Wire = authority on *who is master*; local config = authority on *who is a member*. The §6 capacity math needs the standby's existence, so this is not optional.
- **Failover detected locally** when a higher-priority advertiser takes over, or the master goes quiet past the master-down interval (≈ 3 × advert_interval + skew, skew = (256 − priority)/256 × advert_interval).
- **Priority 0 is an early-warning signal** — graceful stepdown in progress; a free heads-up before the VIP moves.

Every node runs the listener and every node hears every (multicast) advert, so VRRP series carry an **`observer` label**. Normally observers agree and dashboards dedupe with `max by (vrid, node)`. Observer *disagreement* is itself a signal: a network partition looks exactly like "observer A hears master X, observer B hears master Y."

```
nginx_fleet_vrrp_master{vrid, node, vip, observer}          # 1 if node advertised as master, per observer
nginx_fleet_vrrp_priority{vrid, node, observer}
nginx_fleet_vrrp_advert_interval_seconds{vrid, observer}
nginx_fleet_vrrp_last_advert_age_seconds{vrid, node, observer}
nginx_fleet_vrrp_transitions_total{vrid, from_node, to_node, observer}
nginx_fleet_vrrp_advert_version{vrid, observer}
nginx_fleet_vrrp_selfreport_mismatch{vrid, node}            # wire vs keepalived disagreement
nginx_fleet_cluster_info{vrid, vip, member_node}            # from local config, wire-cross-checked
```

### 5.4 Failover capture (goal 4) and the transition window

`vrrp_transitions_total{vrid, from_node, to_node}` is the first-class failover event; the requirement is that the **new master is identifiable within one master-down-interval** of the change (harness-asserted, §8).

During failover there is a window where two nodes advertise, or neither. **Rule:** the exporter emits VRRP state as timestamped series, and the aggregation layer treats *"who was master at time T"* as the attribution source of truth — never instantaneous scrape state. Cluster rollups join against `nginx_fleet_vrrp_master == 1`, never sum blindly across nodes.

Split brain (two masters, same VRID, overlap > 1 advert interval) is an alert, not a data-quality workaround.

### 5.5 Active-node identity, generalized (goal 2)

The rest of the system consumes active-node state through one metric, not through VRRP internals:

```
nginx_fleet_active{node, method} 0|1     # method = "vrrp" | "static"
```

With the `vrrp` module on, this derives from master state. `static` covers single-node deployments (active = always). The indirection leaves the door open for other methods (e.g. LB health-check state) without touching L3.

### 5.6 Reusability

The module is nginx-agnostic — it works on any keepalived pair in the estate unchanged. It lives inside the single binary as a collector, but is written as a self-contained package so it can be extracted into a standalone binary later if other teams want it.

---

## 6. Layer 3 — Aggregation

**Do not build a service for this.** VictoriaMetrics recording rules; a stateless rollup is easier to reason about than another daemon. All joins depend on the `node` label contract in §3.

**Cluster-level per-vhost volume** — only the serving node counts (dedupe observers first):

```promql
sum by (vrid, vhost) (
    nginx_fleet_ingress_bytes_total
  * on (node) group_left (vrid)
    (max by (vrid, node) (nginx_fleet_vrrp_master) == 1)
)
```

**Headroom per node.** Each *proxied* request consumes roughly **two** worker connections — the client side and the upstream side both count against `worker_connections`. Subtracting only ingress connections would overstate headroom by up to 2×, and this is the number the move-a-vhost decision rides on:

```promql
  (nginx_fleet_worker_connections_limit * on (node) nginx_fleet_worker_processes)
- on (node)
  (2 * sum by (node) (nginx_fleet_ingress_connections_active))
```

The factor 2 is the conservative bound (static/cached responses use one); refine against observed `worker_fds_open` once real data exists.

**The capacity question this answers:** total per-vhost load across the cluster (which needs the standby's existence from `cluster_info`, §5.3) versus per-node headroom on candidate clusters → "can cluster B absorb `nginx.kliche.com` if we move it?"

**Hygiene rollups (goal 6):** "idle ≥ N days" vhost/upstream lists from the last-traffic timestamps; per-backend failure-rate alerts from `upstream_failures_total`; intent-vs-actual drift alerts from the §4.5 join.

Surface as: vhost volume leaderboard, per-cluster headroom, backend contribution breakdown, VRRP state timeline, decommission-candidate list.

---

## 7. Build order

### Phase 0 — Config parser + VRRP module + worker metrics
**Build.** `nginx -T` parse → `vhost_info`; AF_PACKET VRRP listener → cluster inference, master state, `nginx_fleet_active`; `/proc` worker metrics; the single-binary collector skeleton with `--vrrp` optionality.
**Test:** Tier 0 (golden pcap) + Tier 1 (netns fleet), both built here.
**Delivers:** a genuinely useful standard exporter on its own — fleet topology map, capacity metrics, cluster discovery, failover timeline, split-brain detection.
**No kernel work, no nginx changes, no dependency on Phase 1.** Start here.

### Phase 1 — Ingress mechanism decision (in-house options only)
Stand up the microVM fleet (§8) and run **our two mechanisms** against identical traffic, with access logs as ground truth and VTS/nginxlog-exporter as independent benchmarks:

| Option | Primary question |
|---|---|
| (a) Log tailing | Accuracy is per-request exact — is log I/O at real volume acceptable? |
| (b) eBPF SNI/Host | Attribution error under h2 coalescing and keep-alive; kernel/BTF floor; upstream correlation feasibility |
| (c) Both | Does the operational cost of two mechanisms buy enough over the better single one? |

**Gate:** per-vhost ingress accuracy within **1%** of log-derived truth *on a like-for-like measure* (see §8 — wire bytes vs `$bytes_sent` are different quantities), **and** a usable unattributed-traffic signal, **and** per-backend (not just per-vhost) error/latency visibility for the §4.5 hygiene metrics.

Timebox: two weeks. Cheap against a quarter of eBPF development.

### Phase 2a — Build the chosen ingress mechanism
Log tailer and/or eBPF SNI+Host extraction with socket-cookie labelling, per-vhost counters, `unattributed_total`.
**Delivers:** motivating question #1, hygiene metrics' runtime side.

### Phase 2b — Upstream correlation
Per-backend attribution via the chosen mechanism. If eBPF: the `keepalive 0` vs `keepalive 32` delta (measured in Phase 1) decides task-context correlation vs statistical fallback vs leaning on logs.
**Delivers:** motivating question #2 with honest error bars, `upstream_up` / `backend_active`.

### Phase 3 — Aggregation + capacity model
Recording rules, dashboards, the move-a-vhost headroom calculation, hygiene rollups.
**Test:** the no-double-count-through-failover assertion gates shipping.
**Delivers:** motivating question #3.

### Phase 4 — Hardening
Split-brain alerting, self-report mismatch detection, cardinality guards, Tier 3 shadow-mode soak, multi-cluster rollout.

**Critical path:** Phase 0 and Phase 1 are independent and can run in parallel.

---

## 8. Evaluation and test harness

The harness first decides Phase 1, then becomes the regression suite. Same infrastructure, different job. Hard to test for two reasons: **kernel behaviour** (eBPF, BTF/CO-RE) and **L2 behaviour that only exists when a node dies** (VRRP failover). Neither fakes convincingly in a container sharing the host kernel.

### Tier 0 — Golden pcap, no VM

Capture real VRRP adverts once (`tcpdump -i any proto 112` — the same capture answers open questions 3a/3b and yields the fixtures). Parser unit tests replay: v2 and v3 adverts, priority-0 stepdown, multi-VIP, truncated packets, a malformed v3 packet parsed with v2 offsets to prove the version branch fires, and **a bad-checksum case per version** (v3's pseudo-header checksum differs from v2's). If the fleet runs unicast VRRP, capture unicast fixtures too. Milliseconds, no privileges.

**This is where parser bugs get caught.** Everything below tests integration.

### Tier 1 — Network namespaces, no VM

keepalived runs fine in a netns: N namespaces veth-paired into a Linux bridge = a real L2 segment with real multicast and real master election. Covers cluster inference by VRID, election, graceful failover, preemption, split brain via `ebtables`, and **unicast_peer mode** if the fleet uses it. Seconds in a privileged CI container, no KVM.

**Bridge gotcha:** Linux bridge IGMP snooping can drop unregistered multicast — set `multicast_snooping=0` or adverts silently vanish.

Tiers 0–1 are built alongside Phase 0.

### Tier 2 — MicroVM fleet

**Phase 1 job:** run mechanisms (a) and (b) against identical known traffic; measure accuracy. **Phase 2+ job:** kernel matrix and full failover semantics.

**Why microVMs:** each VM gets its own kernel — eBPF behaviour must be proven across the kernel versions the fleet actually runs. **Runtime:** Firecracker (sub-second boot, kernel as a swappable `vmlinux` path), driven via `firecracker-go-sdk`. cloud-hypervisor as fallback, QEMU `microvm` for anything exotic.

**Topology** — every VM on a tap, all taps on one bridge:

```
        ┌──── br-test (multicast_snooping=0) ────┐
        │        │        │        │        │
     nginx-1  nginx-2  nginx-3  backend  client(s)
     VRID 51  VRID 51  VRID 52   .1.1.100
     VIP .100 VIP .100 VIP .101
```

nginx-1/2: cluster under test. nginx-3: a *different* VRID, proving inference partitions rather than merges. backend: the shared IP many vhosts point at — this is what we degrade. client: traffic generator with controlled SNI/Host values.

Host-side `tcpdump` on `br-test` is the **independent oracle** — failover timing measured from outside both VMs. Per-VM identity via Firecracker MMDS: one rootfs, N configurations.

### Tier 2 — evaluation matrix (Phase 1)

Drive known traffic with distinct SNI/Host values across N vhosts sharing one backend. Compare each mechanism against access-log truth (`$host`, `$upstream_addr`, `$bytes_sent`) **on a like-for-like measure**: eBPF sees wire bytes (TLS records, handshakes, retransmits); logs record L7 payload bytes. A correct eBPF implementation fails a naive 1% byte comparison structurally. Compare request counts or plaintext-HTTP bytes for the equality gate, and treat the wire-vs-L7 byte *ratio* as a consistency check.

| Criterion | Gate |
|---|---|
| Per-vhost accuracy vs log truth | within **1%** over 60s, like-for-like measure |
| Vhost identity preserved | distinct series per vhost, never collapsed |
| **h2 coalescing / keep-alive error** | drive coalesced h2 + mixed-Host keep-alive traffic; mis-attribution quantified, surfaced in `unattributed_total{reason="coalesced"}` |
| Unattributed traffic visible | `ingress_unattributed_total` populated with correct reasons |
| Per-backend visibility | errors and latency attributable per backend member, not just per vhost (§4.5 needs this) |
| Upstream attribution | measured at `keepalive 0` vs `keepalive 32` |
| Wildcard `server_name` | cardinality bounded under high-cardinality SNI |
| Config coupling | (a): exactly the documented `log_format`, nothing more; (b): zero |
| Operational cost | CPU/memory at load; (a): log I/O; (b): kernel floor |

### Tier 2 — regression matrix (Phase 2+)

**Kernel matrix** (same rootfs, swapped `vmlinux`):

| Kernel | Asserts |
|---|---|
| Fleet production version | everything passes |
| Oldest supported | probes load, or degrade cleanly |
| No BTF / CO-RE unavailable | exporter **starts and serves Phase 0 metrics** with eBPF disabled |

**Module matrix** (goal 5):

| Configuration | Asserts |
|---|---|
| `vrrp` on, keepalived present | full VRRP series, `vrrp_enabled 1` |
| **`vrrp` off / keepalived absent** | **exporter fully functional, zero VRRP series, `vrrp_enabled 0`, all other collectors unaffected** |
| `vrrp` auto-detect, keepalived stopped mid-run | VRRP series go stale/absent; ingress, config, workers keep serving |

**Failover scenarios:**

| Scenario | Injection | Assert |
|---|---|---|
| Graceful stepdown | stop keepalived on master | priority-0 advert surfaced *before* the VIP moves |
| Hard death | kill the Firecracker process | new master identified within master-down-interval; `transitions_total{from_node, to_node}` incremented once |
| Freeze (not death) | Firecracker `Pause` | same as hard death |
| Network partition | remove tap from bridge | split brain: both advertise; **observer labels disagree**; `selfreport_mismatch` fires |
| Preemption | raise backup priority via MMDS + reload | takeover detected, transition counted once |
| Flapping | repeated stop/start at sub-interval rate | no metric corruption, no negative counters, accurate transition count |
| v2 vs v3 | keepalived configured for each | parser branches; seconds vs centiseconds correct; per-version checksum validates |
| Multicast vs unicast | `unicast_peer` config | AF_PACKET listener sees adverts in both modes |

**The no-double-count test** matters most: known byte volume from the client *through* a failover; cluster rollup equals client-sent bytes within threshold. Dropping and double-counting both fail it — exactly the §5.4 risk.

**Backend degradation:** `tc netem` on the backend's tap; assert latency increase attributes to the vhosts driving it, and that a fully-failed member flips `upstream_up` to 0 with the right `reason` — motivating question #2 and goal 6 tested directly.

### Harness notes

- **Snapshots:** restored Firecracker VMs resume with a **stale clock**, wrecking VRRP timing. Snapshot with nginx running but keepalived stopped; start keepalived after resume + clock resync. Backwards ordering produces intermittent failures that look exactly like exporter bugs.
- **KVM in CI:** Firecracker needs `/dev/kvm`; shared runners rarely have it. `.metal` instance for Tier 2, or run it nightly with Tiers 0–1 on the PR path.
- **Rootfs:** one base image (nginx + keepalived + benchmark exporters + ours) via `mkosi` or debootstrap, per-VM overlay.
- **Timing assertions need slack:** assert "within master-down-interval + one advert interval," never an exact millisecond.

### Tier 3 — Staging fleet

Real hardware, real traffic, shadow mode, no alerting. Compare against access logs for a week — long enough to catch the traffic patterns the hygiene metrics depend on — before anything depends on it.

---

## 9. Implementation notes

- **Language:** Go, standard library + minimal deps. Raw AF_PACKET socket + hand-rolled parser for VRRP (gopacket is more dependency than a 20-field struct warrants). `cilium/ebpf` only if Phase 1 picks the eBPF path.
- **Single binary, modular collectors.** One deployable, one systemd unit, collectors as self-contained packages behind the `Collector` interface. The `vrrp` package stays nginx-agnostic and extractable into a standalone binary later if other teams want it — modularity gives the isolation rev 2 wanted from two binaries without two deployables.
- **Deployment:** systemd units on the nginx hosts. Not a DaemonSet — these are VMs.
- **Privileges:** `CAP_NET_RAW` for the VRRP socket; `CAP_BPF` + `CAP_PERFMON` only if eBPF wins Phase 1; read access to nginx config **and the TLS key files `nginx -T` touches** (or a scoped sudo rule for that command) — the one place "no root" needs a footnote.
- **Kernel floor:** confirm before Phase 1 — may decide the mechanism choice before the harness does.
- **Cardinality:** vhost × node × direction, bounded and small; cap distinct vhost labels with an `_other` bucket and alert on approach. Wildcard `server_name` is the risk. The `observer` label multiplies VRRP series by cluster size — fine for pairs, cap for anything larger.
- **Failure isolation:** any collector failing to load or dying leaves the others serving. Proven by the module matrix in §8.

---

## 10. Open questions

**Blocking Phase 0/1 — minutes of work; close these first:**

1. **Kernel version + BTF availability** on the nginx hosts. Gates the eBPF option entirely.
2. **Is upstream `keepalive` enabled** in current configs? One grep. Decides whether eBPF upstream correlation is tractable or statistical — a big thumb on the Phase 1 scale.
3. **One tcpdump answers two questions:** (a) VRRP **v2 or v3**, (b) **multicast or unicast_peer**. `tcpdump -i any proto 112` for five seconds; the capture doubles as the Tier 0 fixtures.

**Blocking Phase 2:**

4. **Wildcard vhosts** — how many, and per-SNI breakdown or one bucket?
5. **Log volume at peak** — if log tailing wins Phase 1, what's the real lines/sec, and does the documented `log_format` change need a rollout process?

**Planning:**

6. **`.metal` runner availability** for Tier 2, or nightly on a dedicated box?
7. **How many distinct kernel versions in the fleet?** More than three argues for CO-RE if eBPF wins.
8. **Other keepalived pairs in the estate?** The `vrrp` package works on them unchanged — may change who else wants it and whether the standalone extraction (§9) happens sooner.
9. **Hygiene observation window** — what is the longest legitimate traffic period (monthly batch? quarterly?) that must pass before "idle" means "decommission candidate"?

---

## 11. Core principle

> **Observe the network, don't interrogate the box.**

Config parsing tells us intent. The wire tells us truth. Self-report — keepalived's or nginx's — is an input to be verified, never a source of authority. The hygiene goal is the same principle applied to configuration: a vhost exists when traffic proves it, not when a config file claims it.

**Corollary, revised in revision 3:** own the whole exporter, keep it boring on the outside. One binary, one `/metrics`, standard exposition — the novelty (wire-derived cluster identity, empirically-verified topology) lives behind the most conventional interface possible.
