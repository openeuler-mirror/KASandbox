job "template-manager" {
  type = "service"
  node_pool  = "${BUILD_NODE_POOL}"
  priority = 75

  group "template-manager" {
    # Ensure one allocation per node (like a system job)
    constraint {
      operator = "distinct_hosts"
      value    = "true"
    }

    // Try to restart the task indefinitely
    // Tries to restart every 5 seconds
    restart {
      interval         = "5s"
      attempts         = 1
      delay            = "5s"
      mode             = "delay"
    }

    // For future as we can remove static and allow multiple instances on one machine if needed.
    // Also network allocation is used by Nomad service discovery on API and edge API to find jobs and register them.
    network {
      port "template-manager" {
        static = "${TEMPLATE_MANAGER_PORT}"
      }
    }

    service {
      name     = "template-manager"
      port     = "${TEMPLATE_MANAGER_PORT}"
      provider = "nomad"

      check {
        type         = "http"
        path         = "/health"
        name         = "health"
        interval     = "20s"
        timeout      = "5s"
      }
    }

    task "start" {
      driver = "raw_exec"
      kill_signal  = "SIGTERM"

      resources {
        memory     = 20480
        cpu        = 20480
      }

      env {
        NODE_ID                       = "$${node.unique.name}"
        CONSUL_TOKEN                  = "${CONSUL_ACL_TOKEN}"
        GCP_DOCKER_REPOSITORY_NAME    = "${HARBOR_HOST}"
        API_SECRET                    = "${EDGE_API_SECRET}"
        ENVIRONMENT                   = "${ENVIRONMENT}"
        DOMAIN_NAME                   = "${DOMAIN_NAME}"
        TEMPLATE_BUCKET_NAME          = "${TEMPLATE_BUCKET_NAME}"
        BUILD_CACHE_BUCKET_NAME       = "${BUILD_CACHE_BUCKET_NAME}"
        OTEL_COLLECTOR_GRPC_ENDPOINT  = "${OTEL_COLLECTOR_GRPC_ENDPOINT}"
        LOGS_COLLECTOR_ADDRESS        = "${LOGS_COLLECTOR_ADDRESS}"
        ORCHESTRATOR_SERVICES         = "template-manager"
        ALLOW_SANDBOX_INTERNET        = "${ALLOW_SANDBOX_INTERNET}"
        SHARED_CHUNK_CACHE_PATH       = "${SHARED_CHUNK_CACHE_PATH}"
        CLICKHOUSE_CONNECTION_STRING  = "clickhouse://${CLICKHOUSE_USERNAME}:${CLICKHOUSE_PASSWORD}@127.0.0.1:${CLICKHOUSE_SERVER_PORT}/${CLICKHOUSE_DATABASE}"
        DOCKERHUB_REMOTE_REPOSITORY_URL  = ""
        GRPC_PORT                     = "${TEMPLATE_MANAGER_PORT}"
        GIN_MODE                      = "release"
        ARTIFACTS_REGISTRY_PROVIDER   = "Local"
        STORAGE_PROVIDER = "${STORAGE_PROVIDER}"
        MINIO_ENDPOINT = "${MINIO_ENDPOINT}"
        MINIO_ACCESS_KEY = "${MINIO_ACCESS_KEY}"
        MINIO_SECRET_KEY = "${MINIO_SECRET_KEY}"
        MAX_STARTING_INSTANCES_PER_NODE = "30"
      }

      config {
        command = "/bin/bash"
        args    = ["-c", " chmod +x /usr/bin/template-manager && /usr/bin/template-manager --port ${TEMPLATE_MANAGER_PORT}"]
      }
    }
  }
}

