#!/usr/bin/env bash
#
# Manage the TTS server as a macOS launchd LaunchAgent (per-user, GUI session so
# it has audio + GPU access). Usage: deploy/service.sh <command>
#
#   install    build-check, render the plist, load & enable the agent (starts now
#              and at login; restarts on crash)
#   uninstall  unload the agent and remove the plist
#   start      load the agent (or restart it if already loaded)
#   stop       unload the agent (with KeepAlive, unloading is the reliable stop)
#   restart    restart the running agent
#   status     show the agent state and hit /healthz
#   logs       follow the error log
#   render     print the rendered plist to stdout (used for linting; no side effects)
#
# TTS_ADDR overrides the listen address (default 127.0.0.1:8080).
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LABEL="com.rtukpe.tts"
TEMPLATE="$REPO/deploy/$LABEL.plist.template"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
LOGDIR="$HOME/Library/Logs"
DOMAIN="gui/$(id -u)"
ADDR="${TTS_ADDR:-127.0.0.1:8080}"

render() {
  sed -e "s|__REPO__|$REPO|g" \
      -e "s|__LOGDIR__|$LOGDIR|g" \
      -e "s|__ADDR__|$ADDR|g" \
      "$TEMPLATE"
}

case "${1:-}" in
  install)
    if [ ! -x "$REPO/tts" ]; then
      echo "error: $REPO/tts not found — run 'mise run build' first" >&2
      exit 1
    fi
    mkdir -p "$HOME/Library/LaunchAgents" "$LOGDIR"
    render > "$PLIST"
    launchctl bootout "$DOMAIN/$LABEL" 2>/dev/null || true
    launchctl bootstrap "$DOMAIN" "$PLIST"
    launchctl enable "$DOMAIN/$LABEL"
    echo "installed & started $LABEL"
    echo "  plist: $PLIST"
    echo "  addr:  http://$ADDR"
    echo "  logs:  $LOGDIR/tts-server.{out,err}.log"
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
    echo "stopped $LABEL (unloaded; run 'start' to bring it back)"
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
    echo "  --- GET http://$ADDR/healthz ---"
    curl -s -m 2 "http://$ADDR/healthz" && echo || echo "  (not responding)"
    ;;
  logs)
    exec tail -n 50 -f "$LOGDIR/tts-server.err.log"
    ;;
  render)
    render
    ;;
  *)
    echo "usage: $(basename "$0") {install|uninstall|start|stop|restart|status|logs|render}" >&2
    exit 1
    ;;
esac
