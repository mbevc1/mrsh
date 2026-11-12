# syntax=docker/dockerfile:1

# Build on the native platform and cross-compile, so multi-arch builds need
# no emulation (the runtime stage only copies files).
FROM --platform=$BUILDPLATFORM golang:1.27.4-alpine@sha256:0a5f7fe796599ff487605987432e7619ae45beeb2fba4c2ce0b907879932aebf AS build

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
FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329

COPY --from=build /out/mrsh /usr/local/bin/mrsh

USER 65532:65532
ENTRYPOINT ["/usr/local/bin/mrsh"]
