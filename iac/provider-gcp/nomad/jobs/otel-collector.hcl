job "otel-collector" {
  type        = "system"
  node_pool   = "all"

  priority = 95

  group "otel-collector" {

    // Try to restart the task indefinitely
    // Tries to restart every 5 seconds
    restart {
      interval         = "5s"
      attempts         = 1
      delay            = "5s"
      mode             = "delay"
    }

    network {
      port "health" {
        to = 13133
      }

      port "metrics" {
        to = 8888
      }

      # Receivers
      port "grpc" {
        to = ${OTEL_COLLECTOR_GRPC_PORT}
      }

      port "http" {
        to = 4318
      }
    }

    service {
      name = "otel-collector"
      port = "grpc"
      tags = ["grpc"]

      check {
        type     = "http"
        name     = "health"
        path     = "/health"
        interval = "20s"
        timeout  = "5s"
        port     = 13133
      }
    }

    task "start-collector" {
      driver = "docker"

      config {
        network_mode = "host"
        image        = "${REGISTRY_URL}/otel/opentelemetry-collector-contrib:0.119.0"
        auth_soft_fail = true #TODO:合入时需删除
        volumes = [
          "local/config:/config",
          "/:/hostfs:ro",
        ]
        args = [
          "--config=local/config/otel-collector-config.yaml",
        ]

        ports = [
          "metrics",
          "grpc",
          "health",
          "http",
        ]
      }

      resources {
        memory_max = ${OTEL_COLLECTOR_PROXY_MAX_RESOURCES_MEMORY_MB}
        memory     = ${OTEL_COLLECTOR_PROXY_RESOURCES_MEMORY_MB}
        cpu        = ${OTEL_COLLECTOR_RESOURCES_CPU_COUNT}
      }

      env {
        NODE_NAME = "${node.unique.name}"
        NODE_ID   = "${node.unique.id}"
      }

      template {
        data        =  <<EOF
receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317
        max_recv_msg_size_mib: 100
        read_buffer_size: 10943040
        max_concurrent_streams: 200
        write_buffer_size: 10943040
  prometheus:
    config:
      scrape_configs:
        - job_name: nomad
          scrape_interval: 15s
          scrape_timeout: 5s
          metrics_path: '/v1/metrics'
          static_configs:
            - targets: ['localhost:4646']
          params:
            format: ['prometheus']

processors:
  batch:
    timeout: 5s

  batch/clickhouse:
    timeout: 5s
    send_batch_size: 50000

  # keep only metrics that are used
  filter/otlp:
    # https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/processor/filterprocessor
    metrics:
      include:
        match_type: regexp
        metric_names:
          - "orchestrator.*"
          - "template.*"
          - "api.*"
          - "client_proxy.*"
          - "Click*"
          - "otelcol.*"

  filter/prometheus:
    metrics:
      include:
        match_type: strict
        metric_names:
          - "nomad_client.host_cpu_total_percent"
          - "nomad_client_host_cpu_idle"
          - "nomad_client_host_disk_available"
          - "nomad_client_host_disk_size"
          - "nomad_client_host_memory_available"
          - "nomad_client_host_memory_total"
          - "nomad_client_allocs_memory_usage"
          - "nomad_client_allocs_memory_allocated"
          - "nomad_client_allocs_cpu_total_percent"
          - "nomad_client_allocs_cpu_allocated"

  filter/external_metrics:
    metrics:
      include:
        match_type: regexp
        metric_names:
          - "e2b.*"

  metricstransform:
    # https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/processor/metricstransformprocessor
    transforms:
      - include: "nomad_client_host_cpu_idle"
        match_type: strict
        action: update
        operations:
          - action: aggregate_labels
            aggregation_type: sum
            label_set: [instance, node_id, node_status, node_pool]

  resource/local:
      attributes:
        - action: upsert
          key: host.name
          value: "{{ env "node.unique.name" }}"
        - action: upsert
          key: service.instance.id
          value: "{{ env "node.unique.name" }}"
        - action: upsert
          key: host.type
          value: "local"
        - action: upsert
          key: host.id
          value: "{{ env "node.unique.id" }}"
        - action: upsert
          key: deployment.environment
          value: "local"

  transform/set-name:
      metric_statements:
        - delete_key(datapoint.attributes, "instance")
        - delete_key(datapoint.attributes, "node_id")
        - delete_key(datapoint.attributes, "node_scheduling_eligibility")
        - delete_key(datapoint.attributes, "node_class")
        - delete_key(datapoint.attributes, "node_status")
        - delete_key(datapoint.attributes, "service_name")
        - set(datapoint.attributes["service.instance.id"], resource.attributes["host.name"])

  filter/rpc_duration_only:
    metrics:
      include:
        match_type: regexp
        # Include info about grpc server endpoint durations - used for monitoring request times
        metric_names:
          - "rpc.server.duration.*"
  resource/remove_instance:
    attributes:
      - action: delete
        key: service.instance.id
extensions:
  basicauth/grafana_cloud:
    # https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/extension/basicauthextension
    client_auth:
      username: "${GRAFANA_USERNAME}"
      password: "${GRAFANA_OTEL_COLLECTOR_TOKEN}"

  health_check:
    endpoint: 0.0.0.0:13133
exporters:
  debug:
    verbosity: detailed
  otlphttp/grafana_cloud:
    # https://github.com/open-telemetry/opentelemetry-collector/tree/main/exporter/otlpexporter
    endpoint: "${GRAFANA_OTLP_URL}/otlp"
    auth:
      authenticator: basicauth/grafana_cloud
  clickhouse:
    endpoint: tcp://${CLICKHOUSE_HOST}:${CLICKHOUSE_SERVER_PORT}
    database: ${CLICKHOUSE_DATABASE}
    username: ${CLICKHOUSE_USERNAME}
    password: ${CLICKHOUSE_PASSWORD}
    async_insert: true
    create_schema: false
    metrics_tables:
      gauge:
        name: "metrics_gauge"
      sum:
        name: "metrics_sum"
service:
  telemetry:
    logs:
      level: warn
    metrics:
      readers:
        - periodic:
            exporter:
              otlp:
                protocol: grpc
                insecure: true
                endpoint: localhost:4317
  extensions:
    - basicauth/grafana_cloud
    - health_check
  pipelines:
    metrics:
      receivers:
        - otlp
      processors: [filter/otlp, transform/set-name, batch]
      exporters:
        - otlphttp/grafana_cloud
    metrics/prometheus:
      receivers:
        - prometheus
      processors: [filter/prometheus, metricstransform, transform/set-name, batch]
      exporters:
        - otlphttp/grafana_cloud
    metrics/rpc_only:
      receivers:
        - otlp
      processors: [filter/rpc_duration_only, resource/remove_instance, transform/set-name, batch]
      exporters:
        - otlphttp/grafana_cloud
    metrics/external:
      receivers:  [otlp]
      processors: [filter/external_metrics, batch/clickhouse]
      exporters:  [clickhouse]
    traces:
      receivers:
        - otlp
      processors: [batch]
      exporters:
        - otlphttp/grafana_cloud
    logs:
      receivers:
        - otlp
      processors: [batch]
      exporters:
        - otlphttp/grafana_cloud
EOF
        destination = "local/config/otel-collector-config.yaml"
      }
    }
  }
}
