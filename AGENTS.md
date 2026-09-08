# AGENTS.md

Two documents matter, depending on what you are here to do.

- **Operating the cluster with this tool** (flashing, checking, syncing):
  read [`cmd/rasputin/MANUAL.md`](cmd/rasputin/MANUAL.md), or run
  `rasputin manual`. It says what every command touches, which ones wipe SD
  cards, and the exact JSON each command emits with `-json`. Use `-json` for
  anything you parse, `status` and `sync -plan` to look before acting, and
  **never run `bake`, `flash` or `sync -yes` without the owner's explicit
  approval of the plan**.
- **Changing the code**: read [`CLAUDE.md`](CLAUDE.md) (traps, ground rules,
  the JSON/manual contract) and then [`README.md`](README.md) for the design.

`go test ./...` runs without hardware. Nothing in this repository pushes to a
remote.
