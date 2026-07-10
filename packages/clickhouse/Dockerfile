FROM debian:bookworm-slim

WORKDIR /app

# 只复制 goose 和 migrations
COPY goose /usr/local/bin/goose
RUN chmod +x /usr/local/bin/goose

COPY migrations-clickhouse/ ./migrations/

ENV GOOSE_DRIVER=clickhouse
ENTRYPOINT ["goose", "-table", "_migrations", "-dir", "migrations", "up"]
