# syntax=docker/dockerfile:1.7

ARG GO_VERSION=1.25
ARG ALPINE_VERSION=3.21

FROM golang:${GO_VERSION}-alpine AS dependencies

ENV CGO_ENABLED=0 \
    GOFLAGS=-mod=readonly

WORKDIR /src

RUN apk add --no-cache ca-certificates git

COPY go.mod go.sum ./

RUN go mod download

FROM dependencies AS source

COPY . .

FROM source AS test-base

ENV CGO_ENABLED=1

RUN apk add --no-cache build-base

FROM test-base AS integration-test

ENTRYPOINT ["go", "test"]
CMD ["-race", "-count=1", "-v", "./internal/jobs", "./internal/webhook"]

FROM alpine:${ALPINE_VERSION} AS runtime

RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S -g 10001 app \
    && adduser -S -D -H -u 10001 -G app app

WORKDIR /app

USER app

FROM source AS migrate-build

RUN go build -trimpath -ldflags="-s -w" -o /out/amocrm-migrate ./cmd/migrate

FROM runtime AS migrate

COPY --from=migrate-build --chown=app:app /out/amocrm-migrate /usr/local/bin/amocrm-migrate
COPY --from=migrate-build --chown=app:app /src/migrations /migrations

ENV MIGRATIONS_DIR=/migrations

ENTRYPOINT ["/usr/local/bin/amocrm-migrate"]
CMD ["up"]

FROM source AS api-build

RUN go build -trimpath -ldflags="-s -w" -o /out/amocrm-api ./cmd/api

FROM runtime AS api

COPY --from=api-build --chown=app:app /out/amocrm-api /usr/local/bin/amocrm-api

EXPOSE 8080

HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:8080/live || exit 1

ENTRYPOINT ["/usr/local/bin/amocrm-api"]

FROM source AS worker-build

RUN go build -trimpath -ldflags="-s -w" -o /out/amocrm-worker ./cmd/worker

FROM runtime AS worker

COPY --from=worker-build --chown=app:app /out/amocrm-worker /usr/local/bin/amocrm-worker

EXPOSE 8081

HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:8081/live || exit 1

ENTRYPOINT ["/usr/local/bin/amocrm-worker"]

FROM test-base AS test

RUN files="$(gofmt -l .)"; \
    if [ -n "$files" ]; then \
        printf 'The following Go files need formatting:\n%s\n' "$files"; \
        exit 1; \
    fi
RUN go vet ./...
RUN go test -race -count=1 ./...
