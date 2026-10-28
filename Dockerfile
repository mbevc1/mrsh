# syntax=docker/dockerfile:1

# Build on the native platform and cross-compile, so multi-arch builds need
# no emulation (the runtime stage only copies files).
FROM --platform=$BUILDPLATFORM golang:1.26.8-alpine@sha256:8ac98ca534ac3f51e1f420a1dd2c15e74c75cfa0f23f3ad27eb5d7236c349a0c AS build

ENV CGO_ENABLED=0 GOTOOLCHAIN=local
WORKDIR /src

ARG TARGETOS TARGETARCH
ARG VERSION=dev COMMIT="" DATE=""
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=bind,target=. \
    GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags "-s -w \
        -X github.com/mbevc1/mrsh/cmd.version=${VERSION} \
        -X github.com/mbevc1/mrsh/cmd.commit=${COMMIT} \
        -X github.com/mbevc1/mrsh/cmd.date=${DATE} \
        -X github.com/mbevc1/mrsh/cmd.builtBy=docker" \
      -o /out/mrsh .

# Distroless static: CA certificates and tzdata, no shell or package manager.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3

COPY --from=build /out/mrsh /usr/local/bin/mrsh

USER 65532:65532
ENTRYPOINT ["/usr/local/bin/mrsh"]
