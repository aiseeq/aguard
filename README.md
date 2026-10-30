# aguard

Agent guardian: one daemon that watches all coding agents on the machine — their resources, their boundaries, and whether the machine has enough of everything they need.

Work in progress, private. Design and decisions: `docs/design.md` (Russian).

Night watch (`night`): once a minute it checks that the night run started — the dream marker by 02:30, today's date in the digest marker by 07:00, and that no session has been stuck on the trust dialog for 5 minutes. A miss is a message to Claude sessions only (the lead and `night.also_to`; the dream signal also goes to `night.dream_to`, the owner of the night run), naming the links that could have failed and the next step. Details: `docs/design.md`, «Устройство», `night`.
