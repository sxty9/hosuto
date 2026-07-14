# hosuto

**Game-server hosting, as a holistic service.** A member creates as many game servers as their quota
allows, invites the people they already know through [contax], and hands them a ready-to-play client
bundle. Minecraft Java Edition in this first pass; the store is shaped for more games.

```
Player ── smp.mc.<zone>:25565 ──► router (FRITZ!Box forwards 25565/tcp)
                                    │
                               mc-router — reads the hostname out of the Minecraft handshake
                                    ├─ smp.mc.<zone>      → 127.0.0.1:25601   hosuto-mc@ann-smp
                                    └─ skyblock.mc.<zone> → 127.0.0.1:25602   hosuto-mc@bob-skyblock
Browser ── /api/services/hosuto/* ──► Caddy ──► hosutod (127.0.0.1:8779)
                                                   │ sudo -n /usr/local/sbin/hosuto-server
                                                   │ RCON on 127.0.0.1 (whitelist, list, stop)
                                                   └─ HTTP → notify, contax
```

Every server keeps `online-mode=true` and authenticates against Mojang itself. mc-router only splices
raw TCP — it is not a Velocity/BungeeCord network, which would force every backend into
`online-mode=false` and let anyone join as anyone.

## What hosuto owns

The **Linux user → game account** mapping. No other holistic service knows a member's Minecraft
identity, and it is what makes the rest work: because hosuto knows that `bob` is `Notch`, and contax
knows that `ann` and `bob` are acquainted, `ann` can add `bob` to her server by name and hosuto can
write a correct `whitelist.json` entry for him.

Membership itself is never copied. A grant points at a contax group or an OS group, and the members
are resolved **live** on every request — contax owns its groups, the OS owns its groups, hosuto owns
only the pointer.

## Tabs

Servers first (**Meine Server** / **Beitretbare Server** — the latter is what others added you to);
pick one and it opens four tabs:

- **Erreichbarkeit** — the domain, live status, and start/stop/restart.
- **Mitspieler** — whitelist / open, and who may join. Adding someone requires that you already know
  them (a shared `hc_*` contact group) or that you share a contax group. Enforced in the daemon.
- **Version & Modding** — Minecraft version, mod loader, and mods from Modrinth.
- **Client Export** — *Just the Mods* (zip), *Prism Launcher Ez2Go* (`.mrpack`), *Prism Instance* (zip).

## Isolation

Each server runs as **its own dedicated system account** (`hs-<slug>`) — not as the member who owns
it. That is deliberate and it is the single most important decision in the repo: a modded server
executes third-party code, and the owner of a real holistic host is in `sudo`, `lxd` and `holistic`
(the shared JWT secret, which forges a session as *any* user). Mod code inherits none of it. Two
servers belonging to the same person are isolated from each other too.

`hosutod` itself is unprivileged and escalates only through `/usr/local/sbin/hosuto-server`, a narrow
sudoers-allowlisted wrapper that re-derives every guard from the kernel and trusts nothing it is told.

## Rights

| Group | Default | Grants |
|---|---|---|
| `hp_hosuto_play` | on | Link a game account, reach the servers you were added to, export client files |
| `hp_hosuto_host` | off | Create and run your own servers |
| `hp_hosuto_admin` | off | See and control every server on the host |

## Configuration

Admin settings (DNS zone, port pool, per-member server cap, heap ceiling, mc-router API) live in the
Holistic Dashboard's central **Configuration** tab, declared by `config/hosuto.json`. The service tab
stays user experience.

## Layout

```
service                   the CLI — generates the systemd units, Caddy route, sudoers and
                          rights/config drop-ins inline; it is their source of truth
sbin/hosuto-server        the privileged wrapper (create/start/stop/restart/destroy/status)
sbin/hosuto-run           ExecStart — turns <dir>/exec.argv into the JVM
sbin/hosuto-stop          ExecStop  — RCON /stop, then waits for the world to save
systemd/hosuto-mc@.service   the per-server template unit
permissions/hosuto.json   rights manifest (privleg)
config/hosuto.json        config manifest (the central Configuration tab)
backend/internal/
  store/     accounts + servers + grants (flat JSON, atomic, daemon is sole writer)
  runtime/   ports, systemd, mc-router route, whitelist, live status
  mcnet/     RCON + Server List Ping, hand-rolled (zero deps)
  mcfiles/   server.properties, whitelist.json, ops.json, eula.txt
  versions/  Mojang / Fabric / NeoForge / Paper — catalogue and install
  modrinth/  mod search + resolve; client_side/server_side is what splits client from server
  export/    mods zip, .mrpack, Prism instance, servers.dat (NBT)
  directory/ "do these two know each other" — shared hc_* Linux group, read live
  contax/    contax personal-group membership (machine-to-machine)
ui/          the dashboard plugin (@holistic/ui only)
```

## Operating

```bash
sudo ./service setup      # install/repair: daemon, Java, mc-router, wrapper, units, rights, config, UI
./service status          # daemon, route, rights, config, mc-router, java, server count
sudo ./service update     # git pull --ff-only + setup
```

## Verify

```bash
(cd backend && go build ./... && go vet ./... && go test ./...)
python3 ../holistic/services/dashboard/lib/holistic-perms.py  validate ./permissions
python3 ../holistic/services/dashboard/lib/holistic-config.py validate ./config
```

## Not done yet

- **Port 25565 must be forwarded on the router**, and the public IPv4 is dynamic — the per-server
  A records need DDNS. Until both are true, servers are reachable on the LAN only.
- Account linking is a *claim* (name → Mojang UUID), not a proof. `Account.Verified` exists for a
  later Microsoft OAuth flow.
- No world backups.

[contax]: https://github.com/sxty9/contax
