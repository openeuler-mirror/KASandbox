// 与 HTTP 回归互补：在隔离 RocksDB 中验证输入拒绝、并发和持久化。

#[tokio::test]
async fn required_fields_rejected_without_catalog_writes() {
    let (app, _temp) = test_router().await;
    for endpoint in ["pause-snapshot", "snapshot-templates"] {
        for field in [
            "operation_id",
            "team_id",
            "base_template_id",
            "source_pod_uid",
            "source_node_name",
            "sandbox_started_at",
            "vcpu",
            "ram_mb",
            "total_disk_size_mb",
            "kernel_version",
            "firecracker_version",
            "envd_version",
            "secure",
        ] {
            for null in [false, true] {
                let mut body = metadata_body("invalid");
                if null {
                    body[field] = Value::Null;
                } else {
                    body.as_object_mut().unwrap().remove(field);
                }
                let (status, error) = request(
                    &app,
                    "POST",
                    &format!("/internal/sandboxes/invalid/{endpoint}"),
                    Some(TOKEN),
                    Some(body),
                )
                .await;
                assert_eq!(
                    status,
                    StatusCode::BAD_REQUEST,
                    "{endpoint} {field} null={null}"
                );
                assert_eq!(error["code"], 400);
            }
        }
    }
    let (_, state) = request(
        &app,
        "GET",
        "/internal/sandboxes/invalid/snapshot-state",
        Some(TOKEN),
        None,
    )
    .await;
    assert_eq!(state["paused"], false);
}

