# Starbound Panel

A small self-hosted web panel for a **Starbound dedicated server**: start and
stop the server, watch the console, see who is online, manage Steam Workshop
mods and keep scheduled backups — all from a browser.

It is a single Go binary with an embedded UI (no Node.js build step, no
database) that runs the game server as its child process. Think of it as the
Starbound counterpart of panels such as `dst-admin-go`.

```
┌───────────────────────────── one container / one pod ─────────────────────────────┐
│  starbound-panel (Go)                                                             │
│    ├─ HTTP API + embedded UI        :8080                                         │
│    ├─ supervises ─────────────────► linux/starbound_server   :21025               │
│    ├─ runs steamcmd                 install / update / workshop downloads         │
│    └─ writes tar.gz backups         /backup                                       │
└───────────────────────────────────────────────────────────────────────────────────┘
```

## Features

- **Server control** — start, stop (graceful SIGINT so the universe is saved),
  restart, crash detection with automatic restart and backoff.
- **Live console** — streamed over SSE, with command input and quick commands.
- **Player list** — derived from the server console (`Client 'Name' <id> (ip)
  connected` / `disconnected`), no server mod required.
- **Scheduled backups** — `tar.gz` of the save directory (which also holds
  `starbound_server.config`), interval + retention, manual "backup now",
  download and delete from the UI.
- **Mod management** — add a Workshop id, the panel downloads it with steamcmd,
  copies the `.pak` files into `mods/`, and can enable/disable/remove them.
  Disabling moves files out of the mods directory, so it survives restarts.
- **Server config editor** — edit `starbound_server.config` in the browser.
- **Single administrator account** — first-run setup, PBKDF2 password hash,
  cookie sessions, login rate limiting.

## Quick start

```bash
docker run -d --name starbound-panel \
  -p 8080:8080 -p 21025:21025/tcp \
  -v "$PWD/data:/data" \
  -v "$PWD/backups:/backup" \
  -e TZ=Asia/Shanghai \
  ghcr.io/scottl97/starbound-panel:0.4.6
```

Then open <http://localhost:8080> and:

1. create the administrator account (first start only);
2. open **Settings → Steam account** and enter the Steam account that owns
   Starbound (the dedicated server cannot be downloaded anonymously);
3. press **Test login** to check the credentials. If the account uses the
   Steam mobile authenticator, paste the current 5-character code or approve
   the prompt in the Steam app within ~90 seconds;
4. press **Install / update**. The panel asks for a Steam Guard code again —
   steamcmd logs in for every run and no sentry file is kept, so paste a fresh
   code (or leave it empty and approve on the phone). Then watch the console:
   steamcmd downloads a few GB into the data volume;
5. press **Start** and connect to `host:21025`.

`docker-compose.example.yml` is in [`deploy/`](deploy/docker-compose.example.yml).

## Configuration

All settings are environment variables.

