FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY main.go logs.html ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/forward .

FROM alpine:3.21
RUN adduser -D -u 10001 app && mkdir -p /data && chown app:app /data
USER app
COPY --from=build /out/forward /usr/local/bin/forward
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
  CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["forward"]