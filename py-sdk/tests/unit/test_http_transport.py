"""Transport initialization tests; no sandbox or network service is required."""

import concurrent.futures
import threading
import time
import unittest
from types import SimpleNamespace
from unittest.mock import patch

import httpx

from e2b.api import client_sync


class TransportInitializationTest(unittest.TestCase):
    def setUp(self):
        self.previous = client_sync.TransportWithLogger.singleton
        client_sync.TransportWithLogger.singleton = None
        self.config = SimpleNamespace(proxy=None)

    def tearDown(self):
        transport = client_sync.TransportWithLogger.singleton
        if transport is not None:
            transport.close()
        client_sync.TransportWithLogger.singleton = self.previous

    def test_concurrent_first_calls_share_one_transport(self):
        workers = 16
        start = threading.Barrier(workers)
        calls = []
        original_init = httpx.HTTPTransport.__init__

        def slow_init(transport, *args, **kwargs):
            calls.append(kwargs)
            # Let the other barrier-released workers reach initialization.
            time.sleep(0.03)
            original_init(transport, *args, **kwargs)

        def get():
            start.wait(timeout=5)
            return client_sync.get_transport(self.config)

        with patch.object(httpx.HTTPTransport, "__init__", slow_init):
            with concurrent.futures.ThreadPoolExecutor(max_workers=workers) as pool:
                futures = [pool.submit(get) for _ in range(workers)]
                transports = [future.result(timeout=10) for future in futures]

        self.assertEqual(len(calls), 1)
        self.assertTrue(all(t is transports[0] for t in transports))
        self.assertIs(client_sync.get_transport(self.config), transports[0])
        self.assertIsNone(calls[0]["proxy"])
        self.assertIs(calls[0]["limits"], client_sync.limits)

    def test_failed_initialization_does_not_publish_and_can_retry(self):
        with patch.object(httpx.HTTPTransport, "__init__", side_effect=ValueError("invalid configuration")):
            with self.assertRaises(ValueError):
                client_sync.get_transport(self.config)
        self.assertIsNone(client_sync.TransportWithLogger.singleton)
        transport = client_sync.get_transport(self.config)
        self.assertIs(client_sync.TransportWithLogger.singleton, transport)


if __name__ == "__main__":
    unittest.main()
