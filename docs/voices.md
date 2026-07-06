# Voice map (`voices.toml`) and `!tts` codes

The server runs **kokoro (always, local, free)** and **Amazon Polly (optional, cloud, premium)
side by side**, and a chat code chooses the voice *and* the engine per message:

- `!tts <text>` — a weighted-random voice (see weights below).
- `!tts<code> <text>` — the voice mapped to `<code>` (e.g. `!ttsk` → Kevin on Polly).

The bot forwards the raw code; **the server owns the code→voice map** (`voices.toml`), so the same
`!ttsk` picks the right voice regardless of what's running. There are no per-engine flags — the
TOML is the single source of truth (`-voices <path>`, default `voices.toml`).

## `voices.toml`

```toml
[kokoro]                                        # local sidecar, always available (free)
codes.a = { voice = "af_nicole", weight = 10 }
codes.b = { voice = "am_adam",   weight = 10 }

[polly]                                         # optional; needs AWS creds (see polly.md)
engine = "neural"                               # default tier for these voices
# region = "us-east-1"                          # optional; else AWS_REGION / ~/.aws/config
codes.k = { voice = "Kevin", weight = 0 }       # !ttsk only (explicit; US)
codes.r = { voice = "Brian", weight = 2 }       # !ttsr, and occasionally random !tts (en-GB)
# codes.g = { voice = "Ruth", weight = 0, engine = "generative" }  # per-voice tier override
```

- **Sections are per engine.** The server merges the sections for the engines it can run into one
  map. **Codes must be unique across sections** (a duplicate is a startup error).
- **`weight`** is the code's share of the bare-`!tts` (and unknown-code) **weighted-random** pool.
  `weight = 0` → the voice is reachable only by its explicit code, never at random. Keep paid Polly
  voices at `0` so a plain `!tts` never bills AWS. An explicit `!ttsk` always works regardless of weight.
- **`engine`** on `[polly]` is the default Polly tier; an individual voice may override it with its
  own `engine` (e.g. a `generative` voice on one code). kokoro ignores `engine`.

Kokoro voice ids come from the model (e.g. `af_nicole`, `am_adam`, `bm_george`); Polly voice ids
are names like `Brian`, `Kevin`, `Joanna`. Run the server with `-lang b` for true British kokoro
phonemes.

## kokoro vs Polly at runtime

- kokoro's sidecar **always runs**, so its section is always active.
- Polly is enabled only if `voices.toml` has a `[polly]` section **and** its AWS credentials resolve.
  If the section is present but creds are missing/invalid, the server logs `polly DISABLED …`, keeps
  serving on kokoro, and any Polly code (`!ttsk`) falls back to a weighted-random kokoro voice.

See [polly.md](polly.md) for AWS credential setup.
