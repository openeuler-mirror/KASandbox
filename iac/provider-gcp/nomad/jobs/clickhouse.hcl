job "clickhouse" {
  type        = "service"
  node_pool   = "${CLICKHOUSE_NODE_POOL}"

  group "server-1" {
    count = 1


    restart {
      interval         = "5m"
      attempts         = 5
      delay            = "15s"
      mode             = "delay"
    }

    network {
      mode = "host"

      dns {
        servers = ["172.17.0.1", "8.8.8.8", "8.8.4.4", "169.254.169.254"]
      }

      port "clickhouse-http" {
        static = 8123
        to = 8123
      }

      port "clickhouse-metrics" {
        static = ${CLICKHOUSE_METRICS_PORT}
        to = ${CLICKHOUSE_METRICS_PORT}
      }

      port "clickhouse-server" {
        static = ${CLICKHOUSE_SERVER_PORT}
        to = ${CLICKHOUSE_SERVER_PORT}
      }
    }

    service {
      name = "clickhouse"
      port = "clickhouse-server"
      tags = ["server-1"]

      check {
        type     = "http"
        path     = "/ping"
        port     = "clickhouse-http"
        interval = "10s"
        timeout  = "5s"
      }
    }

    task "clickhouse-server" {
      driver = "docker"

      env {
           CLICKHOUSE_USER="${CLICKHOUSE_USERNAME}"
      }

      config {
        image = "${REGISTRY_URL}/clickhouse/clickhouse-server:${CLICKHOUSE_VERSION}"
        auth_soft_fail = true #TODO:合入时需删除
        ports = ["clickhouse-server", "clickhouse-http"]
        network_mode = "host"
        ulimit {
          nofile = "262144:262144"
        }

        extra_hosts = [
          "server-1.clickhouse.service.consul:127.0.0.1",
        ]

        volumes = [
          "/clickhouse/data:/var/lib/clickhouse",
          "local/config.xml:/etc/clickhouse-server/config.d/config.xml",
          "local/users.xml:/etc/clickhouse-server/users.d/users.xml",
        ]
      }

      resources {
        cpu    = "${CLICKHOUSE_RESOURCES_CPU_COUNT}"
        memory = "${CLICKHOUSE_RESOURCES_MEMORY_MB}"
      }

      template {
        destination = "local/config.xml"
        data        =<<EOF
<?xml version="1.0"?>
<clickhouse>
    <!-- this is undocumented but needed to enable waiting for for shutdown for a custom amount of time  -->
    <!-- see https://github.com/ClickHouse/ClickHouse/pull/77515 for more details  -->
    <shutdown_wait_unfinished>60</shutdown_wait_unfinished>
    <shutdown_wait_unfinished_queries>1</shutdown_wait_unfinished_queries>

    <!-- Use up 80% of available RAM to be on the safer side, default is 90% -->
    <max_server_memory_usage_to_ram_ratio>0.8</max_server_memory_usage_to_ram_ratio>

    <logger>
        <formatting>
            <type>json</type>
            <names>
                <date_time>date_time</date_time>
                <thread_id>thread_id</thread_id>
                <level>level</level>
                <query_id>query_id</query_id>
                <logger_name>logger_name</logger_name>
                <message>message</message>
                <source_file>source_file</source_file>
                <source_line>source_line</source_line>
            </names>
        </formatting>
        <console>1</console>
        <level>information</level>
    </logger>

    <default_replica_path>/var/lib/clickhouse/tables/{shard}/{database}/{table}</default_replica_path>

    <remote_servers replace="true">
        <cluster>
            <!-- a secret for servers to use to communicate to each other  -->
            <secret>${CLICKHOUSE_SERVER_SECRET}</secret>
            <shard>
                <replica>
                    <host>server-1.clickhouse.service.consul</host>
                    <port>${CLICKHOUSE_SERVER_PORT}</port>
                    <user>e2b</user>
                    <password>123456789</password>
                </replica>
            </shard>
        </cluster>
    </remote_servers>

    <listen_host>0.0.0.0</listen_host>

    <asynchronous_metric_log>
        <ttl>event_date + INTERVAL 7 DAY</ttl>
    </asynchronous_metric_log>

    <trace_log>
        <ttl>event_date + INTERVAL 7 DAY</ttl>
    </trace_log>

    <text_log>
        <ttl>event_date + INTERVAL 7 DAY</ttl>
    </text_log>

    <latency_log>
        <ttl>event_date + INTERVAL 7 DAY</ttl>
    </latency_log>

    <query_log>
        <ttl>event_date + INTERVAL 7 DAY</ttl>
    </query_log>

    <metric_log>
        <ttl>event_date + INTERVAL 7 DAY</ttl>
    </metric_log>

    <processors_profile_log>
        <ttl>event_date + INTERVAL 7 DAY</ttl>
    </processors_profile_log>

    <asynchronous_metric_log>
        <ttl>event_date + INTERVAL 7 DAY</ttl>
    </asynchronous_metric_log>

    <part_log>
        <ttl>event_date + INTERVAL 7 DAY</ttl>
    </part_log>

    <query_metrics_log>
        <ttl>event_date + INTERVAL 7 DAY</ttl>
    </query_metrics_log>

    <error_log>
        <ttl>event_date + INTERVAL 30 DAY</ttl>
    </error_log>

    <prometheus>
        <port>${CLICKHOUSE_METRICS_PORT}</port>
        <endpoint>/metrics</endpoint>
        <metrics>true</metrics>
        <asynchronous_metrics>true</asynchronous_metrics>
        <events>true</events>
        <errors>true</errors>
    </prometheus>

    <tcp_port>${CLICKHOUSE_SERVER_PORT}</tcp_port>
</clickhouse>
EOF
      }

      template {
        destination = "local/users.xml"
        data        =<<EOF
<?xml version="1.0"?>
<clickhouse>
    <users>
        <${CLICKHOUSE_USERNAME}>
            <password>${CLICKHOUSE_PASSWORD}</password>
            <networks>
                <!-- Allow Nomad access https://web.archive.org/web/20250618172506/https://developer.hashicorp.com/nomad/docs/configuration/client#bridge_network_subnet -->
                <ip>172.26.64.0/20</ip>
                <ip>::1</ip> <!-- allow localhost access -->
                <ip>127.0.0.1</ip>
                <ip>10.0.0.0/8</ip> <!-- restrict to internal traffic -->
            </networks>
            <profile>default</profile>

            <!-- https://clickhouse.com/docs/optimize/asynchronous-inserts -->
            <async_insert>1</async_insert>
            <wait_for_async_insert>1</wait_for_async_insert>

            <quota>default</quota>
            <access_management>1</access_management>
        </${CLICKHOUSE_USERNAME}>
    </users>
</clickhouse>
EOF
      }
    }

    task "otel-collector" {
      driver = "docker"

      config {
        network_mode = "host"

        image = "${REGISTRY_URL}/otel/opentelemetry-collector-contrib:0.119.0"
        auth_soft_fail = true #TODO:合入时需删除
        args = [
          "--config=local/otel.yaml",
          "--feature-gates=pkg.translator.prometheus.NormalizeName",
        ]
      }

      resources {
        cpu    = 250
        memory = 128
      }

      template {
        data        =<<EOF
receivers:
  prometheus:
    config:
      scrape_configs:
        - job_name: "clickhouse"
          scrape_interval: 30s
          metrics_path: /metrics
          static_configs:
            - targets: ['localhost:${CLICKHOUSE_METRICS_PORT}']
exporters:
  otlp:
    endpoint: http://localhost:${CLICKHOUSE_SERVER_PORT}
    tls:
      insecure: true
processors:
  batch: {}

  resourcedetection:
    detectors: [gcp]
    override: true
    gcp:
      resource_attributes:
        cloud.provider:
          enabled: false
        cloud.platform:
          enabled: false
        cloud.account.id:
          enabled: false
        cloud.availability_zone:
          enabled: false
        cloud.region:
          enabled: false
        host.type:
          enabled: true
        host.id:
          enabled: true
        gcp.gce.instance.name:
          enabled: true
        host.name:
          enabled: true

  transform/set-name:
    metric_statements:
      - set(datapoint.attributes["service.instance.id"], resource.attributes["gcp.gce.instance.name"])


  # keep only metrics that are used
  filter:
    metrics:
      include:
        match_type: regexp
        metric_names:
          - "nomad_client_host_cpu_idle"
          - "nomad_client_host_disk_available"
          - "nomad_client_host_disk_size"
          - "nomad_client_allocs_memory_usage"
          - "nomad_client_allocs_cpu_usage"
          - "nomad_client_host_memory_available"
          - "nomad_client_host_memory_total"
          - "nomad_client_unallocated_memory"
          - "nomad_nomad_job_summary_running"
          - "orchestrator.*"
          - "api.*"
          - "client_proxy.*"
          # ──────  Query load & latency ──────
          - ClickHouseProfileEvents_SelectQuery
          - ClickHouseProfileEvents_FailedSelectQuery
          - ClickHouseProfileEvents_SelectQueryTimeMicroseconds
          - ClickHouseProfileEvents_InsertQuery
          - ClickHouseProfileEvents_FailedInsertQuery
          - ClickHouseProfileEvents_InsertQueryTimeMicroseconds
          - ClickHouseProfileEvents_QueryTimeMicroseconds
          - ClickHouseProfileEvents_Query
          - ClickHouseMetrics_Query
          - ClickHouseProfileEvents_QueryMemoryLimitExceeded

          # ──────  Table stats ──────
          - ClickHouseAsyncMetrics_TotalRowsOfMergeTreeTables
          - ClickHouseAsyncMetrics_TotalPartsOfMergeTreeTables
          - ClickHouseAsyncMetrics_TotalBytesOfMergeTreeTables

          # ──────  Read / write throughput ──────
          - ClickHouseProfileEvents_AsyncInsertBytes
          - ClickHouseProfileEvents_AsyncInsertRows
          - ClickHouseProfileEvents_InsertedBytes
          - ClickHouseProfileEvents_InsertedRows
          - ClickHouseProfileEvents_SelectedBytes
          - ClickHouseProfileEvents_SelectedRows
          - ClickHouseProfileEvents_SlowRead

          # ──────  Memory ──────
          - ClickHouseAsyncMetrics_CGroupMemoryUsed
          - ClickHouseAsyncMetrics_CGroupMemoryTotal
          - ClickHouseAsyncMetrics_MemoryResident
          # ──────  Network ──────
          - ClickHouseMetrics_NetworkSend
          - ClickHouseMetrics_NetworkReceive

          # ──────  Disk / S3 traffic ──────
          - ClickHouseAsyncMetrics_DiskTotal_default
          - ClickHouseAsyncMetrics_DiskAvailable_default
          - ClickHouseAsyncMetrics_DiskUsed_default
          - ClickHouseProfileEvents_S3GetObject
          - ClickHouseProfileEvents_S3PutObject
          - ClickHouseProfileEvents_ReadBufferFromS3Bytes
          - ClickHouseProfileEvents_WriteBufferFromS3Bytes

          # ──────  Connections ──────
          - ClickHouseMetrics_TCPConnection
          - ClickHouseMetrics_HTTPConnectionsTotal

service:
  telemetry:
    metrics:
      readers:
        - pull:
            exporter:
              prometheus:
                host: '0.0.0.0'
                port: 9999

  pipelines:
    metrics:
      receivers:  [prometheus]
      processors: [filter, resourcedetection, transform/set-name, batch]
      exporters:  [otlp]

EOF
        destination = "local/otel.yaml"
      }

      # Order the sidecar BEFORE the app so it’s ready to receive traffic
      lifecycle {
        sidecar = "true"
        hook = "prestart"
      }
    }
  }
}

