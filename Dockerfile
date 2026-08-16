FROM golang:1.24-bookworm AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY main.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/share-manager main.go

FROM alpine:3.22
RUN apk add --no-cache samba-common-tools nfs-utils util-linux ca-certificates curl
WORKDIR /app
COPY --from=build /out/share-manager /usr/local/bin/share-manager
COPY index.html ./
COPY static ./static
COPY share-manager-icon.svg ./static/share-manager-icon.svg
COPY header-controls.json ./static/header-controls.json
EXPOSE 8080
CMD ["/usr/local/bin/share-manager"]
