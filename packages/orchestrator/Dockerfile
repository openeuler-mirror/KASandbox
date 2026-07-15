FROM ubuntu:24.04
RUN apt-get update && apt-get install -y iptables iproute2 rsync && rm -rf /var/lib/apt/lists/*
COPY orchestrator /usr/bin/orchestrator
COPY fc-netns-exec /opt/e2b-infra/bin/fc-netns-exec
RUN chmod +x /usr/bin/orchestrator /opt/e2b-infra/bin/fc-netns-exec

ENTRYPOINT [ "/usr/bin/orchestrator"]

