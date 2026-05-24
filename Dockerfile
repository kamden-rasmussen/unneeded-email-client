FROM golang:1.26-alpine AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o email-agent .

FROM alpine:latest
RUN apk --no-cache add ca-certificates tzdata
WORKDIR /data
COPY --from=builder /build/email-agent /usr/local/bin/email-agent
EXPOSE 8090
ENTRYPOINT ["email-agent"]
CMD ["--config", "/data/config.yaml", "--prefs", "/data/preferences.yaml"]
