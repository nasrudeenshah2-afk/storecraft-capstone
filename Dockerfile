# storecraft Go server image (primary or replica; the role is chosen by the command).
# Build:  docker build -t storecraft .
# Run:    docker run -p 8090:8080 -e STORECRAFT_ADMIN_TOKEN=<synthetic> -v storecraft-data:/data storecraft

FROM golang:1.27.0-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
# modernc.org/sqlite is pure Go, so CGO stays off and the binary is static.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/storecraft ./cmd/storecraft

FROM alpine:3.22.6
RUN adduser -D -H -u 10001 storecraft && mkdir /data && chown storecraft:storecraft /data
COPY --from=build /out/storecraft /usr/local/bin/storecraft
USER storecraft
WORKDIR /data
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["storecraft"]
CMD ["serve", "-role=primary", "-addr", ":8080", "-db", "/data/storecraft.db"]
