# Blockers

Format: date, task, description, what was tried, what would unblock it.

## Open

- 2026-08-28 · T16/T21 · **rasputin002 and rasputin004 lack passwordless
  sudo** for user `ericovis` (`sudo -n true` fails). Remote adoption/flash
  triggering needs it. Unblock: owner runs on each node:
  `echo 'ericovis ALL=(ALL) NOPASSWD:ALL' | sudo tee /etc/sudoers.d/ericovis`
  (they will be prompted for their password once). Not automatable from here.
- 2026-08-28 · T21 · **rasputin003 is offline** (`rasputin003.local` does not
  resolve; MAC b8:27:eb:07:08:09 never seen on the LAN during planning).
  Unblock: owner powers it on / checks cabling. Possibly related to the
  historical duplicate-hostname issue.

## Design concerns

- 2026-08-28 · T05 · `ModeError` (a flag file with no URL and no baked-in
  default) is implemented as specified: the agent logs forever and never
  touches the card. Consequence: such a node stays in the initramfs and does
  not boot until someone power-cycles it after clearing the flag. Booting
  normally after logging the error would be equally safe for the card and
  would leave the node reachable. Left as specified; flag files written by
  the CLI always carry a URL, so this only bites a hand-made empty flag on a
  build with no default URL.
