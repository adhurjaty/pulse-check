FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/pulse-check .

FROM alpine:3.22
RUN adduser -D -u 10001 app && mkdir /data && chown app /data
COPY --from=build /out/pulse-check /usr/local/bin/pulse-check
USER app
ENV PORT=8080 DATA_DIR=/data
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=3s CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["pulse-check"]
