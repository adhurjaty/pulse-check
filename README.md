# Pulse Check

Live room-based polling for classroom discussions. Go backend (standard library only), plain HTML/JS frontend, Server-Sent Events for live updates.

## Run locally

```sh
go run .            # http://localhost:8080
```

## Endpoints (so far)

- `GET /healthz`: liveness check.
- `GET /stream`: SSE test stream that sends the server time every second. Used to confirm the reverse proxy doesn't buffer events.

## Deploy

Coolify on the Hetzner server builds the `Dockerfile` and serves the app behind its Caddy proxy (TLS through Let's Encrypt) at `https://pulse.5.78.111.152.sslip.io`. The container listens on port `8080`.
