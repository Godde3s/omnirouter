# OmniRouter — multi-provider web-to-api router (single static binary)
FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /omnirouter .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 router
WORKDIR /app
COPY --from=builder /omnirouter /app/omnirouter
COPY .env.example /app/.env.example
COPY start.sh /app/start.sh
RUN chmod +x /app/start.sh
USER router
ENV PORT=8080 HOST=0.0.0.0
EXPOSE 8080
ENTRYPOINT ["/app/omnirouter"]
