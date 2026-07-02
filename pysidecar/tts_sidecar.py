"""Persistent Kokoro TTS sidecar.

Loads the Kokoro model ONCE, then serves synthesis requests over a simple
line-delimited JSON protocol on stdin/stdout so a parent process (the Go
server) can keep the model warm across many requests.

Protocol
--------
stdout carries ONLY protocol JSON, one object per line:
  - once, after the model has loaded:      {"type": "ready"}
  - one per request, echoing the id:       {"type": "result", "id": N, "ok": true}
                                            {"type": "result", "id": N, "ok": false, "error": "..."}
All logging / library noise goes to stderr.

stdin carries one request object per line:
  {"id": N, "text": "...", "voice": "af_heart", "speed": 1.0, "out": "/path/out.wav"}

Audio is written as mono / 16-bit PCM / 24 kHz WAV using the stdlib `wave`
module, matching Kokoro's native output (so no soundfile/scipy dependency).
"""

import contextlib
import json
import os
import sys
import wave

import numpy as np


SAMPLE_RATE = 24000  # Kokoro is fixed at 24 kHz mono.


def log(*args):
    """Write a diagnostic line to stderr (never stdout — stdout is protocol-only)."""
    print(*args, file=sys.stderr, flush=True)


def emit(obj):
    """Write one protocol JSON object to stdout and flush immediately."""
    sys.stdout.write(json.dumps(obj) + "\n")
    sys.stdout.flush()


def build_pipeline():
    """Construct the KPipeline once.

    Importing torch/transformers/kokoro and loading the checkpoint can print to
    stdout; redirect stdout to stderr for the duration so it can't corrupt the
    protocol stream, then restore it.
    """
    lang = os.environ.get("TTS_LANG", "a")
    repo_id = os.environ.get("TTS_REPO_ID", "hexgrad/Kokoro-82M")
    with contextlib.redirect_stdout(sys.stderr):
        from kokoro import KPipeline

        pipeline = KPipeline(lang_code=lang, repo_id=repo_id)
    log(f"kokoro pipeline ready (lang={lang!r}, repo_id={repo_id!r})")
    return pipeline


def synthesize(pipeline, text, voice, speed, out_path):
    """Synthesize `text` to a WAV file at `out_path`.

    Kokoro auto-chunks long text into multiple segments; we concatenate every
    segment's samples into the one output file.
    """
    wrote_frames = False
    with wave.open(out_path, "wb") as wav:
        wav.setnchannels(1)
        wav.setsampwidth(2)  # 16-bit PCM
        wav.setframerate(SAMPLE_RATE)
        for result in pipeline(text, voice=voice, speed=speed, split_pattern=r"\n+"):
            if result.audio is None:
                continue
            samples = (result.audio.numpy() * 32767).astype(np.int16)
            wav.writeframes(samples.tobytes())
            wrote_frames = True
    if not wrote_frames:
        raise ValueError("no audio produced (empty or unspeakable text?)")


def handle(pipeline, req):
    rid = req.get("id")
    try:
        text = req["text"]
        voice = req.get("voice") or os.environ.get("TTS_VOICE", "af_heart")
        speed = float(req.get("speed") or 1.0)
        out_path = req["out"]
        synthesize(pipeline, text, voice, speed, out_path)
        emit({"type": "result", "id": rid, "ok": True})
    except Exception as exc:  # noqa: BLE001 — report every failure back to the parent
        log(f"synth error (id={rid}): {exc!r}")
        emit({"type": "result", "id": rid, "ok": False, "error": str(exc)})


def main():
    pipeline = build_pipeline()
    emit({"type": "ready"})
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            req = json.loads(line)
        except json.JSONDecodeError as exc:
            log(f"bad request line: {exc}")
            continue
        handle(pipeline, req)
    log("stdin closed, sidecar exiting")


if __name__ == "__main__":
    main()
