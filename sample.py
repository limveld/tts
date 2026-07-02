"""Sample Kokoro TTS script.

Usage:
    python sample.py "Text to speak" [output.wav]

Requires: pip install kokoro soundfile
macOS:    brew install espeak-ng
"""
import sys

import numpy as np
import soundfile as sf
from kokoro import KPipeline

SAMPLE_RATE = 24000


def synthesize(text: str, out_path: str = "output.wav", voice: str = "af_heart") -> str:
    pipeline = KPipeline(lang_code="a")  # 'a' = American English
    chunks = []
    for _, _, audio in pipeline(text, voice=voice):
        chunks.append(audio)
    full = np.concatenate(chunks)
    sf.write(out_path, full, SAMPLE_RATE)
    print(f"Wrote {out_path} ({len(full) / SAMPLE_RATE:.2f}s of audio)")
    return out_path


if __name__ == "__main__":
    text = sys.argv[1] if len(sys.argv) > 1 else (
        "Hello! This is Kokoro, an open source text to speech model, running locally."
    )
    out = sys.argv[2] if len(sys.argv) > 2 else "output.wav"
    synthesize(text, out)
