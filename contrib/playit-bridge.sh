#!/usr/bin/env bash
# playit-bridge.sh — macht EINEN hosuto-Server öffentlich erreichbar, ohne Portfreigabe am Router.
#
# WAS DAS IST
#   Eine TEMPORÄRE BRÜCKE, kein Teil von hosuto. Wer an seinen Router nicht rankommt (Elternhaus,
#   WG, Studentenwohnheim), kann Port 25565 nicht freigeben — und Minecraft ist rohes TCP, das der
#   Cloudflare-Tunnel der restlichen Landschaft nicht tragen kann. playit.gg schliesst genau diese
#   Lücke: der Agent baut eine AUSGEHENDE Verbindung auf, playit nimmt öffentlich Verbindungen an
#   und schiebt sie durch den Tunnel zurück. Der Router muss nichts wissen.
#
# WAS DU DAMIT AUFGIBST — ehrlich, vor dem Ausführen
#   * Der gesamte Spielverkehr deiner Freunde läuft über playits Server. Du vertraust einem Dritten.
#   * Es entsteht ein dauerhafter Dienst auf dieser Maschine, der nach aussen verbindet.
#   * Es funktioniert für GENAU EINEN Server. playit terminiert die Verbindung unter SEINEM eigenen
#     Hostnamen, unsere Domain steht also nie im Minecraft-Handshake — mc-router hat nichts zum
#     Matchen und braucht eine Default-Route ("alles Unbekannte zu diesem einen Server"). Mehrere
#     Server mit je eigener Domain gehen damit NICHT. Dafür braucht es die Portfreigabe.
#
# DER AUSSTIEG (wenn die Portfreigabe kommt)
#   1. sudo ./contrib/playit-bridge.sh --uninstall
#   2. Im Dashboard → Configuration → hosuto: `defaultServer` auf LEER setzen.
#   3. DNS: *.mc.<zone> als A-Record auf die öffentliche IP, DNS-only (graue Wolke), plus DDNS.
#      NIEMALS einen SRV-Record in diese Zone — SRV ERSETZT den Hostnamen im Handshake und alle
#      Server kollabieren auf ein Backend.
#   Danach routet hosuto wieder per Hostname auf beliebig viele Server. Kein Umbau, nur ein Schalter.
#
# Exit codes: 0 ok · 1 generisch · 2 usage · 4 extern

set -euo pipefail

VERSION=v1.0.10
BASE=https://github.com/playit-cloud/playit-agent/releases/download/$VERSION
APP=/opt/playit
SECRET=/etc/playit/playit.toml
UNIT=/etc/systemd/system/playit.service
SVC_USER=playit
# Das lokale Ziel des Tunnels: mc-router. Er lauscht auf 25565 und verteilt an die Server-Backends.
LOCAL_TARGET="127.0.0.1:25565"

log()  { printf '[playit] %s\n' "$*"; }
warn() { printf '[playit] warnung: %s\n' "$*" >&2; }
die()  { printf '[playit] fehler: %s\n' "$*" >&2; exit "${2:-1}"; }

[ "$(id -u)" -eq 0 ] || exec sudo -E "$0" "$@"

# ── uninstall ─────────────────────────────────────────────────────────────────────────
if [ "${1:-}" = "--uninstall" ]; then
  log "entferne playit..."
  systemctl disable --now playit >/dev/null 2>&1 || true
  rm -f "$UNIT"
  systemctl daemon-reload
  rm -rf "$APP" /etc/playit /var/log/playit
  userdel "$SVC_USER" >/dev/null 2>&1 || true
  groupdel "$SVC_USER" >/dev/null 2>&1 || true
  log "playit ist weg."
  log "JETZT NOCH: Dashboard → Configuration → hosuto → 'defaultServer' auf LEER setzen,"
  log "sonst landet weiterhin jede unbekannte Verbindung auf diesem einen Server."
  exit 0