#[tokio::test]
async fn json_query_and_duplicate_auth_have_structured_errors() {
    let (app, _temp) = test_router().await;
    for (method, uri, raw) in [
        ("POST", "/internal/sandboxes/sb/pause-snapshot", "{"),
        ("POST", "/internal/sandboxes/sb/snapshot-templates", "[]"),
        ("POST", "/internal/builds/build/status", "{}"),
        ("GET", "/internal/snapshots/id/resolve", ""),
        ("DELETE", "/internal/snapshot-templates/id", ""),
        ("DELETE", "/internal/templates/id?team_id=a&team_id=b", ""),
    ] {
        let response = app
            .clone()
            .oneshot(
                HttpRequest::builder()
                    .method(method)
                    .uri(uri)
                    .header(INTERNAL_ADMIN_TOKEN_HEADER, TOKEN)
                    .header("content-type", "application/json")
                    .body(Body::from(raw))
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(response.status(), StatusCode::BAD_REQUEST);
        let bytes = axum::body::to_bytes(response.into_body(), 65536)
            .await
            .unwrap();
        let error: Value = serde_json::from_slice(&bytes).unwrap();
        assert_eq!(error["code"], 400);
    }
    let response = app
        .oneshot(
            HttpRequest::builder()
                .uri("/internal/sandboxes/sb/snapshot-state")
                .header(INTERNAL_ADMIN_TOKEN_HEADER, TOKEN)
                .header(INTERNAL_ADMIN_TOKEN_HEADER, TOKEN)
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::UNAUTHORIZED);
}

#[tokio::test]
async fn optional_objects_enforce_schema_and_preserve_false() {
    let (app, _temp) = test_router().await;
    for field in [
        "network",
        "auto_resume",
        "source_machine_info",
        "volume_mounts",
    ] {
        for value in [json!(1), json!(true), json!("bad"), json!([1])] {
            let mut body = metadata_body("invalid-object");
            body[field] = value;
            let (status, _) = request(
                &app,
                "POST",
                "/internal/sandboxes/sb/pause-snapshot",
                Some(TOKEN),
                Some(body),
            )
            .await;
            assert_eq!(status, StatusCode::BAD_REQUEST, "{field}");
        }
    }
    let mut body = metadata_body("valid-optional");
    body["secure"] = json!(false);
    body["auto_pause"] = json!(false);
    body["total_disk_size_mb"] = json!(0);
    body["volume_mounts"] = json!([]);
    let (status, allocated) = request(
        &app,
        "POST",
        "/internal/sandboxes/sb/pause-snapshot",
        Some(TOKEN),
        Some(body),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    let uri = format!(
        "/internal/builds/{}/status",
        allocated["build_id"].as_str().unwrap()
    );
    let (status, _) = request(
        &app,
        "POST",
        &uri,
        Some(TOKEN),
        Some(json!({"operation_id":"valid-optional","status":"success"})),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    let (_, resume) = request(
        &app,
        "GET",
        "/internal/sandboxes/sb/last-snapshot",
        Some(TOKEN),
        None,
    )
    .await;
    assert_eq!(resume["env_secure"], false);
    assert_eq!(resume["auto_pause"], false);
    assert_eq!(resume["total_disk_size_mb"], 0);
}

#[tokio::test]
async fn concurrent_allocations_and_terminal_callbacks_are_serialized() {
    let (app, _temp) = test_router().await;
    let mut tasks = tokio::task::JoinSet::new();
    for _ in 0..16 {
        let app = app.clone();
        tasks.spawn(async move {
            request(
                &app,
                "POST",
                "/internal/sandboxes/sb/snapshot-templates",
                Some(TOKEN),
                Some(metadata_body("concurrent")),
            )
            .await
        });
    }
    let mut first: Option<Value> = None;
    while let Some(result) = tasks.join_next().await {
        let (status, value) = result.unwrap();
        assert_eq!(status, StatusCode::OK);
        if let Some(first) = &first {
            assert_eq!(first, &value);
        } else {
            first = Some(value);
        }
    }
    let build = first.unwrap()["build_id"].as_str().unwrap().to_owned();
    for terminal in ["success", "failed"] {
        let app = app.clone();
        let uri = format!("/internal/builds/{build}/status");
        tasks.spawn(async move {
            request(
                &app,
                "POST",
                &uri,
                Some(TOKEN),
                Some(json!({"operation_id":"concurrent","status":terminal})),
            )
            .await
        });
    }
    let mut statuses = Vec::new();
    while let Some(result) = tasks.join_next().await {
        statuses.push(result.unwrap().0.as_u16());
    }
    statuses.sort();
    assert_eq!(statuses, vec![200, 409]);
}

#[tokio::test]
async fn catalog_reopen_preserves_idempotency_status_and_tombstones() {
    let temp = TempDir::new().unwrap();
    let path = temp.path().join("sync-catalog");
    let body: RuntimeSnapshotMetadata =
        serde_json::from_value(metadata_body("persistent")).unwrap();
    let store = InternalMetadataStore::open(&path).await.unwrap();
    let first = store
        .create_snapshot_template("sb", body.clone())
        .await
        .unwrap();
    store
        .finish_build(
            &first.build_id,
            BuildStatusRequest {
                operation_id: "persistent".into(),
                status: BuildTerminalStatus::Success,
                error: None,
            },
        )
        .await
        .unwrap();
    drop(store);
    let store = InternalMetadataStore::open(&path).await.unwrap();
    let replay = store
        .create_snapshot_template("sb", body.clone())
        .await
        .unwrap();
    assert_eq!(replay.build_id, first.build_id);
    assert_eq!(
        store
            .resolve_snapshot(&first.snapshot_id, TEAM_ID)
            .await
            .unwrap()
            .build_id,
        first.build_id
    );
    store
        .delete_snapshot_template(&first.snapshot_id, TEAM_ID)
        .await
        .unwrap();
    store
        .delete_template("base-template", TEAM_ID)
        .await
        .unwrap();
    drop(store);
    let store = InternalMetadataStore::open(&path).await.unwrap();
    assert_eq!(
        store
            .resolve_snapshot(&first.snapshot_id, TEAM_ID)
            .await
            .unwrap_err()
            .status,
        StatusCode::NOT_FOUND
    );
    store
        .delete_snapshot_template(&first.snapshot_id, TEAM_ID)
        .await
        .unwrap();
    store
        .delete_template("base-template", TEAM_ID)
        .await
        .unwrap();
    assert_eq!(
        store
            .create_snapshot_template("sb", body)
            .await
            .unwrap()
            .build_id,
        first.build_id
    );
    assert!(
        store
            .resolve_snapshot(&first.snapshot_id, TEAM_ID)
            .await
            .is_err()
    );
}

#[tokio::test]
async fn corrupt_catalog_returns_500_without_panicking() {
    let temp = TempDir::new().unwrap();
    let store = InternalMetadataStore::open(temp.path().join("catalog"))
        .await
        .unwrap();
    store
        .store
        .put(b"snapshot/broken".to_vec(), b"broken-json".to_vec())
        .await
        .unwrap();
    let app = router(InternalApiState::new(
        Arc::new(store),
        ApiKey::new(TOKEN).unwrap(),
    ));
    let (status, error) = request(
        &app,
        "GET",
        "/internal/sandboxes/sb/snapshot-state",
        Some(TOKEN),
        None,
    )
    .await;
    assert_eq!(status, StatusCode::INTERNAL_SERVER_ERROR);
    assert_eq!(error["code"], 500);
}

#[tokio::test]
async fn catalog_crash_child() {
    let Ok(root) = std::env::var("AENV_INTERNAL_CRASH_TEST_DIR") else {
        return;
    };
    let root = std::path::PathBuf::from(root);
    let store = InternalMetadataStore::open(root.join("catalog"))
        .await
        .unwrap();
    let body = serde_json::from_value(metadata_body("crash-acknowledged")).unwrap();
    let result = store.create_snapshot_template("sb", body).await.unwrap();
    store
        .finish_build(
            &result.build_id,
            BuildStatusRequest {
                operation_id: "crash-acknowledged".into(),
                status: BuildTerminalStatus::Success,
                error: None,
            },
        )
        .await
        .unwrap();
    std::fs::write(root.join("ready"), result.snapshot_id).unwrap();
    // 父测试只终止这个独立测试进程，验证已确认的 Sync 写入不依赖正常析构。
    std::future::pending::<()>().await;
}

#[tokio::test]
async fn catalog_acknowledged_writes_survive_process_kill() {
    let temp = TempDir::new().unwrap();
    let mut child = std::process::Command::new(std::env::current_exe().unwrap())
        .args([
            "--exact",
            "api::internal::tests::catalog_crash_child",
            "--nocapture",
        ])
        .env("AENV_INTERNAL_CRASH_TEST_DIR", temp.path())
        .stdout(std::process::Stdio::null())
        .stderr(std::process::Stdio::null())
        .spawn()
        .unwrap();
    let deadline = std::time::Instant::now() + std::time::Duration::from_secs(30);
    while !temp.path().join("ready").exists() {
        if child.try_wait().unwrap().is_some() || std::time::Instant::now() > deadline {
            let _ = child.kill();
            let _ = child.wait();
            panic!("隔离 catalog 子进程未确认写入");
        }
        std::thread::sleep(std::time::Duration::from_millis(20));
    }
    child.kill().unwrap();
    child.wait().unwrap();
    let snapshot = std::fs::read_to_string(temp.path().join("ready")).unwrap();
    let store = InternalMetadataStore::open(temp.path().join("catalog"))
        .await
        .unwrap();
    assert_eq!(
        store
            .resolve_snapshot(&snapshot, TEAM_ID)
            .await
            .unwrap()
            .base_template_id
            .as_deref(),
        Some("base-template")
    );
}

#[tokio::test]
async fn internal_http_test_server() {
    let Ok(root) = std::env::var("AENV_INTERNAL_HTTP_TEST_DIR") else {
        return;
    };
    let root = std::path::PathBuf::from(root);
    let store = InternalMetadataStore::open(root.join("catalog"))
        .await
        .unwrap();
    let app = router(InternalApiState::new(
        Arc::new(store),
        ApiKey::new(TOKEN).unwrap(),
    ));
    // 独立测试进程仅监听 loopback 随机端口，不替换正在提供业务的 Runtime。
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let port = listener.local_addr().unwrap().port();
    std::fs::write(root.join("url"), format!("http://127.0.0.1:{port}")).unwrap();
    axum::serve(listener, app).await.unwrap();
}
