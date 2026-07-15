job "client-proxy" {
  node_pool = "${API_NODE_POOL}"
  priority  = 80

  group "client-proxy" {
    // If the service fails, try up to 2 restarts in 10 minutes
    // if another restart happens, it will trigger reschedule
    restart {
      attempts = 2
      interval = "10m"
      delay    = "10s"
      mode     = "fail"
    }

    // If too many restarts happens on one node,
    // try to place it on another with exponential backoff
    reschedule {
      delay          = "30s"
      delay_function = "exponential"
      max_delay      = "10m"
      unlimited      = true
    }

    count = ${CLIENT_PROXY_COUNT}

    constraint {
      operator  = "distinct_hosts"
      value     = "true"
    }

    network {
      port "proxy" {
        static = "${EDGE_PROXY_PORT}"
      }

      port "health" {
        static = "${EDGE_HEALTH_PORT}"
      }
    }

    service {
      name = "client-proxy"
      port = "proxy"

      // This route is fallback (with lowest priority) to catch all requests as it serves sandbox traffic with dynamic subdomains
      tags = [
        "traefik.enable=true",

        "traefik.http.routers.client-proxy.rule=PathPrefix(`/`)",
        "traefik.http.routers.client-proxy.ruleSyntax=v2",
        "traefik.http.routers.client-proxy.priority=100",

        "traefik.http.services.client-proxy.loadbalancer.server.port=$${NOMAD_PORT_proxy}"
      ]

      check {
        type     = "http"
        name     = "health"
        path     = "/health"
        interval = "3s"
        timeout  = "3s"
        port     = "health"
      }
    }
    task "start" {
      driver = "docker"
      # If we need more than 30s we will need to update the max_kill_timeout in nomad
      # https://developer.hashicorp.com/nomad/docs/configuration/client#max_kill_timeout

      kill_signal  = "SIGTERM"

      resources {
        memory_max = ${CLIENT_PROXY_MAX_RESOURCES_MEMORY_MB}
        memory     = ${CLIENT_PROXY_RESOURCES_MEMORY_MB}
        cpu        = ${CLIENT_PROXY_RESOURCES_CPU_COUNT}
      }

      env {
        NODE_ID = "$${node.unique.id}"
        NODE_IP = "$${attr.unique.network.ip-address}"

        HEALTH_PORT = "$${NOMAD_PORT_health}"
        PROXY_PORT  = "$${NOMAD_PORT_proxy}"

        ENVIRONMENT = "dev"

        OTEL_COLLECTOR_GRPC_ENDPOINT = "${OTEL_COLLECTOR_GRPC_ENDPOINT}"
        LOGS_COLLECTOR_ADDRESS       = "${LOGS_COLLECTOR_ADDRESS}"

        REDIS_URL           = "${REDIS_URL}:${REDIS_PORT}"
        REDIS_CLUSTER_URL   = ""
        REDIS_TLS_CA_BASE64 = "${redis_tls_ca_base64}"

        # used only when client-proxy is deployed directly in the cluster next to the API
        API_GRPC_ADDRESS = "${API_GRPC_ADDRESS}"

      }

config {
        network_mode = "host"
        dns_servers  = ["127.0.0.1"]
        image        = "${REGISTRY_URL}/${CLIENT_PROXY_DOCKER_IMAGE}"
        auth_soft_fail = true #TODO:合入时需删除
        ports        = ["proxy", "health"]
      }
    }
  }
}

