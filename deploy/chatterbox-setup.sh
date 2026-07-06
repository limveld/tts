#!/usr/bin/env bash
#
# Install the vendored devnen Chatterbox-TTS server (the `chatterbox-server`
# submodule) into its own Python 3.10 virtualenv on macOS / Apple Silicon.
#
# This replicates the deterministic install that the upstream launcher
# (chatterbox-server/start.py) performs for a Mac install — WITHOUT running
# start.py itself, because start.py is interactive (menu / y-n prompts) and would
# hang under automation. Kept in lockstep with the pinned submodule commit; if you
# bump the submodule, re-check start.py's install steps (CHATTERBOX_REPO, the
# requirements file, the --no-deps set) against this script.
#
# The chatterbox-v2 fork installed below already contains the MPS float32 fix, so
# start.py's post-install `_patch_chatterbox_mps_float32` is intentionally skipped.
#
# Usage: mise run chatterbox:setup   (or: bash deploy/chatterbox-setup.sh)
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SRV="$REPO/chatterbox-server"
VENV="$SRV/venv"                 # start.py uses "venv" (not ".venv")
PY_VERSION="3.10"                # devnen requires 3.10; 3.11+ lacks wheels
CHATTERBOX_REPO="git+https://github.com/devnen/chatterbox-v2.git@master"

if [ ! -f "$SRV/server.py" ]; then
  echo "error: $SRV/server.py not found — run 'git submodule update --init chatterbox-server' first" >&2
  exit 1
fi

echo "==> Creating Python $PY_VERSION virtualenv at $VENV"
if [ ! -x "$VENV/bin/python" ]; then
  # mise provisions Python 3.10 on demand; the repo's default python is 3.12.
  mise x "python@$PY_VERSION" -- python -m venv "$VENV"
fi
PIP="$VENV/bin/pip"

echo "==> Upgrading pip"
"$VENV/bin/python" -m pip install --upgrade pip

# Base deps: pins torch==2.5.1 (the macOS arm64 wheel supports MPS) + transformers,
# audio libs, fastapi/uvicorn, etc. chatterbox-tts itself is intentionally absent here.
echo "==> Installing base requirements (torch, fastapi, audio stack)…"
"$PIP" install --no-warn-script-location -r "$SRV/requirements.txt"

# chatterbox engine + s3tokenizer + onnx with --no-deps to preserve the torch build.
echo "==> Installing chatterbox-v2 (MPS-patched fork), s3tokenizer, onnx…"
"$PIP" install --no-deps "$CHATTERBOX_REPO" s3tokenizer==0.3.0 onnx==1.16.0

# onnx needs a newer protobuf than some transitive deps pin; force it last.
echo "==> Pinning protobuf>=4.25.0"
"$PIP" install --no-deps --force-reinstall "protobuf>=4.25.0"

# Bind loopback:8004 and run on CPU. MPS loads the model but crashes during
# synthesis of the Turbo model — torchaudio's reference resample raises "conv1d
# output channels > 65536 not supported on MPS", which PYTORCH_ENABLE_MPS_FALLBACK
# does NOT rescue (the op is MPS-registered). CPU is reliable (~4s per short line on
# Apple Silicon). config.py reads ./config.yaml relative to cwd, which the launchd
# plist sets to chatterbox-server.
echo "==> Patching config.yaml (device: cpu, host: 127.0.0.1, port: 8004)"
CFG="$SRV/config.yaml"
sed -i.bak \
  -e 's/^\(  host: \).*/\1127.0.0.1/' \
  -e 's/^\(  port: \).*/\18004/' \
  -e 's/^\(  device: \).*/\1cpu/' \
  "$CFG"
rm -f "$CFG.bak"

# Keep the submodule's own `git status` tidy (the parent already has ignore = dirty).
EXCLUDE="$REPO/.git/modules/chatterbox-server/info/exclude"
if [ -f "$EXCLUDE" ] && ! grep -qx "venv/" "$EXCLUDE"; then
  printf 'venv/\n' >> "$EXCLUDE"
fi

echo ""
echo "==> Done. The model (~3 GB) downloads on first server run."
echo "    Try it:   mise run chatterbox:serve"
echo "    Service:  TTS_ENGINE=chatterbox mise run server:service:install"
