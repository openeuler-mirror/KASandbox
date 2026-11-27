FROM ubuntu:24.04
RUN apt-get update && apt-get install -y iptables iproute2 rsync && rm -rf /var/lib/apt/lists/*
COPY orchestrator /usr/bin/orchestrator
COPY fc-netns-exec /opt/e2b-infra/bin/fc-netns-exec
# The KASandbox chart probes the template manager with /bin/grpc_health_probe; the RPM ships it in bin/
COPY grpc_health_probe /bin/grpc_health_probe
RUN chmod +x /usr/bin/orchestrator /opt/e2b-infra/bin/fc-netns-exec /bin/grpc_health_probe

ENTRYPOINT [ "/usr/bin/orchestrator"]
