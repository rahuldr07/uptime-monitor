FROM golang:1.26-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /uptime-monitor .

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata \
	&& adduser -D -H -u 10001 app \
	&& mkdir -p /data && chown app:app /data && chmod 700 /data
WORKDIR /data
COPY --from=build /uptime-monitor /usr/local/bin/uptime-monitor
USER app
EXPOSE 8080
VOLUME /data
# Assumes the default "listen": ":8080".
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
	CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["uptime-monitor"]
# Without -config the binary uses /data/config.json if present (the original
# layout), else /etc/uptime-monitor/config.json (docker-compose.yml).
CMD ["-log-format", "json"]
