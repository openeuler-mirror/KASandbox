from typing import List, Optional

import httpcore
import httpx
from e2b.connection_config import ConnectionConfig
from e2b.envd.rpc import handle_rpc_exception
from e2b.gsd.api import GSD_API_HEALTH_ROUTE, ahandle_gsd_api_exception
from e2b.gsd.checkpoint import checkpoint_connect, checkpoint_pb2
from e2b.sandbox.checkpoint.types import CheckpointInfo


class AsyncCheckpoint:
    """
    Module for checkpointing and restoring sandbox state via GSD (async).
    """

    def __init__(
        self,
        checkpointd_api_url: str,
        connection_config: ConnectionConfig,
        pool: httpcore.AsyncConnectionPool,
        transport: httpx.AsyncBaseTransport,
    ) -> None:
        self._connection_config = connection_config
        self._rpc = checkpoint_connect.CheckpointClient(
            checkpointd_api_url,
            async_pool=pool,
            json=True,
            headers=connection_config.checkpointd_headers,
        )
        self._gsd_api = httpx.AsyncClient(
            base_url=checkpointd_api_url,
            transport=transport,
            headers=connection_config.checkpointd_headers,
            verify=connection_config.verify_ssl,
        )

    async def is_running(self, request_timeout: Optional[float] = None) -> bool:
        """
        Check if GSD (checkpointd) is running in the sandbox.

        :param request_timeout: Timeout for the request in **seconds**

        :return: ``True`` if GSD is running, ``False`` otherwise
        """
        try:
            r = await self._gsd_api.get(
                GSD_API_HEALTH_ROUTE,
                timeout=self._connection_config.get_request_timeout(request_timeout),
            )

            if r.status_code == 502:
                return False

            err = await ahandle_gsd_api_exception(r)

            if err:
                raise err

        except httpx.TimeoutException:
            raise

        return True

    async def create(
        self,
        name: Optional[str] = None,
        request_timeout: Optional[float] = None,
    ) -> CheckpointInfo:
        """
        Create a checkpoint of the sandbox's current state.

        :param name: Optional name for the checkpoint
        :param request_timeout: Timeout for the request in **seconds**

        :return: CheckpointInfo with the checkpoint ID and metadata
        """
        try:
            req = checkpoint_pb2.CreateCheckpointRequest()
            if name is not None:
                req.name = name

            res = await self._rpc.acreate_checkpoint(
                req,
                request_timeout=self._connection_config.get_request_timeout(
                    request_timeout
                ),
            )

            return CheckpointInfo(
                checkpoint_id=res.checkpoint_id,
                name=name,
            )
        except Exception as e:
            raise handle_rpc_exception(e)

    async def restore(
        self,
        checkpoint_id: str,
        request_timeout: Optional[float] = None,
    ) -> bool:
        """
        Restore the sandbox to a previously created checkpoint.

        :param checkpoint_id: ID of the checkpoint to restore
        :param request_timeout: Timeout for the request in **seconds**

        :return: True if the restore was successful
        """
        try:
            res = await self._rpc.arestore_checkpoint(
                checkpoint_pb2.RestoreCheckpointRequest(
                    checkpoint_id=checkpoint_id,
                ),
                request_timeout=self._connection_config.get_request_timeout(
                    request_timeout
                ),
            )
            return res.success
        except Exception as e:
            raise handle_rpc_exception(e)

    async def list(
        self,
        request_timeout: Optional[float] = None,
    ) -> List[CheckpointInfo]:
        """
        List all checkpoints in the sandbox.

        :param request_timeout: Timeout for the request in **seconds**

        :return: List of CheckpointInfo objects
        """
        try:
            res = await self._rpc.alist_checkpoints(
                checkpoint_pb2.ListCheckpointsRequest(),
                request_timeout=self._connection_config.get_request_timeout(
                    request_timeout
                ),
            )

            result = []
            for cp in res.checkpoints:
                result.append(
                    CheckpointInfo(
                        checkpoint_id=cp.checkpoint_id,
                        name=cp.name if cp.HasField("name") else None,
                        created_at=cp.created_at,
                    )
                )
            return result
        except Exception as e:
            raise handle_rpc_exception(e)

    async def delete(
        self,
        checkpoint_id: str,
        request_timeout: Optional[float] = None,
    ) -> bool:
        """
        Delete a checkpoint.

        :param checkpoint_id: ID of the checkpoint to delete
        :param request_timeout: Timeout for the request in **seconds**

        :return: True if the deletion was successful
        """
        try:
            res = await self._rpc.adelete_checkpoint(
                checkpoint_pb2.DeleteCheckpointRequest(
                    checkpoint_id=checkpoint_id,
                ),
                request_timeout=self._connection_config.get_request_timeout(
                    request_timeout
                ),
            )
            return res.success
        except Exception as e:
            raise handle_rpc_exception(e)