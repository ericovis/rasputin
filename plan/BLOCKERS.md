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

(none yet)
