# vpc-gateway : passerelle du réseau privé d'un projet (voir cmd/vpc-gateway).
# Alpine + iproute2, wireguard-tools et iptables.
FROM golang:1.27.4-alpine3.23 AS build
ARG GOMAXPROCS=
ARG GOMEMLIMIT=
ARG GOFLAGS=
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/vpc-gateway ./cmd/vpc-gateway

FROM alpine:3.23.7
RUN apk add --no-cache iproute2 wireguard-tools-wg iptables
COPY --from=build /out/vpc-gateway /vpc-gateway
# root dans le conteneur, mais seulement la capacité NET_ADMIN (espace
# réseau du pod).
ENTRYPOINT ["/vpc-gateway"]
