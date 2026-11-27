FROM debian:bookworm-slim

COPY api /api
RUN chmod +x /api

ENV GIN_MODE=release
ENTRYPOINT [ "/api"]
