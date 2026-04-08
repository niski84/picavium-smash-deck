# ── Stage 1: CSS ─────────────────────────────────────────────────────────────
FROM node:20-alpine AS css
WORKDIR /build
COPY package.json package-lock.json ./
RUN npm ci --omit=dev
COPY web/styles/ ./web/styles/
COPY web/pikvm/static/ ./web/pikvm/static/
RUN npx @tailwindcss/cli -i ./web/styles/input.css -o ./web/pikvm/static/app.css --minify

# ── Stage 2: Go binary ────────────────────────────────────────────────────────
FROM golang:latest AS builder
WORKDIR /build
# Install templ code generator
RUN go install github.com/a-h/templ/cmd/templ@latest
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=css /build/web/pikvm/static/app.css ./web/pikvm/static/app.css
# Regenerate templ → _templ.go files from source
RUN templ generate -path ./internal/server/views
# Static binary — no libc dependency in runtime image
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o pikvm ./cmd/pikvm/

# ── Stage 3: slim (Tesseract OCR) ────────────────────────────────────────────
FROM debian:bookworm-slim AS slim
RUN apt-get update && apt-get install -y --no-install-recommends \
        tesseract-ocr \
        tesseract-ocr-eng \
        ca-certificates \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=builder /build/pikvm .

# Config via env vars — see .env.example
ENV PORT=8095
EXPOSE 8095

ENTRYPOINT ["./pikvm", "server"]

# ── Stage 4: full (Tesseract + PaddleOCR) ────────────────────────────────────
# Build with: docker build --target full -t picavium-smash-deck:full .
# Warning: ~2 GB image due to Python + PyTorch dependencies.
FROM slim AS full
RUN apt-get update && apt-get install -y --no-install-recommends \
        python3 python3-pip \
        libglib2.0-0 libsm6 libxext6 libxrender1 \
    && rm -rf /var/lib/apt/lists/*
RUN pip3 install --no-cache-dir paddlepaddle paddleocr
