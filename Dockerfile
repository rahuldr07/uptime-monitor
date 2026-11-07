FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /uptime-monitor .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 app
WORKDIR /data
COPY --from=build /uptime-monitor /usr/local/bin/uptime-monitor
USER app
EXPOSE 8080
ENTRYPOINT ["uptime-monitor"]
CMD ["-config", "/data/config.json"]
