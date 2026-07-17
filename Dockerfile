# ── Stage 1: Build the viewer ────────────────────────────────────────────────
FROM node:20-alpine AS viewer-builder

WORKDIR /viewer

COPY viewer/package.json viewer/package-lock.json ./
RUN npm ci --ignore-scripts

COPY viewer/ ./
# Build without VITE_AUTH_TOKEN — the UI reads the token from the input field.
RUN npm run build

# ── Stage 2: Build the Go server ─────────────────────────────────────────────
FROM golang:1.22-alpine AS go-builder

WORKDIR /app

COPY server/go.mod server/go.sum ./
RUN go mod download

COPY server/ ./

# Copy pre-built viewer assets so go:embed can pick them up.
COPY --from=viewer-builder /viewer/dist ./viewer/dist

RUN CGO_ENABLED=0 GOOS=linux go build -o cloudproxy-server .

# ── Stage 3: Minimal runtime ──────────────────────────────────────────────────
FROM alpine:latest

RUN apk --no-cache add ca-certificates

WORKDIR /app
COPY --from=go-builder /app/cloudproxy-server .

# Cloud Run sets PORT=8080 via environment variable.
EXPOSE 8080

ENTRYPOINT ["./cloudproxy-server"]
