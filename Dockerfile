FROM golang:1.25-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -buildvcs=false -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/docker-control ./cmd/docker-control

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/docker-control /docker-control
ENTRYPOINT ["/docker-control", "server"]
