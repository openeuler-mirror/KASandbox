job "redis" {
  datacenters = ["${GCP_ZONE}"]
  node_pool = "${API_NODE_POOL}"
  type = "service"
  priority = 95

  group "redis" {
    // Try to restart the task indefinitely
    // Tries to restart every 5 seconds
    restart {
      interval         = "5s"
      attempts         = 1
      delay            = "5s"
      mode             = "delay"
    }

    network {
      port "redis" {
        static = "${REDIS_PORT}"
      }
    }

    service {
      name = "redis"
      port = "${REDIS_PORT_NAME}"

      check {
        type     = "tcp"
        name     = "health"
        interval = "10s"
        timeout  = "2s"
        port     = "${REDIS_PORT}"
      }
    }

    task "start" {
      driver = "docker"

      resources {
        memory_max = 4096
        memory     = 2048
        cpu        = 1000
      }

      config {
        network_mode = "host"
        image        = "${REGISTRY_URL}/redis:${REDIS_VERSION}"
        auth_soft_fail = true #TODO:合入时需删除
        ports        = ["${REDIS_PORT_NAME}"]
        args = ["redis-server", "--port", "${REDIS_PORT}"]
      }
    }
  }
}