| Variable | Default | Purpose |
|---|---|---|
| `SB_LISTEN` | `:8080` | HTTP listen address |
| `SB_DATA_DIR` | `/data` | Root of all persistent state |
| `SB_SERVER_DIR` | `$SB_DATA_DIR/server` | Dedicated server installation |
| `SB_STORAGE_DIR` | `$SB_DATA_DIR/storage` | Saves (symlinked as `<server>/storage`) |
| `SB_STATE_DIR` | `$SB_DATA_DIR/state` | Panel state (`state.json`, mode 0600) |
| `SB_BACKUP_DIR` | `$SB_DATA_DIR/backups` | Backup archives |
| `SB_MODS_DISABLED_DIR` | `$SB_DATA_DIR/mods-disabled` | Disabled mods |
| `SB_STEAMCMD` | `/data/steamcmd/steamcmd.sh` | steamcmd executable, kept on the data volume so the client is not re-downloaded on every pod start |
| `SB_STEAMCMD_SEED` | `/opt/steamcmd` | Copy shipped in the image, used to seed `SB_STEAMCMD` |
| `SB_APP_ID` | `211820` | Starbound Steam app id |
| `SB_SERVER_BINARY` | `linux/starbound_server` | Executable relative to the server dir |
| `SB_GAME_PORT` | `21025` | Port shown in the UI (the server itself reads its config) |
| `SB_PUBLIC_ADDRESS` | *(empty)* | Address players connect to, shown on the dashboard (e.g. `game.example.com:21025`); defaults to the panel host name |
| `SB_ADMIN_USER` | `admin` | Administrator name for first-run setup |
| `SB_ADMIN_PASSWORD` | *(empty)* | Seed the password on first start instead of using the setup page |
| `SB_LOG_BUFFER` | `2000` | Console lines kept in memory |
| `SB_GITHUB_TOKEN` | *(empty)* | Optional; lifts the GitHub API rate limit when listing OpenStarbound releases |
| `SB_API_TOKEN` | *(empty)* | Optional bearer token for automation; grants admin access to the JSON API and skips the CSRF header |

The game port and server name live in `starbound_server.config`. Starbound
resolves that file relative to its **storage directory** (see
`Root::toStoragePath` in the engine), so it ends up at
`/data/storage/starbound_server.config`. The server generates it on first start
and the panel edits it in place.

## Data layout

```
/data
├── server/                  # steamcmd install target
│   ├── linux/
│   │   ├── starbound_server
│   │   └── sbinit.config    # read from the working directory (linux/)
│   ├── assets/ …
│   ├── mods/                # enabled mods (.pak)
│   └── storage -> /data/storage
├── storage/                 # universe + player saves
│   └── starbound_server.config
├── steamcmd/                # steamcmd client (seeded from the image on first use)
├── state/state.json         # admin hash, sessions, Steam credentials, mods
├── mods-disabled/           # disabled mods
└── backups/                 # default backup location
```

The server runs with its working directory set to `server/linux` (the
equivalent of the shipped `run-server.sh`), because `sbinit.config` — which
points at `../assets`, `../mods` and `../storage` — is resolved from the
current directory.

`state.json` contains the Steam password in plain text (mode 0600) because
steamcmd needs it non-interactively. Protect the volume; use a dedicated Steam
account rather than your main one.

## Security notes

- The panel is designed for a trusted network (LAN or VPN). It has its own
  login but no 2FA: put it behind your reverse proxy / SSO if it is reachable
  from the internet.
- Cookies are `HttpOnly` + `SameSite=Lax`; state-changing requests require the
  `X-SB-Request` header, which blocks cross-site form posts.
- Run the container as a non-root user (the image uses uid/gid 1000).

## Kubernetes

The image is stateless apart from `/data` and `/backup`, so a single-replica
Deployment with `strategy: Recreate` (RWO volume) is enough. Expose `8080` with
an Ingress/HTTPRoute and `21025/TCP` with a LoadBalancer or NodePort service.
Set `terminationGracePeriodSeconds` to at least the stop timeout (default 90s)
so the universe is saved on pod shutdown.

## OpenStarbound

