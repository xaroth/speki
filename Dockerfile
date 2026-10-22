# Build with cgo: the search index uses mattn/go-sqlite3.
FROM golang:1.27-bookworm@sha256:972b4110ba0f47539a975c31e52c64765e1ff19f2d74469ae4a595c6676e80a5 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=1 go build -tags 'sqlite_fts5,sqlite_omit_load_extension' \
    -ldflags "-s -w -X main.version=${VERSION}" -o /out/speki ./cmd/speki

FROM gcr.io/distroless/base-debian12:nonroot@sha256:7f0c72cd138b442ae0deeb69c08b1acf5525439ba251a49ad93c320a061567e5
COPY --from=build /out/speki /usr/local/bin/speki
WORKDIR /etc/speki
# Only the public port is exposed. Prometheus scrapes the internal port (9090) over the container
# network; don't publish it.
EXPOSE 8080
VOLUME /var/lib/speki
ENTRYPOINT ["/usr/local/bin/speki"]
