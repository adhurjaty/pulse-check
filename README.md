# Pulse Check

Live room-based polling for classroom discussions. Go backend (standard library only), plain HTML/JS frontend, Server-Sent Events for live updates.

## Run locally

```sh
go run .            # http://localhost:8080
```

## Question files

You upload a JSON question file when you create a room. `questions.schema.json` defines the format. If your file starts with `"$schema": "./questions.schema.json"`, editors with a JSON language server (such as `jsonls` in Neovim) autocomplete fields and flag mistakes as you type. See `sample-questions.json` for an example.

```json
{
  "title": "Engineering Ethics",
  "choices": ["Fine", "Fine with reservations", "Uncomfortable", "Unacceptable"],
  "questions": [
    { "text": "The question, shown large.", "detail": "Optional context, shown smaller." }
  ]
}
```

- `title` (required): up to 200 characters.
- `choices` (optional): 2 to 6 unique entries. Defaults to the four shown above.
- `questions` (required): 1 to 20 entries. `text` is required (up to 500 characters). `detail` is optional (up to 1,000 characters).

An invalid file is rejected with every problem listed at once, for example `questions[2]: unknown field "txt"`. Question numbers in these messages start at 0.

## Endpoints (so far)

- `GET /healthz`: liveness check.
- `GET /stream`: SSE test stream that sends the server time every second. Used to confirm the reverse proxy doesn't buffer events.

## Deploy

Coolify on the Hetzner server builds the `Dockerfile` and serves the app behind its Caddy proxy (TLS through Let's Encrypt) at `https://pulse.5.78.111.152.sslip.io`. The container listens on port `8080`.
