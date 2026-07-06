# Amazon Polly engine

The TTS server can synthesize with **Amazon Polly** — a cloud TTS API whose **Neural** voices
are far more natural than the local engines. It's a startup-selectable engine (`-engine polly`
/ `TTS_ENGINE=polly`); there's no model, venv, submodule, or extra service to run. kokoro stays
the fast default; the bot is untouched (`!tts` speaks in the configured Polly voice).

## AWS setup

Credentials + region come from the **AWS default credential chain** — nothing is stored in
this repo. Any one of:

- `aws configure` — writes `~/.aws/credentials` + `~/.aws/config` (region). Recommended; the
  launchd service reads these too.
- Environment: `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_REGION`.
- An IAM role (EC2/SSO).

The IAM principal needs `polly:SynthesizeSpeech`. The server validates credentials + region at
startup and exits with a clear message if either is missing.

## Running

```sh
# foreground
TTS_ENGINE=polly ./bin/tts-server          # or: mise run server:serve:polly
# as a service (reads ~/.aws; no co-agent, no secrets in the plist)
TTS_ENGINE=polly mise run server:service:install
```

### Flags

| Flag | Env | Default | Meaning |
|------|-----|---------|---------|
| `-engine` | `TTS_ENGINE` | `kokoro` | `kokoro` \| `polly` |
| `-polly-voice` | `POLLY_VOICE` | `Brian` | Polly VoiceId (e.g. `Brian`, `Joanna`, `Matthew`) |
| `-polly-engine` | `POLLY_ENGINE` | `neural` | `standard` \| `neural` \| `long-form` \| `generative` |
| `-polly-region` | `AWS_REGION` | `""` | AWS region (falls back to `~/.aws/config`) |

Voice/engine/region must be mutually compatible (e.g. a generative voice + `-polly-engine
generative` in a supported region) — the first `!tts` validates this against AWS.

## Output & cost

Polly returns **MP3 @ 24 kHz** (served through the OBS overlay like any clip). Cost is a
rounding error for a Twitch bot: **Neural $16 / 1M chars ≈ $0.003 per 200-char message**;
free tier 1M chars/month for 12 months. The bot's `-max-chars` caps per-message spend.
Generative ($30/1M) is the most human-like tier if you want to spend a bit more on quality.
