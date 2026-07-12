# Build stage
FROM golang:1.26-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS builder

ARG VERSION=dev

WORKDIR /build

# Cache dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build
COPY . .
RUN CGO_ENABLED=0 go build -ldflags "-X github.com/KilimcininKorOglu/M365Bridge/pkg/models.Version=${VERSION}" -o bin/m365-bridge ./cmd/cli

# Runtime stage
FROM alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc

RUN apk add --no-cache ca-certificates git go tzdata

WORKDIR /app

COPY --from=builder /build/bin/m365-bridge ./bin/m365-bridge

# Data directory for tokens, cache, setup.json
RUN mkdir -p data/tokens

EXPOSE 8000

ENTRYPOINT ["./bin/m365-bridge"]
CMD ["serve", "--port", "8000"]
