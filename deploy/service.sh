#!/usr/bin/env bash
#
# Manage a component (server|bot) as a macOS launchd LaunchAgent (per-user, GUI
# session so it has audio + GPU access).
#
# Usage: deploy/service.sh <server|bot> <command>
#   install    render the plist, load & enable the agent (starts now and at login)
#   uninstall  unload the agent and remove the plist
#   start      load the agent (or restart it if already loaded)
#   stop       unload the agent (with KeepAlive, unloading is the reliable stop)
#   restart    restart the running agent
#   status     show the agent state (server also hits /healthz)
#   logs       follow the error log
#   render     print the rendered plist to stdout (for linting; no side effects)
#
# Env overrides (read from the installing shell — launchd never sources ~/.zshrc):
#   both:   TTS_TOKEN (baked into the plist's EnvironmentVariables; empty = no auth)
#   server: TTS_ADDR (default 127.0.0.1:8080)
#   bot:    TTS_CHANNEL (required for install), TTS_URL (default http://127.0.0.1:8080),
#           TWITCH_CLIENT_ID / TWITCH_CLIENT_SECRET (enable chat replies; empty = disabled)
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LOGDIR="$HOME/Library/Logs"
DOMAIN="gui/$(id -u)"

# sed-escape a value so it's treated literally in the plist substitution.
esc() { printf '%s' "${1:-}" | sed 's/[&|\\]/\\&/g'; }

# Bearer token + Twitch app credentials from the current environment.
TOKEN_ESC=$(esc "${TTS_TOKEN:-}")
TWITCH_ID_ESC=$(esc "${TWITCH_CLIENT_ID:-}")
TWITCH_SECRET_ESC=$(esc "${TWITCH_CLIENT_SECRET:-}")

TARGET="${1:-}"
CMD="${2:-}"

usage() {
  echo "usage: $(basename "$0") <server|bot> <install|uninstall|start|stop|restart|status|logs|render>" >&2
  exit 1
}

case "$TARGET" in
  server)
    LABEL="com.rtukpe.tts-server"
    BIN="$REPO/bin/tts-server"
    LOGBASE="tts-server"
    BUILD_HINT="server:build"
    ADDR="${TTS_ADDR:-127.0.0.1:8080}"
    render() {
      sed -e "s|__REPO__|$REPO|g" -e "s|__LOGDIR__|$LOGDIR|g" -e "s|__ADDR__|$ADDR|g" \
          -e "s|__TTS_TOKEN__|$TOKEN_ESC|g" \
          "$REPO/deploy/$LABEL.plist.template"
    }
    health() { curl -s -m 2 "http://$ADDR/healthz" && echo || echo "  (not responding)"; }
    preflight() { :; }
    ;;
  bot)
    LABEL="com.rtukpe.tts-bot"
    BIN="$REPO/bin/tts-bot"
    LOGBASE="tts-bot"
    BUILD_HINT="bot:build"
    CHANNEL="${TTS_CHANNEL:-}"
    TTS_URL="${TTS_URL:-http://127.0.0.1:8080}"
    render() {
      sed -e "s|__REPO__|$REPO|g" -e "s|__LOGDIR__|$LOGDIR|g" \
          -e "s|__CHANNEL__|$CHANNEL|g" -e "s|__TTS_URL__|$TTS_URL|g" \
          -e "s|__TTS_TOKEN__|$TOKEN_ESC|g" \
          -e "s|__TWITCH_CLIENT_ID__|$TWITCH_ID_ESC|g" \
          -e "s|__TWITCH_CLIENT_SECRET__|$TWITCH_SECRET_ESC|g" \
          "$REPO/deploy/$LABEL.plist.template"
    }
    health() { echo "  (bot has no HTTP endpoint; check logs)"; }
    preflight() {
      if [ -z "${CHANNEL:-}" ]; then
        echo "error: set TTS_CHANNEL=<your_channel> for the bot" >&2
        exit 1
      fi
    }
    ;;
  *)
    usage
    ;;
esac

PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"

case "$CMD" in
  install)
    if [ ! -x "$BIN" ]; then
      echo "error: $BIN not found — run 'mise run $BUILD_HINT' first" >&2
      exit 1
    fi
    preflight
    mkdir -p "$HOME/Library/LaunchAgents" "$LOGDIR"
    render > "$PLIST"
    launchctl bootout "$DOMAIN/$LABEL" 2>/dev/null || true
    launchctl bootstrap "$DOMAIN" "$PLIST"
    launchctl enable "$DOMAIN/$LABEL"
    echo "installed & started $LABEL"
    echo "  plist: $PLIST"
    echo "  logs:  $LOGDIR/$LOGBASE.{out,err}.log"
    ;;
  uninstall)
    launchctl bootout "$DOMAIN/$LABEL" 2>/dev/null || true
    rm -f "$PLIST"
    echo "uninstalled $LABEL"
    ;;
  start)
    launchctl bootstrap "$DOMAIN" "$PLIST" 2>/dev/null || launchctl kickstart "$DOMAIN/$LABEL"
    echo "started $LABEL"
    ;;
  stop)
    launchctl bootout "$DOMAIN/$LABEL" 2>/dev/null || true
    echo "stopped $LABEL (unloaded; run start to bring it back)"
    ;;
  restart)
    launchctl kickstart -k "$DOMAIN/$LABEL"
    echo "restarted $LABEL"
    ;;
  status)
    if launchctl print "$DOMAIN/$LABEL" >/dev/null 2>&1; then
      launchctl print "$DOMAIN/$LABEL" \
        | grep -E '^\s*(state|pid|last exit code) =' | sed 's/^[[:space:]]*/  /' || true
    else
      echo "  $LABEL is not loaded"
    fi
    echo "  --- health ---"
    health
    ;;
  logs)
    exec tail -n 50 -f "$LOGDIR/$LOGBASE.err.log"
    ;;
  render)
    render
    ;;
  *)
    usage
    ;;
esac
