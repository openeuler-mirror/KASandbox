FROM debian:bookworm-slim

WORKDIR /app

# 只复制 goose 和 migrations（离线构建：goose 二进制需预先放到构建上下文）
COPY goose /usr/local/bin/goose
RUN chmod +x /usr/local/bin/goose

COPY migrations/ ./migrations/

ENV GOOSE_DRIVER=clickhouse
ENTRYPOINT ["goose", "-table", "_migrations", "-dir", "migrations", "up"]
