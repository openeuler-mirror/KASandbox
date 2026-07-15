# Final stage
FROM debian:bookworm-slim

COPY migrator /usr/local/bin/migrator
RUN chmod +x /usr/local/bin/migrator
WORKDIR /app
COPY migrations ./migrations

ENTRYPOINT ["/usr/local/bin/migrator"]

