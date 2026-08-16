FROM golang:1.24-bookworm AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY main.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/share-manager main.go

FROM debian:13-slim
RUN apt-get update && apt-get install -y --no-install-recommends samba-common-bin nfs-common util-linux ca-certificates curl && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=build /out/share-manager /usr/local/bin/share-manager
COPY index.html ./
COPY static ./static
COPY share-manager-icon.svg ./static/share-manager-icon.svg
COPY header-controls.json ./static/header-controls.json
EXPOSE 8080
CMD ["/usr/local/bin/share-manager"]
