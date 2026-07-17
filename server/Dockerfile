# Build stage (kept for local server-only builds; serves the viewer/dist
# placeholder page since it doesn't build the UI. Not used for Cloud Run —
# use the root Dockerfile for that, which embeds the real viewer UI.)
FROM golang:1.22-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o cloudproxy-server .

# Run stage
FROM alpine:latest

RUN apk --no-cache add ca-certificates

WORKDIR /app
COPY --from=builder /app/cloudproxy-server .

# Cloud Run sets PORT=8080 via environment variable.
EXPOSE 8080

ENTRYPOINT ["./cloudproxy-server"]
