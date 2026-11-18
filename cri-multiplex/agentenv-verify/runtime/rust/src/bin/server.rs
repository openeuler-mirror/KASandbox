#[cfg(test)]
mod shutdown_tests {
    use super::drain_server_task;
    use std::time::Duration;
    use tokio::io::AsyncWriteExt;
    use tokio::sync::oneshot;

    #[tokio::test]
    async fn incomplete_http_request_does_not_block_shutdown() {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();
        let (shutdown_tx, shutdown_rx) = oneshot::channel();
        let task = tokio::spawn(async move {
            axum::serve(listener, axum::Router::new())
                .with_graceful_shutdown(async move {
                    let _ = shutdown_rx.await;
                })
                .await
                .map_err(anyhow::Error::from)
        });
        let mut client = tokio::net::TcpStream::connect(address).await.unwrap();
        client
            .write_all(b"GET / HTTP/1.1\r\nHost: localhost\r\nX-Pending: ")
            .await
            .unwrap();
        // 确保连接已进入 HTTP 解析，模拟不再继续发送请求头的客户端。
        tokio::time::sleep(Duration::from_millis(100)).await;
        shutdown_tx.send(()).unwrap();
        tokio::time::sleep(Duration::from_millis(100)).await;
        assert!(!task.is_finished(), "半个请求头应阻塞无限期 graceful drain");
        let abort_handle = task.abort_handle();
        tokio::time::timeout(
            Duration::from_secs(2),
            drain_server_task("HTTP test", task, Duration::from_millis(50)),
        )
        .await
        .unwrap()
        .unwrap();
        assert!(abort_handle.is_finished());
        drop(client);
    }

    #[tokio::test]
    async fn drain_allows_in_flight_task_to_finish() {
        let (finished_tx, finished_rx) = oneshot::channel();
        let task = tokio::spawn(async move {
            tokio::time::sleep(Duration::from_millis(20)).await;
            finished_tx.send(()).unwrap();
            Ok(())
        });
        drain_server_task("test", task, Duration::from_secs(2))
            .await
            .unwrap();
        finished_rx.await.unwrap();
    }

    #[tokio::test]
    async fn drain_propagates_server_failure() {
        let task = tokio::spawn(async { anyhow::bail!("server failure") });
        let error = drain_server_task("test", task, Duration::from_secs(1))
            .await
            .unwrap_err();
        assert_eq!(error.to_string(), "server failure");
    }
}
