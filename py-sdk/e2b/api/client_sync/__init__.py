from typing import Optional

import httpx
import logging
import threading

from e2b.api import ApiClient, limits
from e2b.connection_config import ConnectionConfig

logger = logging.getLogger(__name__)


def get_api_client(config: ConnectionConfig, **kwargs) -> ApiClient:
    return ApiClient(
        config,
        transport=get_transport(config),
        **kwargs,
    )


class TransportWithLogger(httpx.HTTPTransport):
    singleton: Optional["TransportWithLogger"] = None

    def handle_request(self, request):
        url = f"{request.url.scheme}://{request.url.host}{request.url.path}"
        logger.info(f"Request: {request.method} {url}")
        response = super().handle_request(request)

        # data = connect.GzipCompressor.decompress(response.read()).decode()
        logger.info(f"Response: {response.status_code} {url}")

        return response

    @property
    def pool(self):
        return self._pool


_transport: Optional[TransportWithLogger] = None
_transport_lock = threading.Lock()


def get_transport(config: ConnectionConfig) -> TransportWithLogger:
    if TransportWithLogger.singleton is not None:
        return TransportWithLogger.singleton

    # Serialize the first construction so concurrent clients share one pool.
    # Once published, the fast path above remains lock-free.
    with _transport_lock:
        if TransportWithLogger.singleton is None:
            TransportWithLogger.singleton = TransportWithLogger(
                limits=limits,
                proxy=config.proxy,
            )

    return TransportWithLogger.singleton
