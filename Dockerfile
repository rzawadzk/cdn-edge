# syntax=docker/dockerfile:1.7
# ---- Build stage ----
FROM golang:1.25-alpine AS build

WORKDIR /src

# Cache dependencies first.
COPY go.mod go.sum* ./
RUN go mod download

# Copy source and build a fully static binary.
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags="-s -w" -o /out/cdn-edge .

# ---- Runtime stage ----
# Distroless: no shell, no package manager, minimal attack surface.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/cdn-edge /cdn-edge

# Expose the default data-plane port (admin can be on a separate port).
EXPOSE 8080

# Run as a non-root user (distroless nonroot = UID 65532).
USER nonroot:nonroot

ENTRYPOINT ["/cdn-edge"]
