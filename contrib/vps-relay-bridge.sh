#!/usr/bin/env bash
# vps-relay-bridge.sh — macht hosuto-Server öffentlich erreichbar über einen EIGENEN gemieteten VPS,
# ohne Portfreigabe am Heimrouter und ohne einen fremden Tunneldienst.
#
# WAS DAS IST
#   Der Heimserver baut eine AUSGEHENDE WireGuard-Verbindung zu einem kleinen VPS auf (der Router
#   muss nichts freigeben). Der VPS ist die öffentliche Front: er nimmt die Spiel-Ports an und leitet
#   sie durch den verschlüsselten Tunnel auf `127.0.0.1:25565` zuhause — dort verteilt mc-router wie
#   gewohnt nach Hostname an beliebig viele Server. Anders als playit terminiert NICHTS die Verbindung
#   unter fremdem Namen: der echte Hostname steht im Handshake, also funktioniert Hostname-Routing und
#   es braucht KEINE Default-Route (mehrere Server je eigene Domain gehen). WireGuard + Weiterleitung
#   laufen im Kernel → praktisch kein Overhead, nur die reine Netzstrecke Spieler↔VPS↔Heim.
#
# WAS DU AUFGIBST — ehrlich, vor dem Ausführen
#   * Ein kleiner Dauer-Kostenposten (der VPS, ~4-5 €/Monat).
#   * Ein zusätzlicher Netz-Hop (VPS↔Heim, ~10-15 ms). Dafür bleibt das Heimnetz komplett zu und die
#     öffentliche IP versteckt; ein DDoS trifft den VPS, nicht euren Anschluss.
#   * Spieler erscheinen dem Gameserver unter der VPS-Overlay-IP (wie bei playit unter 127.0.0.1) —
#     Whitelisting per Mojang-UUID bleibt unberührt, Ban-per-IP nicht.
#
# DER AUSSTIEG (wenn z. B. doch ein Port-Forward kommt)
#   1. Auf VPS und Heim: sudo ./contrib/vps-relay-bridge.sh --uninstall
#   2. VPS kündigen; DNS der `zone` wieder auf die eigene öffentliche IP (DNS-only, graue Wolke).
#   3. Dashboard → Configuration → hosuto: `exposureMethod` = direct-port.
#
# NUTZUNG — drei Schritte, in dieser Reihenfolge:
#   1) Auf dem HEIM-Server:  sudo ./contrib/vps-relay-bridge.sh init
#        -> erzeugt den Heim-Schlüssel (falls nötig) und druckt den HEIM-PUBLIC-KEY.
#   2) Auf dem VPS:          sudo ./contrib/vps-relay-bridge.sh vps <HEIM_PUBLIC_KEY>
#        -> richtet WireGuard + Port-Weiterleitung ein, druckt VPS-PUBLIC-KEY und VPS-IP.
#   3) Auf dem HEIM-Server:  sudo ./contrib/vps-relay-bridge.sh home <VPS_PUBLIC_KEY> <VPS_IP>
#        -> verbindet den Tunnel und startet ihn dauerhaft.
#
# Exit codes: 0 ok · 1 generisch · 2 usage · 4 extern

set -euo pipefail

WG_CONF=/etc/wireguard/wg0.conf
WG_KEYDIR=/etc/wireguard
WG_PORT=51820
OVERLAY_VPS=10.10.0.1
OVERLAY_HOME=10.10.0.2
# Spiel-Ports, die der VPS nach Hause durchreicht. TCP 25565 = Minecraft (mc-router). Die UDP-Bereiche
# sind für spätere Spiele (ARK 7777-7790, Steam-Query 27015-27025) — schaden ungenutzt nicht.
TCP_PORTS="25565"
UDP_RANGES="7777:7790 27015:27025"

log()  { printf '[relay] %s\n' "$*"; }
warn() { printf '[relay] warnung: %s\n' "$*" >&2; }
die()  { printf '[relay] fehler: %s\n' "$*" >&2; exit "${2:-1}"; }

[ "$(id -u)" -eq 0 ] || exec sudo -E "$0" "$@"

