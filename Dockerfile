# The build stage runs on the BUILDER's architecture and cross-compiles for the
# target, so a multi-arch build needs no emulation for the Go compile.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /out/alertmanager-github-issues .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/alertmanager-github-issues /alertmanager-github-issues
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/alertmanager-github-issues"]
