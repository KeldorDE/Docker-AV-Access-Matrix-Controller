FROM golang:1.26-alpine AS build

WORKDIR /src

COPY scripts/app.go ./app.go

RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w -buildid=" \
    -o /out/av-access-controller \
    ./app.go

FROM scratch

COPY --from=build /out/av-access-controller /av-access-controller

USER 65532:65532

EXPOSE 62225

STOPSIGNAL SIGTERM

ENTRYPOINT ["/av-access-controller"]