ensure_tools() {
  command -v wg >/dev/null 2>&1 && return
  log "installiere wireguard-tools..."
  DEBIAN_FRONTEND=noninteractive apt-get update -qq
  DEBIAN_FRONTEND=noninteractive apt-get install -y -qq wireguard-tools >/dev/null
}

ensure_keys() {
  umask 077
  mkdir -p "$WG_KEYDIR"
  [ -s "$WG_KEYDIR/home_private.key" ] || wg genkey > "$WG_KEYDIR/home_private.key"
  chmod 600 "$WG_KEYDIR/home_private.key"
  wg pubkey < "$WG_KEYDIR/home_private.key" > "$WG_KEYDIR/home_public.key"
  chmod 644 "$WG_KEYDIR/home_public.key"
}

mode="${1:-}"; shift || true
case "$mode" in

# ── 1) HEIM: Schlüssel + Public-Key ─────────────────────────────────────────────────────
init)
  ensure_tools; ensure_keys
  log "Heim-Schlüssel bereit. Diesen HEIM-PUBLIC-KEY auf dem VPS verwenden:"
  echo; cat "$WG_KEYDIR/home_public.key"; echo
  log "Weiter: auf dem VPS  ->  sudo ./vps-relay-bridge.sh vps <dieser-key>"
  ;;

# ── 2) VPS: WireGuard-Server + Port-Weiterleitung ───────────────────────────────────────
vps)
  HOME_PUB="${1:-}"
  [ -n "$HOME_PUB" ] || die "usage: $0 vps <HEIM_PUBLIC_KEY>" 2
  ensure_tools
  # Frische Cloud-Images bringen iptables nicht immer mit — die Weiterleitung braucht es.
  command -v iptables >/dev/null 2>&1 || { log "installiere iptables..."; DEBIAN_FRONTEND=noninteractive apt-get install -y -qq iptables >/dev/null; }
  umask 077; mkdir -p "$WG_KEYDIR"
  [ -s "$WG_KEYDIR/vps_private.key" ] || wg genkey > "$WG_KEYDIR/vps_private.key"
  chmod 600 "$WG_KEYDIR/vps_private.key"
  VPS_PRIV=$(cat "$WG_KEYDIR/vps_private.key")
  VPS_PUB=$(wg pubkey < "$WG_KEYDIR/vps_private.key")

  # DNAT-Regeln für alle Spiel-Ports, gebaut als PostUp/PostDown der wg0-Schnittstelle: sie kommen und
  # gehen atomar mit dem Tunnel, kein separates iptables-persistent nötig. MASQUERADE auf wg0 sorgt für
  # den Rückweg (Heim antwortet an die VPS-Overlay-IP, die per Tunnel erreichbar ist).
  up=""; down=""
  for p in $TCP_PORTS; do
    up+="\nPostUp = iptables -t nat -A PREROUTING -p tcp --dport $p -j DNAT --to-destination $OVERLAY_HOME:$p"
    down+="\nPostDown = iptables -t nat -D PREROUTING -p tcp --dport $p -j DNAT --to-destination $OVERLAY_HOME:$p || true"
  done
  for r in $UDP_RANGES; do
    up+="\nPostUp = iptables -t nat -A PREROUTING -p udp --dport $r -j DNAT --to-destination $OVERLAY_HOME"
    down+="\nPostDown = iptables -t nat -D PREROUTING -p udp --dport $r -j DNAT --to-destination $OVERLAY_HOME || true"
  done

  cat > "$WG_CONF" <<EOF
