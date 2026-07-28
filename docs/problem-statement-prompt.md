# Objective
I want you to act as a senior software architect and reverse engineer.
**Do NOT write any production code yet.** The goal of this phase is investigation and feasibility analysis. I only want to know whether this project is realistically possible and what the best architecture would be.
---
# Background
I use:
* Readest as my EPUB reader on all my devices.
* BookOrbit as my self-hosted library server.
There is currently no direct integration between them.
However, I discovered the following workflow already works:
Readest
→ Readest Sync
→ KOReader (Readest plugin imports progress)
→ KOReader (BookOrbit plugin exports progress)
→ BookOrbit
This proves that KOReader can successfully act as a bridge between the two systems.
My goal is to eliminate KOReader entirely.
Instead, I want a standalone service (daemon/CLI) that synchronizes Readest progress directly into BookOrbit.
Desired architecture:
Readest
→ Readest Sync
→ Standalone Bridge
→ BookOrbit
The bridge should perform whatever API calls or synchronization logic the KOReader plugins currently perform, without requiring KOReader itself. For now, I want the MVP as a one-way sync from Readest to BookOrbit. Readest should be the source of truth. BookOrbit is only used to reflect reading progress in my self-hosted library, so I don't need BookOrbit to push progress back to Readest. We can consider two-way sync in the future if there's a compelling use case.
---
