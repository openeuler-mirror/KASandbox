job "orchestrator" {
  type = "system"
  node_pool = "default"

  priority = 91

  group "client-orchestrator" {
    // For future as we can remove static and allow multiple instances on one machine if needed.
    // Also network allocation is used by Nomad service discovery on API and edge API to find jobs and register them.
    network {
      port "orchestrator" {
        static = "${ORCHESTRATOR_PORT}"
      }

      port "orchestrator-proxy" {
        static = "${ORCHESTRATOR_PROXY_PORT}"
      }
    }

    service {
      name = "orchestrator"
      port = "${ORCHESTRATOR_PORT}"

      provider = "nomad"

      check {
        type         = "http"
        path         = "/health"
        name         = "health"
        interval     = "20s"
        timeout      = "5s"
      }
    }

    service {
      name = "orchestrator-proxy"
      port = "${ORCHESTRATOR_PROXY_PORT}"

      provider = "nomad"

      check {
        type     = "tcp"
        name     = "health"
        interval = "30s"
        timeout  = "1s"
      }
    }

    task "start" {
      driver = "raw_exec"

      restart {
        attempts = 0
      }

      env {
        NODE_ID                      = "$${node.unique.name}"
        LOGS_COLLECTOR_ADDRESS       = "${LOGS_COLLECTOR_ADDRESS}"
        ENVIRONMENT                  = "${ENVIRONMENT}"
        ENVD_TIMEOUT                 = "${ENVD_TIMEOUT}"
        TEMPLATE_BUCKET_NAME         = "${TEMPLATE_BUCKET_NAME}"
        OTEL_COLLECTOR_GRPC_ENDPOINT = "${OTEL_COLLECTOR_GRPC_ENDPOINT}"
        ALLOW_SANDBOX_INTERNET       = "${ALLOW_SANDBOX_INTERNET}"
        CLICKHOUSE_CONNECTION_STRING = "clickhouse://${CLICKHOUSE_USERNAME}:${CLICKHOUSE_PASSWORD}@127.0.0.1:${CLICKHOUSE_SERVER_PORT}/${CLICKHOUSE_DATABASE}"
        REDIS_URL                    = "${REDIS_URL}:${REDIS_PORT}"
        REDIS_CLUSTER_URL            = ""
        REDIS_TLS_CA_BASE64          = ""
        GRPC_PORT                    = "${ORCHESTRATOR_PORT}"
        PROXY_PORT                   = "${ORCHESTRATOR_PROXY_PORT}"
        GIN_MODE                     = "release"

        CONSUL_TOKEN                 = "${CONSUL_ACL_TOKEN}"
        DOMAIN_NAME                  = "${DOMAIN_NAME}"
        SHARED_CHUNK_CACHE_PATH      = "${SHARED_CHUNK_CACHE_PATH}"
        ORCHESTRATOR_SERVICES        = "orchestrator"
        STORAGE_PROVIDER = "${STORAGE_PROVIDER}"
        ARTIFACTS_REGISTRY_PROVIDER = "${ARTIFACTS_REGISTRY_PROVIDER}"
        MINIO_ENDPOINT = "${MINIO_ENDPOINT}"
        MINIO_ACCESS_KEY = "${MINIO_ACCESS_KEY}"
        MINIO_SECRET_KEY = "${MINIO_SECRET_KEY}"
        MAX_STARTING_INSTANCES_PER_NODE = "30"
      }

      config {
        command = "/bin/bash"
        args    = ["-c", "chmod +x /usr/bin/orchestrator && /usr/bin/orchestrator"]
      }
    }
  }
}

