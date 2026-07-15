job "api" {
  datacenters = ["${GCP_ZONE}"]
  node_pool = "${API_NODE_POOL}"
  priority = 90

  group "api-service" {
    // Try to restart the task indefinitely
    // Tries to restart every 5 seconds
    restart {
      interval         = "5s"
      attempts         = 1
      delay            = "5s"
      mode             = "delay"
    }

    network {
      port "api" {
        static = "${API_PORT}"
      }

      port "grpc" {
        static = "${API_GRPC_PORT}"
      }
    }

    constraint {
      operator  = "distinct_hosts"
      value     = "true"
    }

    service {
      name = "api"
      port = "${API_PORT}"
      task = "start"

      check {
        type     = "http"
        name     = "health"
        path     = "/health"
        interval = "3s"
        timeout  = "3s"
        port     = "${API_PORT}"
      }
    }

    service {
      name = "api-grpc"
      port = "grpc"
      task = "start"

      check {
        type     = "tcp"
        name     = "grpc"
        interval = "3s"
        timeout  = "3s"
        port     = "grpc"
      }
    }

    task "start" {
      driver       = "docker"
      # If we need more than 30s we will need to update the max_kill_timeout in nomad
      # https://developer.hashicorp.com/nomad/docs/configuration/client#max_kill_timeout
      kill_timeout = "30s"
      kill_signal  = "SIGTERM"

      resources {
        memory_max = 40960
        memory     = 20480
        cpu        = 20000
      }

      env {
        ENVIRONMENT                    = "dev"
        NODE_ID                        = "$${node.unique.id}"
        NOMAD_TOKEN                    = "${NOMAD_ACL_TOKEN}"
        ORCHESTRATOR_PORT              = "${ORCHESTRATOR_PORT}"
        API_GRPC_PORT                  = "${API_GRPC_PORT}"
        ADMIN_TOKEN                    = "${API_ADMIN_TOKEN}"
        SANDBOX_ACCESS_TOKEN_HASH_SEED = "${SANDBOX_ACCESS_TOKEN_HASH_SEED}"

        POSTGRES_CONNECTION_STRING              = "${POSTGRES_CONNECTION_STRING}"
        AUTH_DB_CONNECTION_STRING               = "${POSTGRES_CONNECTION_STRING}"
        SUPABASE_JWT_SECRETS                    = "${SUPABASE_JWT_SECRETS}"

        LOKI_URL                      = "${LOKI_URL}"
        CLICKHOUSE_CONNECTION_STRING  = "clickhouse://${CLICKHOUSE_USERNAME}:${CLICKHOUSE_PASSWORD}@127.0.0.1:${CLICKHOUSE_SERVER_PORT}/${CLICKHOUSE_DATABASE}"

        POSTHOG_API_KEY                = "${POSTHOG_API_KEY}"
        ANALYTICS_COLLECTOR_HOST       = "${ANALYTICS_COLLECTOR_HOST}"
        ANALYTICS_COLLECTOR_API_TOKEN  = "${ANALYTICS_COLLECTOR_API_TOKEN}"
        LOGS_COLLECTOR_ADDRESS         = "${LOGS_COLLECTOR_ADDRESS}"
        OTEL_COLLECTOR_GRPC_ENDPOINT   = "${OTEL_COLLECTOR_GRPC_ENDPOINT}"

        REDIS_URL                      = "${REDIS_URL}:${REDIS_PORT}"
        REDIS_CLUSTER_URL              = ""
        REDIS_TLS_CA_BASE64            = ""

        SANDBOX_STORAGE_BACKEND        = "${SANDBOX_STORAGE_BACKEND}"
        # This is here just because it is required in some part of our code which is transitively imported
        TEMPLATE_BUCKET_NAME          = "skip"
      }

      config {
        network_mode = "host"
        dns_servers  = ["127.0.0.1"]
        image        = "${REGISTRY_URL}/${API_DOCKER_IMAGE}"
        auth_soft_fail = true #TODO:合入时需删除
        ports        = ["${API_PORT_NAME}"]
        args         = [
          "--port", "${API_PORT}",
        ]
      }
    }

    task "db-migrator" {
      driver = "docker"

      env {
        POSTGRES_CONNECTION_STRING="${POSTGRES_CONNECTION_STRING}"
      }

      config {
        network_mode = "host"
        image = "${REGISTRY_URL}/${DB_MIGRATOR_DOCKER_IMAGE}"
        auth_soft_fail = true #TODO:合入时需删除
      }

      resources {
        cpu    = 250
        memory = 128
      }

      lifecycle {
        hook = "prestart"
        sidecar = false
      }
    }
  }
}

