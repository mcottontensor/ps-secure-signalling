# Builds static binaries and ships each in a minimal, non-root image.
#   docker build -f deploy/Dockerfile --target wilbur -t ps-wilbur .
#   docker build -f deploy/Dockerfile --target sfu    -t ps-sfu .
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/wilbur ./cmd/wilbur && \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/sfu ./cmd/sfu && \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/teststreamer ./cmd/teststreamer

# distroless/static has CA certificates and a nonroot user, and nothing else: no shell, no package
# manager, nothing for an intruder to use.
FROM gcr.io/distroless/static-debian12:nonroot AS wilbur
COPY --from=build /out/wilbur /wilbur
# Mount the built frontend at /www and the config at /etc/wilbur/wilbur.yaml.
WORKDIR /
EXPOSE 8080 8888 8889
USER nonroot:nonroot
ENTRYPOINT ["/wilbur", "-config", "/etc/wilbur/wilbur.yaml"]

FROM gcr.io/distroless/static-debian12:nonroot AS sfu
COPY --from=build /out/sfu /sfu
USER nonroot:nonroot
ENTRYPOINT ["/sfu", "-config", "/etc/sfu/sfu.yaml"]

FROM gcr.io/distroless/static-debian12:nonroot AS teststreamer
COPY --from=build /out/teststreamer /teststreamer
USER nonroot:nonroot
ENTRYPOINT ["/teststreamer"]
