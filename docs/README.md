# Documentation

This directory holds the design and implementation documentation for Secret Media Bot. The [README](../README.md) is the entry point: it covers quickstart, configuration, usage, development, and CI/CD.

| Document | Audience | Contents |
| --- | --- | --- |
| [README](../README.md) | Everyone | What the bot does, quickstart, configuration, commands, development, CI/CD, caveats. |
| [architecture.md](architecture.md) | Developers and reviewers | Component boundaries, data model, state machines and concurrency, retention and deletion, privacy and threat model, deployment posture. |
| [runbooks.md](runbooks.md) | Operators | Operational runbooks for backups, restores, key rotation, and incident response. |
| [live-validation.md](live-validation.md) | QA and testers | Manual validation checklist against live Telegram Bot API. |
| [progress.md](progress.md) | Maintainers | Implementation progress log against the improvements backlog. |
| [improvements.md](improvements.md) | Maintainers | Prioritized top-25 improvement backlog from the full-project review. |
| [telegram-media-whisper-v1.md](telegram-media-whisper-v1.md) | Reviewers | Historical initial V1 build specification. |

Suggested reading order for a new developer or reviewer:

1. [README](../README.md) — what the project is and how to run it.
2. [architecture.md](architecture.md) — how it is built and why.
3. [telegram-media-whisper-v1.md](telegram-media-whisper-v1.md) — the requirements the code implements.

## Keeping this in sync

- Any change that alters the data model, state machines, or trust boundaries must update [architecture.md](architecture.md).
- Any change to how the project is built, tested, or deployed must update the relevant README section and the CI workflow in `.github/workflows/ci.yml`.
- Any change to a documented default must update `.env.example` and the configuration tables in the README.