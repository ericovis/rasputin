# Blockers

Format: date, task, description, what was tried, what would unblock it.

## Open

(none — every blocker is resolved.)

## Resolved

- 2026-08-29 · T21 · ~~rasputin002/003/004 lack passwordless sudo~~ —
  **RESOLVED.** The owner granted it 2026-08-29 ~05:10; all three then passed
  `sudo -n true` and T21 completed. Original entry kept below for the record.
  (Also mitigated in code: `ssh.sudo: password` now lets the CLI manage nodes
  that never receive the grant.)

- 2026-08-29 · T21 · (resolved) rasputin002, rasputin003 and rasputin004 lacked
  passwordless sudo for user `ericovis` (`sudo -n true` fails; 003 confirmed
  2026-08-28 at 192.168.0.222). Remote adoption/flash triggering needs it.
  Unblock: the owner runs, on each of the three nodes:
  `echo 'ericovis ALL=(ALL) NOPASSWD:ALL' | sudo tee /etc/sudoers.d/ericovis`
  (they will be prompted for their password once). Not automatable from here.
  rasputin001 already has it, so the whole bake path (T16–T20) is DONE;
  only T21 — adopting and flashing the remaining three — needs this.
  Re-checked 2026-08-29 00:26: all three still refuse `sudo -n true`.

  **Exact commands for the owner** (each prompts once for the ericovis
  password; run them from this Mac):

      ssh ericovis@rasputin002.local "echo 'ericovis ALL=(ALL) NOPASSWD:ALL' | sudo tee /etc/sudoers.d/ericovis"
      ssh ericovis@192.168.0.222     "echo 'ericovis ALL=(ALL) NOPASSWD:ALL' | sudo tee /etc/sudoers.d/ericovis"
      ssh ericovis@rasputin004.local "echo 'ericovis ALL=(ALL) NOPASSWD:ALL' | sudo tee /etc/sudoers.d/ericovis"

  Note rasputin003 must be reached by IP (192.168.0.222): its hostname is
  still the duplicate `rasputin002`, so `rasputin003.local` does not resolve.
  Flashing it is what fixes the name permanently.

  Once done, the rest of T21 is one command per node and needs no further
  decisions:

      go run ./cmd/rasputin adopt rasputin002 && go run ./cmd/rasputin flash rasputin002
      go run ./cmd/rasputin adopt rasputin003 && go run ./cmd/rasputin flash rasputin003
      go run ./cmd/rasputin adopt rasputin004 && go run ./cmd/rasputin flash rasputin004
      go run ./cmd/rasputin flash rasputin002 rasputin004   # parallel exercise
      go run ./cmd/rasputin status
- 2026-08-28 · T21 · ~~rasputin003 is offline~~ — **RESOLVED, was wrong.**
  T15 found it up at 192.168.0.222 with MAC b8:27:eb:07:08:09, reachable over
  SSH as `ericovis`. It does not resolve as `rasputin003.local` because its
  hostname is set to `rasputin002` (duplicate hostname); it is findable only
  via the ARP table, which the resolver now tries. Flashing it will fix the
  name permanently via the baked identity service. It still needs the sudo
  fix below (it is one of the three nodes without passwordless sudo).

## Design concerns

- 2026-08-28 · T05 · `ModeError` (a flag file with no URL and no baked-in
  default) is implemented as specified: the agent logs forever and never
  touches the card. Consequence: such a node stays in the initramfs and does
  not boot until someone power-cycles it after clearing the flag. Booting
  normally after logging the error would be equally safe for the card and
  would leave the node reachable. Left as specified; flag files written by
  the CLI always carry a URL, so this only bites a hand-made empty flag on a
  build with no default URL.
