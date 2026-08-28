# Blockers

Format: date, task, description, what was tried, what would unblock it.

## Open

- 2026-08-28 · T16/T21 · **rasputin002, rasputin003 and rasputin004 lack
  passwordless sudo** for user `ericovis` (`sudo -n true` fails; 003 confirmed
  2026-08-28 at 192.168.0.222). Remote adoption/flash triggering needs it.
  Unblock: the owner runs, on each of the three nodes:
  `echo 'ericovis ALL=(ALL) NOPASSWD:ALL' | sudo tee /etc/sudoers.d/ericovis`
  (they will be prompted for their password once). Not automatable from here.
  rasputin001 already has it, so the whole bake path (T16–T20) is unblocked;
  only T21 — adopting the remaining three — needs this.
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
