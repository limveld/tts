# Playing TTS in OBS / Streamlabs (no BlackHole)

The server can play each TTS clip through an **OBS/Streamlabs Browser Source** instead of
your machine's speakers. OBS renders the browser source's audio natively into the stream mix,
so you need **no virtual audio device (BlackHole) and no desktop-audio capture** — and it
works whether the server runs on the streaming machine or a different one.

This is the default (`-player browser`). Use `-player vlc` to play on local speakers instead
(handy for testing without OBS).

## Add the Browser Source

1. Run the server: `mise run server:serve` (browser mode is the default). The log prints the
   exact URL to use.
2. In OBS/Streamlabs: **Sources → + → Browser**.
   - **URL:** `http://127.0.0.1:8080/overlay`
   - **Width/Height:** small (e.g. 50×50) — it's audio-only; the page is transparent.
   - Tick **"Control audio via OBS"** so you can set its volume and monitor it back to your
     own speakers (Audio Mixer → gear → Advanced Audio Properties → Monitor).
3. Fire a `!tts` (or `curl -XPOST http://127.0.0.1:8080/say -d 'text=hello'`). The audio comes
   through OBS. `!skip` cuts the current clip.

If the Browser Source isn't added/open, clips are simply dropped (the queue never stalls).

## Running the server on a different machine

Because the audio plays inside OBS on the **streaming** machine, the server itself can live
elsewhere (a headless box, a Pi, a spare PC):

1. Start the server bound to the network **with a token**:
   `./bin/tts-server -addr 0.0.0.0:8080 -token YOUR_SECRET`
   (open/forward the port; keep it LAN-only or behind a VPN/tunnel).
2. Browser Source URL = `http://<server-host>:8080/overlay?token=YOUR_SECRET`
   (a Browser Source can't send auth headers, so the overlay takes the token as a query
   param; the page forwards it to the event/clip/done endpoints automatically).
3. Point your Twitch bot / `/say` callers at `http://<server-host>:8080` with the bearer token.

## How it works

Server → page over **Server-Sent Events** (`play`/`stop`); the page plays the clip via an
`<audio>` element and POSTs `/overlay/done` when it finishes, which lets the queue stay
serialized. All standard-library, no extra dependencies. Endpoints: `GET /overlay`,
`GET /overlay/events`, `GET /overlay/clip/{id}.wav`, `POST /overlay/done`.