fi
[ $# -eq 0 ] || die "usage: $0 [--uninstall]" 2

# ── vorbedingungen ────────────────────────────────────────────────────────────────────
command -v curl >/dev/null || die "curl fehlt" 4
systemctl is-active --quiet mc-router \
  || die "mc-router läuft nicht — erst 'sudo ./service setup' im hosuto-Repo" 4

# ── binaries ──────────────────────────────────────────────────────────────────────────
# Statisches Rust-Binary, keine Abhängigkeiten. Bewusst KEIN APT-Repo: dessen GPG-Key würde playit
# erlauben, dauerhaft beliebige Pakete als root auf diese Maschine zu schieben. Ein einzelnes Binary
# ist die kleinere Vertrauensaussage.
install_binaries() {
  local tmp; tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' RETURN
  log "lade playit $VERSION..."
  curl -fsSL --max-time 180 -o "$tmp/playitd"    "$BASE/playit-linux-amd64"     || die "download playitd fehlgeschlagen" 4
  curl -fsSL --max-time 180 -o "$tmp/playit-cli" "$BASE/playit-cli-linux-amd64" || die "download playit-cli fehlgeschlagen" 4

  # Das ist wirklich ein Programm und keine HTML-Fehlerseite?
  head -c4 "$tmp/playitd" | grep -q $'\x7fELF' || die "playitd ist kein ELF-Binary — Download kaputt?" 4

  log "prüfsummen (zum mitschreiben):"
  sha256sum "$tmp/playitd" "$tmp/playit-cli" | sed 's#/.*/#  #'

  install -d -m 0755 "$APP"
  install -m 0755 -o root -g root "$tmp/playitd"    "$APP/playitd"
  install -m 0755 -o root -g root "$tmp/playit-cli" "$APP/playit-cli"
  # root-owned: playit läuft ALS $SVC_USER und darf sein eigenes Binary nicht überschreiben können.
}

# ── systemd ───────────────────────────────────────────────────────────────────────────
# playits eigene Unit (aus dem .deb) hat KEIN Hardening — kein NoNewPrivileges, kein ProtectSystem.
# Der Agent braucht nur ausgehende Verbindungen und 127.0.0.1:25565. Also wird er entsprechend eng
# gesetzt: fremder Code, der dauerhaft nach aussen spricht, bekommt so wenig wie er zum Leben braucht.
write_unit() {
  cat > "$UNIT" <<EOF
[Unit]
Description=playit.gg agent — öffentlicher Tunnel für den hosuto-Minecraft-Server
Documentation=https://playit.gg
After=network-online.target mc-router.service
Wants=network-online.target

[Service]
User=$SVC_USER
Group=$SVC_USER
RuntimeDirectory=playit
RuntimeDirectoryMode=0750
LogsDirectory=playit
LogsDirectoryMode=0750
ExecStart=$APP/playitd --secret-path $SECRET --socket-path /run/playit/playitd.sock -l /var/log/playit/playit.log
Restart=on-failure
RestartSec=5

# Sandbox. Der Agent liest sein Secret und spricht TCP — mehr nicht.
NoNewPrivileges=yes
ProtectSystem=strict
ReadWritePaths=/etc/playit
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
RestrictSUIDSGID=yes
RestrictRealtime=yes
RestrictNamespaces=yes
LockPersonality=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
CapabilityBoundingSet=
SystemCallArchitectures=native
SystemCallFilter=@system-service

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
}

# ── claim ─────────────────────────────────────────────────────────────────────────────
# Der Agent muss einmalig mit einem playit-Konto verknüpft werden. Das geht nur über den Browser:
# wir erzeugen einen Code, der User bestätigt ihn auf playit.gg, wir tauschen ihn gegen ein Secret.
# Ein Konto ist NICHT nötig — playit legt bei Bedarf ein Gastkonto an.
do_claim() {
  if [ -s "$SECRET" ]; then
    log "secret liegt schon ($SECRET) — überspringe claim."
    return 0
  fi
  local code url secret
  code="$("$APP/playit-cli" claim generate)"
  url="$("$APP/playit-cli" claim url "$code")"

  printf '\n'
  printf '  ┌───────────────────────────────────────────────────────────────┐\n'
  printf '  │  JETZT DU: diesen Link im Browser öffnen und bestätigen        │\n'
  printf '  └───────────────────────────────────────────────────────────────┘\n\n'
  printf '      %s\n\n' "$url"
  printf '  (Kein Konto nötig — "als Gast fortfahren" reicht.)\n'
  printf '  Ich warte hier, bis du bestätigt hast — bis zu 10 Minuten.\n\n'

  # Blockiert, bis der User im Browser bestätigt.
  #
  # ACHTUNG: `claim exchange` druckt seinen ganzen Fortschritt ("Open this link...", "Program
  # approved...") ebenfalls auf STDOUT, nicht auf stderr. Die komplette Ausgabe einzufangen wuerde
  # also den Fortschritts-Schwall MIT ins Secret schreiben — der Daemon haengt dann ewig bei
  # "Waiting for frontend secret provisioning". Das echte Secret ist die einzige Zeile mit exakt
  # 64 Hex-Zeichen; genau die wird herausgezogen, alles andere verworfen.
  local raw secret
  raw="$("$APP/playit-cli" claim exchange "$code" --wait 600)" \
    || die "claim nicht bestätigt (timeout) — skript einfach nochmal starten" 4
  secret="$(printf '%s\n' "$raw" | grep -oiE '[0-9a-f]{64}' | head -1)"
  [ -n "$secret" ] || die "kein gültiges Secret in der playit-Antwort gefunden" 4

  install -d -m 0755 /etc/playit
  ( umask 077; printf 'secret_key = "%s"\n' "$secret" > "$SECRET" )
  chown "$SVC_USER:$SVC_USER" "$SECRET"
  chmod 0600 "$SECRET"
  log "claim bestätigt, secret gespeichert."
}

# ── los ───────────────────────────────────────────────────────────────────────────────
log "system-user..."
getent group  "$SVC_USER" >/dev/null || groupadd --system "$SVC_USER"
getent passwd "$SVC_USER" >/dev/null \
  || useradd --system --gid "$SVC_USER" --no-create-home --home-dir /nonexistent \
             --shell /usr/sbin/nologin "$SVC_USER"

install_binaries
install -d -m 0755 /etc/playit
do_claim
write_unit

log "starte playit..."
systemctl enable playit >/dev/null 2>&1 || true
systemctl restart playit
sleep 3
systemctl is-active --quiet playit || {
  journalctl -u playit -n 15 --no-pager >&2 || true
  die "playit startet nicht" 4
}

cat <<EOF

[playit] läuft.

  ┌───────────────────────────────────────────────────────────────────────────┐
  │  NOCH ZWEI SCHRITTE — beide bei dir                                        │
  └───────────────────────────────────────────────────────────────────────────┘

  1) TUNNEL ANLEGEN (im Browser)

     Einloggen:   $($APP/playit-cli account login-url 2>/dev/null || echo 'https://playit.gg/account')

     Dann:  Tunnels → Create Tunnel
              Typ            : Minecraft Java
              Local Address  : 127.0.0.1
              Local Port     : 25565      <- das ist mc-router
              Proxy Protocol : aus

     Du bekommst eine Adresse der Form   <name>.gl.joinmc.link   (ohne Port).
     Genau die tippen deine Freunde bei "Server hinzufügen" ein.

  2) FALLBACK-SERVER SETZEN (im Dashboard)

     Configuration → hosuto → "Fallback server (address)"
       = der Slug deines Servers, z.B.  smp

     Das ist nötig, weil playit die Verbindung unter SEINEM Hostnamen ausliefert
     (mal .gl.joinmc.link, mal .gl.at.ply.gg) — mc-router kennt den nicht und würde
     ihn sonst abweisen. Der Fallback fängt beides ab.

  Danach: Server in hosuto anlegen, starten, Freunde einladen.

  Rückbau, sobald die Portfreigabe da ist:
     sudo $0 --uninstall

EOF
