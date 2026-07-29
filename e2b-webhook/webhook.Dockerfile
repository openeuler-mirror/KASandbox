FROM debian:bookworm-slim

COPY e2b-webhook /usr/bin/e2b-webhook

EXPOSE 8443

ENTRYPOINT ["/usr/bin/e2b-webhook"]
