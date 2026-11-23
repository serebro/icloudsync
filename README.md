# icloudsync

[![CI](https://github.com/serebro/icloudsync/actions/workflows/ci.yml/badge.svg)](https://github.com/serebro/icloudsync/actions/workflows/ci.yml) [![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

A Go CLI that signs in to iCloud (Apple ID + password + 2FA code), keeps the session in a file for as long as Apple accepts it, and on every run mirrors **iCloud Drive**, **iCloud Photos** (personal and Shared Library) and **Shared Albums** to an external drive.

> It uses the unofficial iCloud web API, the same one used by icloud.com and by pyicloud/icloudpd. Apple can change it at any time.

> **Status:** tested on a real Apple ID: 2FA sign-in, Drive, the personal photo library and Shared Albums. SRP is checked against a reference Python implementation with test vectors. Run the first `sync` to a new drive with `--dry-run`.
>
> **Not yet verified against live iCloud** (covered by unit tests with a fake server only): `--size` renditions other than `original`, `--xmp-sidecar`, `--albums`/`--album`, `--recent`/`--until-found` (including which end of the library they start from), and the system keychain. Try them with `--dry-run` first and check the result.

This project is not affiliated with or endorsed by Apple Inc. "iCloud" is a trademark of Apple Inc.

Русская версия: [README.ru.md](README.ru.md).

## Build

```sh
go build -o icloudsync .
```

Homebrew (macOS and Linux):

```sh
brew tap serebro/icloudsync https://github.com/serebro/icloudsync
brew trust serebro/icloudsync
brew install icloudsync
```

Recent Homebrew versions refuse to load formulae from a third-party tap until you trust it (`Refusing to load formula ... from untrusted tap`). `brew trust serebro/icloudsync` does that; to trust only this formula use `brew trust --formula serebro/icloudsync/icloudsync`.

Or install with `go install github.com/serebro/icloudsync@latest`, or download a binary from the [Releases](https://github.com/serebro/icloudsync/releases) page.

Requires Go 1.26+. macOS and Linux are supported.

## Usage

```sh
# 1. First sign-in (interactive: password, then a 6-digit code from your iPhone/Mac or via SMS)
icloudsync login --apple-id you@example.com

# 2. Sync (run as often as you like)
icloudsync sync --dest /Volumes/Backup/icloud

# Only show what would change
icloudsync sync --dest /Volumes/Backup/icloud --dry-run

# List your own albums, then mirror some of them
icloudsync albums
icloudsync sync --dest /Volumes/Backup/icloud --only-photos --album "Family" --album "Trips/Rome"

# Fast incremental run: stop after 50 photos that are already up to date
icloudsync sync --dest /Volumes/Backup/icloud --only-photos --until-found 50

# Check the session / sign out
icloudsync status
icloudsync logout
```

`sync` flags:

| flag | default | |
|---|---|---|
| `--dest DIR` | `$ICLOUDSYNC_DEST` | folder on the external drive; **must exist** |
| `--only-drive` / `--only-photos` | | sync only Drive, or only Photos (libraries and Shared Albums); without either flag everything is synced |
| `--dry-run` | | write and delete nothing (not even the `.icloudsync` folder) |
| `--no-delete` | | keep files that were removed from iCloud |
| `--force-delete` | | lift the >25% mass-deletion safeguard (an empty listing still never deletes anything) |
| `--trash` | | move files removed from iCloud to `.icloudsync/trash/<time>/` instead of deleting them |
| `--trash-days N` | 0 | with `--trash`: purge trash batches older than N days (0 = keep forever) |
| `--workers N` | 4 | parallel downloads |
| `--debug-log FILE` | | HTTP trace; tokens, cookies and scnt are redacted |

Photo flags:

| flag | default | |
|---|---|---|
| `--size LIST` | `original` | comma-separated renditions: `original`, `adjusted` (the edited version), `alternative` (the JPEG of a RAW+JPEG pair), `medium`, `thumb` |
| `--force-size` | | skip a photo that lacks a requested rendition instead of falling back to its original |
| `--xmp-sidecar` | | write an `.xmp` sidecar (caption, description, keywords, location, favorite) for photos that have such metadata |
| `--albums` / `--album NAME` | | mirror all of your own albums, or the named ones (repeatable; `Folder/Album` or just the album name) into `Photos/Albums/` |
| `--recent N` | | list only the first N photos of each library and of each album |
| `--until-found N` | | stop after N consecutive photos that are already up to date |

Notification flags (see [Notifications](#notifications)): `--notify-script`, `--notify-email`, `--notify-email-from`, `--smtp-host`, `--smtp-port`, `--smtp-username`, `--smtp-no-tls`, `--notify-on`.

Common flags: `--apple-id` (`$ICLOUD_APPLE_ID`), `--session FILE` (`$ICLOUDSYNC_SESSION`).

## Layout on disk

```
<dest>/
  Drive/...                                         folder tree as in iCloud Drive
  Photos/Primary/2024/05/17/IMG_0001.HEIC           personal library
  Photos/Primary/2024/05/17/IMG_0001_HEVC.MOV       video part of a Live Photo
  Photos/Shared/2024/05/17/IMG_0002.JPG             iCloud Shared Photo Library
  Photos/SharedAlbums/<album>/2024/05/17/IMG_0003.JPG   Shared Albums (owned and subscribed)
  Photos/Albums/<folder>/<album>/2024/05/17/IMG_0001.HEIC   your own albums (--albums / --album)
  Photos/Primary/2024/05/17/IMG_0001.HEIC.xmp       metadata sidecar (--xmp-sidecar)
  Photos/Primary/2024/05/17/IMG_0001-adjusted.JPG   edited version (--size original,adjusted)
  .icloudsync/manifest.json                         which files belong to the program
  .icloudsync/trash/<time>/...                      files removed from iCloud (--trash)
```

## How the sign-in is stored

- Session file: `~/Library/Application Support/icloudsync/session.json` on macOS, `~/.config/icloudsync/session.json` on Linux. Mode 0600, directory 0700. It holds cookies, the session token and the 2FA trust token.
- Apple decides how long the session lives; the program does not invent a lifetime. Each run validates the session. If Apple rejects it, the program signs in again with the password and the trust token, usually without a 2FA code. Once the trust expires too (typically after 1–2 months), run `icloudsync login` in a terminal again.
- The password is **not stored** by default. It is taken from, in this order:
  - `$ICLOUD_PASSWORD`;
  - the system keychain: `login --keyring` stores it in the macOS Keychain (Linux: the Secret Service, e.g. GNOME Keyring or KeePassXC). If the keychain is unavailable (cron, ssh, a locked keychain) the next source is tried;
  - `login --save-password`: the password is stored **in plain text** in the session file (0600), for unattended runs on a machine without a keychain;
  - a terminal prompt.
- `--keyring` and `--save-password` are mutually exclusive; storing in the keychain removes an older plain-text copy. `logout` removes the keychain entry too (unless `--keep-password`), and `status` shows where a password is kept.
- The password is never accepted as a command-line flag (it would end up in shell history and `ps`).
- At most one sign-in attempt is made per run. If Apple rejects a stored password, it is removed from the file, so a scheduled job cannot lock the Apple ID with repeated attempts.

## Mirroring and data safety

- Transient network and Apple errors (DNS timeouts, 5xx, `TRY_AGAIN_LATER`) are retried with backoff for up to about 2.5 minutes.
- Only new and changed files are downloaded (etag/checksum and size are compared). A file renamed in Drive is moved on disk without downloading it again.
- **Only** files listed in `manifest.json`, i.e. downloaded by this program, are ever deleted. Other files in `--dest` are left alone.
- Deletions happen only if the service's listing was complete and error-free.
- Deletions are blocked if more than 25% of the files (and more than 50) would disappear; `--force-delete` lifts this. If iCloud returns an empty listing, nothing is ever deleted, even with `--force-delete`.
- Files that the program does not own are never overwritten: if such a file sits where an iCloud file should go, the iCloud file gets a ` (2)` suffix.
- Every 30 seconds each service prints a progress line, so long runs that mostly skip unchanged files do not look stuck.
- If `--dest` does not exist, the program exits with an error and does not create it; otherwise an unmounted `/Volumes/X` would become a folder on the internal disk.
- The manifest records the Apple ID, so a different account cannot be mirrored into the same `--dest`.
- Each file is first written to a temporary `.icloudsync-*.part`, its size is checked, then it is renamed into place. The manifest is saved every 30 seconds and on Ctrl+C, so an interrupted first run continues where it stopped.
- Names are cleaned up for exFAT/FAT32/NTFS (`: * ? " < > |` etc.). Names that collide case-insensitively get a ` (2)` suffix. FAT32 cannot hold files over 4 GB; the program reports it, so prefer exFAT or APFS.

## Notifications

An unattended run cannot ask for a 2FA code, so it exits with code 3 and, if configured, tells you:

```sh
icloudsync sync --dest /Volumes/Backup/icloud \
  --notify-script ~/bin/icloud-alert.sh \
  --notify-email me@example.com --smtp-host smtp.example.com --smtp-username me@example.com
```

- `--notify-script PROG` runs `PROG` with `ICLOUDSYNC_EVENT`, `ICLOUDSYNC_MESSAGE` and `ICLOUDSYNC_APPLE_ID` in its environment.
- `--notify-email ADDR` sends a message through `--smtp-host` (`--smtp-port` 587 uses STARTTLS, 465 implicit TLS; `--smtp-no-tls` is for a trusted local relay only). The SMTP password is read from `$ICLOUDSYNC_SMTP_PASSWORD`, never from a flag.
- `--notify-on` lists the states to announce: `interaction` (exit 3, the default), `partial` (exit 4), `error` (exit 1). Ctrl+C and usage errors never notify.
- A failing notification is printed as a warning and never changes the exit code.

## Exit codes (for cron/launchd)

| code | meaning |
|---|---|
| 0 | everything is synced |
| 1 | error (including the drive not being mounted) |
| 2 | invalid arguments |
| 3 | a human is needed: the sign-in expired, or a 2FA code or the password is required. Run `icloudsync login` |
| 4 | finished, but some files failed or deletions were blocked |
| 130 | interrupted with Ctrl+C |

## Photo options in detail

- **Sizes.** `--size original,adjusted` downloads both the original and the edited version (`IMG_1.HEIC` and `IMG_1-adjusted.JPG`; the extension follows the rendition's own type). `medium` and `thumb` always get a suffix, `adjusted` and `alternative` only next to the original. A photo without an edit uses its original instead of the missing rendition unless `--force-size` is set. Videos are always the original; shared albums are not affected by `--size`. The Live Photo video is stored with the full-size renditions.
- **Changing `--size` later** replaces the main file of each photo in place under the same identity; extra files from a rendition you stopped asking for are removed like any file that left iCloud (use `--trash` or `--no-delete` if you want to keep them). `--force-size` therefore removes photos that lack the rendition from an existing mirror.
- **Sidecars** are generated locally (deterministic, so unchanged metadata is not rewritten). Only photos with a caption, keywords, location, favorite flag or screenshot marker get one. Running without `--xmp-sidecar` removes the sidecars again; photos are kept.
- **Albums.** A photo that is in your library and in an album is stored once per tree. Albums inside folders become nested folders. A requested album that does not exist is an error and blocks deletions.
- **`--recent` / `--until-found`** make the listing partial on purpose for the libraries and your own albums: nothing is deleted in such a run, and the item-count check is skipped. Shared Albums and Drive are always listed in full. `--until-found N` counts photos (a Live Photo and its video count once).
- **Trash.** `--trash` applies to files that disappeared from iCloud only, and the 25% / empty-listing safeguards still apply. `--dry-run` creates nothing.

## Limitations

- **Advanced Data Protection.** With ADP on, "Access iCloud Data on the Web" must be enabled in the Apple ID settings, otherwise Drive and Photos are unavailable.
- China-region accounts (`icloud.com.cn`), shared Drive folders and hardware security keys (FIDO) are not supported.
- Shared Albums keep reduced-size copies (photos up to 2048 px, videos up to 720p); that is all Apple serves to subscribers, there are no originals. A photo that is both in the library and in a Shared Album is downloaded twice. A renamed album is moved to its new folder without downloading again.
- Photos are downloaded as the original unless `--size` says otherwise. RAW+JPEG pairs are not aligned (`alternative` gives the other half). Photos in Recently Deleted and Hidden are not mirrored.
- Drive packages (`.app`, `.textbundle`, `.workflow`, `.rtfd`, …) are stored as unpacked folders, as on a Mac. iCloud serves them as zip archives, so a changed package is downloaded and replaced as a whole.

## License

[MIT](LICENSE)