[OpenStarbound](https://github.com/OpenStarbound/OpenStarbound) is a community
fork built from the leaked Starbound 1.4.4 source. It ships its own
`linux/starbound_server`, keeps the same `sbinit.config` / `storage/` /
`starbound_server.config` layout, and logs the same
`Client 'Name' <id> (host) connected` lines, so the panel supervises it
unchanged.

**Settings → Server engine** switches between the two:

1. install the vanilla server once — that provides `assets/packed.pak`, which
   OpenStarbound requires;
2. stop the server, choose `OpenStarbound` and a release (`latest` or a pinned
   tag such as `v0.1.15.1`), then press **Switch engine**. The panel downloads
   the Linux server archive from GitHub, verifies its SHA-256 and overlays it
   into the install directory;
3. switch back at any time. Before the first switch the vanilla binary is
   copied to `/data/engine/vanilla`, so no Steam round-trip is needed; if that
   copy is missing the panel falls back to `steamcmd`.

`Install / update` follows the selected engine: SteamCMD for vanilla, the
GitHub release for OpenStarbound. `lzip` is included in the image to unpack the
`.tar.lz` archives.

OpenStarbound also offers an optional query server and RCON
(`runQueryServer` / `runRconServer` in `starbound_server.config`). The panel
does not use them: it parses the console log so that vanilla servers work too.

## Workshop mods

The **Mods** tab renders every installed mod as a card with its Workshop title,
preview image, description and subscriber count. That metadata comes from the
public Workshop details endpoint, so it works without any key.

- **Subscribe** — downloads the item with steamcmd, copies its `.pak` files into
  the server's `mods/` directory and enables it. Restart the server to apply.
- **Disable / Enable** — moves the files in or out of the game's mod search path.
- **Unsubscribe** — deletes the files the panel installed and forgets the item.
- **Search** — needs a free Steam Web API key from
  <https://steamcommunity.com/dev/apikey>, entered under
  **Settings → Steam account**. Without a key the browse card explains what is
  missing; everything else keeps working.

Player subscriptions cannot be shown: the Starbound server protocol does not
expose the client's mod list, and Steam does not link a player's in-game
account to their Workshop subscriptions.

The dedicated server never downloads Steam subscriptions by itself — steamcmd
only fetches an item when the panel asks it to. Files copied into mods/ by
hand are listed under **Unmanaged files**, where they can be enabled, disabled
or deleted.

**Upload** accepts one or more .pak files or a .zip that contains them
(subfolders are flattened). Uploads are written enabled into mods/ and show
up under unmanaged files; restart the server to apply.

## How the player list works

Starbound has no query protocol and no RCON. The dedicated server does log
client connections with a descriptive name:

```
[Info] UniverseServer: Client 'Nyazuli' <1> (203.0.113.5) connected
[Info] UniverseServer: Client 'Nyazuli' <1> (203.0.113.5) disconnected for reason: …
```

The panel parses those lines, plus `Chat: <name>` lines for older/vanilla
builds that only log a bare client id. If the log format changes, the parser is
in [`internal/supervisor/parser.go`](internal/supervisor/parser.go).

## Building

```bash
docker build --build-arg VERSION=0.4.6 -t starbound-panel:0.4.6 .
```

or locally:

```bash
go build ./cmd/starbound-panel
go test ./...
```

Go 1.27+ is required. There are no third-party dependencies.

## API

Every endpoint is JSON and requires the session cookie except `/api/session`,
`/api/login`, `/api/setup` and `/api/healthz`.

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/status` | Server status, players, Steam state, backup state |
| GET | `/api/stream` | SSE stream (`hello`, `log`, `status`, `players`, `ping`) |
| POST | `/api/server/{start,stop,restart}` | Process control |
| POST | `/api/server/install` | Install/update via steamcmd (async) |
| POST | `/api/server/command` | Send a console command |
| GET/PUT | `/api/config` | Read/write `starbound_server.config` |
| GET/POST | `/api/mods` | List / add a Workshop mod |
| POST | `/api/mods/{id}/{enable,disable}` | Toggle a mod |
| DELETE | `/api/mods/{id}` | Remove a mod |
| GET/POST | `/api/backups` | List / create backups |
| DELETE | `/api/backups/{name}` | Delete a backup |
| GET | `/api/backups/{name}/download` | Download a backup |
| GET/PUT | `/api/settings` | Panel settings |
| POST | `/api/settings/steam` | Steam credentials |
| POST | `/api/settings/password` | Change the admin password |

## License

MIT — see [LICENSE](LICENSE).

Starbound and its dedicated server are property of Chucklefish; this project
only automates the official server binary and SteamCMD.
