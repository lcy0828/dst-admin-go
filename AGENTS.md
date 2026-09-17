# DST Admin product constraints

Before changing architecture, background work, capacity rules, deployment, or
multi-machine UI, read `docs/product-usage-scenarios.md`.

The dominant deployment is one 2 vCPU / 4 GiB All-in-One host running one Room
with Master and Caves. Optimize this path first. Multi All-in-One management,
one Controller with remote Agents, and one Room split across machines must
extend the same Runtime and Room contracts without adding steady-state work to
the single-host path.

Keep the local Runtime in process. Product behavior, errors, operation results,
and resource presentation should match remote Agents, while local calls and
remote WebSocket transport remain separate adapters. Background work must be
demand-driven, coalesced, bounded, and inactive when its target is unused.
