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

## Public documentation and local data

Keep published docs focused on installation, supported behavior, operation, and
compatibility. Put development diaries, discussions, machine-specific service
notes, and private research in ignored `.local-docs/`. Never commit real configs,
credentials, player records, saves, or build artifacts. Preserve existing local
data when removing it from Git tracking. Deployment documentation belongs in the
backend repository; its packages include the official frontend.
