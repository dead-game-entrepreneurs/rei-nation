FROM golang:1.27.4-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY configs/tsfp-certs ./configs/tsfp-certs
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/rei-nation ./cmd/rei-nation && \
    install -d -m 0755 /out/tsfp-certs && \
    install -m 0444 configs/tsfp-certs/fesl-cert.pem /out/tsfp-certs/fesl-cert.pem && \
    install -m 0400 configs/tsfp-certs/fesl-key.pem /out/tsfp-certs/fesl-key.pem

FROM scratch
COPY --from=build /out/rei-nation /rei-nation
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build --chown=65534:65534 /out/tsfp-certs /tsfp-certs
USER 65534:65534
EXPOSE 10600/tcp 5000/tcp 3658/tcp 3658/udp 3659/udp 9999/udp
EXPOSE 18070/tcp 18071/tcp 18071/udp 18072/tcp
ENTRYPOINT ["/rei-nation"]
