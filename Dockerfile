# syntax=docker/dockerfile:1
FROM golang:1.23 AS builder
ARG VERSION
WORKDIR /build

COPY go.mod ./
COPY go.sum ./

RUN go mod download

ADD . .
# RUN --mount=type=cache,target=/root/.cache/go-build GOOS=linux go build -trimpath -ldflags "-w -s -X cmd.Version=$VERSION -X main.Version=$VERSION" -v -o mev-boost .
RUN --mount=type=cache,target=/root/.cache/go-build GOOS=linux go build -trimpath -ldflags "-s -X cmd.Version=$VERSION -X main.Version=$VERSION -linkmode external -extldflags '-static'" -v -o mev-boost .
# RUN --mount=type=cache,target=/root/.cache/go-build CGO_ENABLED=0 GOOS=linux go build \
#     -trimpath \
#     -v \
#     -ldflags "-w -s -X 'github.com/AnomalyFi/anchor/config.Version=$VERSION'" \
#     -o mev-boost .

FROM alpine
RUN apk add --no-cache libstdc++ libc6-compat
WORKDIR /app
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /build/mev-boost /app/mev-boost
EXPOSE 18550
ENTRYPOINT ["/app/mev-boost"]
