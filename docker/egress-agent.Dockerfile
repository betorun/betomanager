# egress-agent : monte les IP de sortie dédiées sur le nœud passerelle
# (voir cmd/egress-agent). Alpine + iproute2 (commande ip).
FROM golang:1.27.4-alpine3.23 AS build
ARG GOMAXPROCS=
ARG GOMEMLIMIT=
ARG GOFLAGS=
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/egress-agent ./cmd/egress-agent

FROM alpine:3.23.7
RUN apk add --no-cache iproute2
COPY --from=build /out/egress-agent /egress-agent
# root dans le conteneur, mais seulement la capacité NET_ADMIN (DaemonSet).
ENTRYPOINT ["/egress-agent"]
