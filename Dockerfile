# syntax=docker/dockerfile:1

# --- build stage ---
FROM golang:1.26-alpine AS build
WORKDIR /src

# Module cache layer (no external deps, but keeps builds reproducible/cacheable).
COPY go.mod ./
RUN go mod download

# Only the Go sources + embedded assets are needed for the build.
COPY *.go ./
COPY internal/ ./internal/
COPY templates/ ./templates/
COPY static/ ./static/
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /server .

# --- runtime stage ---
# distroless/static ships CA certificates needed for registry TLS, and runs as nonroot.
FROM gcr.io/distroless/static:nonroot
COPY --from=build /server /server
EXPOSE 8080
ENV PORT=8080
ENTRYPOINT ["/server"]
