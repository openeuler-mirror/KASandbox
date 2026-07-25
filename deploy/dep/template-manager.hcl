job "template-manager" {
  datacenters = ["${GCP_ZONE}"]
  type = "system"
  node_pool  = "${BUILD_NODE_POOL}"
  priority = 70

  group "template-manager" {

    network {
      port "template-manager" {
        static = "${TEMPLATE_MANAGER_PORT}"
      }
    }

    service {
      name = "template-manager"
      port = "${TEMPLATE_MANAGER_PORT}"

      check {
        type         = "grpc"
        name         = "health"
        interval     = "20s"
        timeout      = "5s"
        grpc_use_tls = false
        port         = "${TEMPLATE_MANAGER_PORT}"
      }
    }

    task "start" {
      driver = "raw_exec"
      kill_signal  = "SIGTERM"
      kill_timeout = "10m"
      resources {
        memory     = ${TEMPLATE_MANAGER_RESOURCES_MEMORY_MB}
        cpu        = ${TEMPLATE_MANAGER_RESOURCES_CPU_COUNT}
      }

      env {
        NODE_ID                       = "$${node.unique.name}"
        CONSUL_TOKEN                  = "${CONSUL_ACL_TOKEN}"
        GCP_DOCKER_REPOSITORY_NAME    = "${HARBOR_HOST}"
        API_SECRET                    = "${EDGE_API_SECRET}"
        OTEL_TRACING_PRINT            = "${OTEL_TRACING_PRINT}"
        ENVIRONMENT                   = "${ENVIRONMENT}"
        TEMPLATE_BUCKET_NAME          = "${TEMPLATE_BUCKET_NAME}"
        BUILD_CACHE_BUCKET_NAME       = "${BUILD_CACHE_BUCKET_NAME}"
        OTEL_COLLECTOR_GRPC_ENDPOINT  = "${OTEL_COLLECTOR_GRPC_ENDPOINT}"
        LOGS_COLLECTOR_ADDRESS        = "${LOGS_COLLECTOR_ADDRESS}"
        ORCHESTRATOR_SERVICES         = "orchestrator,template-manager"
        LOGS_COLLECTOR_PUBLIC_IP      = "${LOGS_COLLECTOR_PUBLIC_IP}"
        ALLOW_SANDBOX_INTERNET        = "${ALLOW_SANDBOX_INTERNET}"
        SHARED_CHUNK_CACHE_PATH       = "${SHARED_CHUNK_CACHE_PATH}"
        E2B_FC_NETNS_EXEC_HELPER      = "${E2B_FC_NETNS_EXEC_HELPER}"
        E2B_USE_FC_NETNS_EXEC_HELPER  = "${E2B_USE_FC_NETNS_EXEC_HELPER}"
        GRPC_PORT                     = "${TEMPLATE_MANAGER_PORT}"
        GIN_MODE                      = "release"
        STORAGE_PROVIDER              = "${STORAGE_PROVIDER}"
        ARTIFACTS_REGISTRY_PROVIDER   = "${ARTIFACTS_REGISTRY_PROVIDER}"
        MINIO_ENDPOINT                = "${MINIO_ENDPOINT}"
        MINIO_ACCESS_KEY              = "${MINIO_ACCESS_KEY}"
        SSL_CERT_FILE                 = "${HARBOR_CERTS_DIR}/harbor.crt"
        # --- Mooncake ---
        GLOG_logtostderr              = "${GLOG_logtostderr}"
        MOONCAKE_LOCAL_HOSTNAME       = "$${attr.unique.network.ip-address}"
        MOONCAKE_MASTER_ADDR          = "${MOONCAKE_MASTER_ADDR}"
        MOONCAKE_METADATA_SERVER      = "${MOONCAKE_METADATA_SERVER}"
        MC_TCP_BIND_ADDRESS           = "$${attr.unique.network.ip-address}"
        MOONCAKE_LOCAL_BUFFER_SIZE    = "${MOONCAKE_LOCAL_BUFFER_SIZE}"
        MOONCAKE_GLOBAL_SEGMENT_SIZE  = "${MOONCAKE_GLOBAL_SEGMENT_SIZE}"
        MOONCAKE_PROTOCOL             = "${MOONCAKE_PROTOCOL}"
        MC_URMA_TRANS_MODE            = "${MC_URMA_TRANS_MODE}"
        MOONCAKE_DEVICE_NAME          = "${MOONCAKE_DEVICE_NAME}"
        MC_LOG_ENABLE                 = "${MC_LOG_ENABLE}"
        MC_LOG_DIR                    = "${MC_LOG_DIR}"
        MC_LOG_LEVEL                  = "${MC_LOG_LEVEL}"
        MC_STORE_LOCAL_HOT_CACHE_USE_SHM       = "${MC_STORE_LOCAL_HOT_CACHE_USE_SHM}"
        MC_STORE_LOCAL_HOT_BLOCK_SIZE          = "${MC_STORE_LOCAL_HOT_BLOCK_SIZE}"
        MC_STORE_LOCAL_HOT_ADMISSION_THRESHOLD = "${MC_STORE_LOCAL_HOT_ADMISSION_THRESHOLD}"
        MC_SLICE_SIZE                 = "${MC_SLICE_SIZE}"
        MC_WORKERS_PER_CTX            = "${MC_WORKERS_PER_CTX}"
        MC_MAX_WR                     = "${MC_MAX_WR}"
     }

      config {
        command = "/bin/bash"
        args    = ["-c", " chmod +x /usr/bin/template-manager && /usr/bin/template-manager --port ${TEMPLATE_MANAGER_PORT}"]
      }
    }
  }
}
