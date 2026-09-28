# syntax=docker/dockerfile:1

FROM golang:1.25 AS builder
WORKDIR /src
ENV CGO_ENABLED=0 GOOS=linux

RUN GOBIN=/out go install -tags='no_sqlite3' -ldflags='-s -w' \
        github.com/pressly/goose/v3/cmd/goose@v3.27.1

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -trimpath -ldflags='-s -w' -o /out/tg-agent-mcp ./cmd/tg-agent-mcp \
    && install -d -m 0700 /out/service-data

FROM gcr.io/distroless/static:nonroot
ARG VCS_REF=unknown
LABEL org.opencontainers.image.revision=$VCS_REF
COPY --from=builder /out/tg-agent-mcp /app
COPY --from=builder /out/goose /goose
COPY --from=builder /src/migrations /migrations
# Каталог сессии MTProto: права 0700 и владелец nonroot - в нём лежит доступ к
# живому аккаунту. В dry-run контуре он остаётся пустым.
COPY --from=builder --chown=65532:65532 --chmod=0700 /out/service-data /var/lib/tg-agent-mcp
CMD ["/app"]
