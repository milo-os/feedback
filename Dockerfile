# Build stage
FROM golang:1.27-alpine AS builder

ARG VERSION=dev
ARG GIT_COMMIT=unknown
ARG GIT_TREE_STATE=unknown
ARG BUILD_DATE=unknown

WORKDIR /workspace

COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/
COPY migrations/ migrations/
COPY pkg/ pkg/

RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-X 'go.miloapis.com/feedback/internal/version.Version=${VERSION}' \
              -X 'go.miloapis.com/feedback/internal/version.GitCommit=${GIT_COMMIT}' \
              -X 'go.miloapis.com/feedback/internal/version.GitTreeState=${GIT_TREE_STATE}' \
              -X 'go.miloapis.com/feedback/internal/version.BuildDate=${BUILD_DATE}'" \
    -o feedback ./cmd/feedback

# Runtime stage
FROM gcr.io/distroless/static:nonroot

WORKDIR /
COPY --from=builder /workspace/feedback .
USER 65532:65532

ENTRYPOINT ["/feedback"]
