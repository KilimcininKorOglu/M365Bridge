# Build stage
FROM golang:1.26-alpine@sha256:3889b425f035be855a72fb4755265311293b6d414521f0a519d819df32222d83 AS builder

ARG VERSION=dev

WORKDIR /build

# Cache dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build
COPY . .
RUN CGO_ENABLED=0 go build -ldflags "-X github.com/KilimcininKorOglu/M365Bridge/pkg/models.Version=${VERSION}" -o bin/m365-bridge ./cmd/cli

# Runtime stage
FROM alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b

RUN apk add --no-cache ca-certificates git go tzdata

WORKDIR /app

COPY --from=builder /build/bin/m365-bridge ./bin/m365-bridge

# Data directory for tokens, cache, setup.json
RUN mkdir -p data/tokens

EXPOSE 8000

ENTRYPOINT ["./bin/m365-bridge"]
CMD ["serve", "--port", "8000"]
