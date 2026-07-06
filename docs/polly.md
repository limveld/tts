# Amazon Polly engine

Amazon Polly is an optional **cloud** engine whose **Neural**/**Generative** voices are far more
natural than the local kokoro voices. It runs **alongside** kokoro (which is always available) —
there's no model, venv, or extra service. Voices and codes are configured in
[`voices.toml`](voices.md); Polly turns on automatically when that file has a `[polly]` section and
its AWS credentials resolve.

## AWS setup

Credentials + region come from the **AWS default credential chain** — nothing is stored in this
repo or the launchd plist. Any one of:

- `aws configure` — writes `~/.aws/credentials` + `~/.aws/config` (region). Recommended; the launchd
  service reads these too (it sets `HOME`, so `~/.aws` is found).
- Environment: `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_REGION`.
- An IAM role (EC2/SSO).

The IAM principal needs `polly:SynthesizeSpeech` (the managed **AmazonPollyReadOnlyAccess** policy
is enough). The server validates credentials + region at startup; if they're missing it logs
`polly DISABLED …` and keeps running kokoro-only (Polly codes fall back to a random kokoro voice).

Region resolves from the `[polly].region` key in `voices.toml`, else `AWS_REGION` / `~/.aws/config`.

## Output & cost

Polly returns **MP3 @ 24 kHz** (served through the OBS overlay like any clip; kokoro clips are WAV
— both work in one session). Cost is a rounding error for a Twitch bot: **Neural $16 / 1M chars ≈
$0.003 per 200-char message**; free tier 1M chars/month for 12 months. Keep paid voices at
`weight = 0` in `voices.toml` so a bare `!tts` never bills AWS. **Generative** ($30/1M) is the most
human-like tier — set it per voice via a `engine = "generative"` override.
