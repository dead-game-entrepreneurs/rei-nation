FROM golang:1.27.4-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/rei-nation ./cmd/rei-nation

FROM scratch
COPY --from=build /out/rei-nation /rei-nation
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
USER 65534:65534
EXPOSE 10600/tcp 5000/tcp 3658/tcp 3658/udp 3659/udp 9999/udp 18040/tcp 18041/tcp 18041/udp 18042/tcp 18043/tcp
ENTRYPOINT ["/rei-nation"]