[Interface]
# VPS-Seite: öffentliche Front. Nimmt die Spiel-Ports an und reicht sie durch den Tunnel nach Hause.
Address = $OVERLAY_VPS/24
ListenPort = $WG_PORT
PrivateKey = $VPS_PRIV
PostUp = sysctl -w net.ipv4.ip_forward=1$(printf '%b' "$up")
PostUp = iptables -t nat -A POSTROUTING -o %i -j MASQUERADE
# WireGuard verkleinert die MTU auf 1420 — ohne MSS-Klemme zerbrechen grosse TCP-Pakete (z.B.
# Minecrafts update_recipes) im Tunnel und der Client wirft einen DecoderException. Die Klemme
# setzt die TCP-MSS beim Verbindungsaufbau auf die Pfad-MTU, in beide Richtungen.
PostUp = iptables -t mangle -A FORWARD -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu
PostUp = iptables -A FORWARD -i %i -j ACCEPT
PostUp = iptables -A FORWARD -o %i -j ACCEPT
PostDown = iptables -t nat -D POSTROUTING -o %i -j MASQUERADE || true
PostDown = iptables -t mangle -D FORWARD -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu || true
PostDown = iptables -D FORWARD -i %i -j ACCEPT || true
PostDown = iptables -D FORWARD -o %i -j ACCEPT || true$(printf '%b' "$down")

[Peer]
# Der Heimserver (nur er darf durch den Tunnel).
PublicKey = $HOME_PUB
AllowedIPs = $OVERLAY_HOME/32
PersistentKeepalive = 25
EOF
  chmod 600 "$WG_CONF"
  systemctl enable --now "wg-quick@wg0" >/dev/null 2>&1 || { systemctl daemon-reload; systemctl enable --now "wg-quick@wg0"; }

  VPS_IP=$(curl -s --max-time 8 https://api.ipify.org 2>/dev/null || hostname -I | awk '{print $1}')
  log "VPS-Relay steht. Diese zwei Werte auf dem HEIM-Server verwenden:"
  echo; echo "  VPS_PUBLIC_KEY = $VPS_PUB"; echo "  VPS_IP         = $VPS_IP"; echo
  warn "Falls der VPS eine Firewall hat: UDP $WG_PORT eingehend erlauben (der WireGuard-Port)."
  log "Weiter: auf dem HEIM-Server  ->  sudo ./vps-relay-bridge.sh home $VPS_PUB $VPS_IP"
  ;;

# ── 3) HEIM: Tunnel verbinden + starten ─────────────────────────────────────────────────
home)
  VPS_PUB="${1:-}"; VPS_IP="${2:-}"
  [ -n "$VPS_PUB" ] && [ -n "$VPS_IP" ] || die "usage: $0 home <VPS_PUBLIC_KEY> <VPS_IP>" 2
  ensure_tools; ensure_keys
  HOME_PRIV=$(cat "$WG_KEYDIR/home_private.key")
  cat > "$WG_CONF" <<EOF
[Interface]
# Heim-Seite: nur AUSGEHEND (kein ListenPort, keine Router-Freigabe). Nur das Overlay geht durch den
# Tunnel — die normale Heim-Route bleibt unberührt.
Address = $OVERLAY_HOME/24
PrivateKey = $HOME_PRIV

[Peer]
PublicKey = $VPS_PUB
Endpoint = $VPS_IP:$WG_PORT
AllowedIPs = $OVERLAY_VPS/32
PersistentKeepalive = 25
EOF
  chmod 600 "$WG_CONF"
  systemctl enable --now "wg-quick@wg0" >/dev/null 2>&1 || { systemctl daemon-reload; systemctl enable --now "wg-quick@wg0"; }
  sleep 2
  if wg show wg0 2>/dev/null | grep -q 'latest handshake'; then
    log "Tunnel steht (Handshake ok)."
  else
    warn "Noch kein Handshake — prüfe VPS-IP/Firewall (UDP $WG_PORT) und 'wg show wg0'."
  fi
  log "JETZT NOCH im Dashboard: Configuration → hosuto → exposureMethod = vps-relay,"
  log "und die DNS-Zone auf $VPS_IP zeigen (DNS-only / graue Wolke)."
  ;;

# ── Abbau (beide Seiten) ────────────────────────────────────────────────────────────────
--uninstall)
  systemctl disable --now "wg-quick@wg0" >/dev/null 2>&1 || true
  rm -f "$WG_CONF"
  log "wg0 entfernt. Schlüssel bleiben unter $WG_KEYDIR (löschbar, wenn du sicher bist)."
  ;;

*)
  die "usage: $0 {init | vps <heim_public_key> | home <vps_public_key> <vps_ip> | --uninstall}" 2
  ;;
esac
