FROM debian:bookworm-slim

COPY client-proxy /client-proxy
RUN chmod +x /client-proxy

ENTRYPOINT [ "/client-proxy"]

