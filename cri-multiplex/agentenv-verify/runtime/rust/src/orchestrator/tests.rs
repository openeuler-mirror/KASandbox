#[test]
fn store_already_exists_maps_to_stable_orchestrator_error() {
    let sandbox_id = SandboxId::new();
    let err = OrchestratorError::from(StoreError::SandboxAlreadyExists { sandbox_id });

    assert!(matches!(
        err,
        OrchestratorError::SandboxAlreadyExists(id) if id == sandbox_id
    ));
}


#[tokio::test]
async fn finalize_reclaimed_sandbox_rejects_live_handle() -> Result<()> {
    setup();
    let orchestrator = make_orchestrator().await;
    let created = orchestrator
        .create_sandbox(create_request(
            Some(60),
            &[("team", "remediation-live-handle")],
        ))
        .await?;
    let sandbox_id = created.id;

    let error = orchestrator
        .finalize_reclaimed_sandbox(sandbox_id)
        .await
        .expect_err("live handle must reject stale remediation finalization");
    assert!(matches!(
        error,
        OrchestratorError::SandboxOperationConflict {
            sandbox_id: id,
            operation: SandboxOperation::Stop,
        } if id == sandbox_id
    ));
    assert!(orchestrator.get_sandbox(&sandbox_id).await?.is_some());
    orchestrator.delete_sandbox(sandbox_id).await?;
    Ok(())
}



#[tokio::test]
async fn finalize_reclaimed_sandbox_removes_handleless_running_record() -> Result<()> {
    setup();
    let orchestrator = make_orchestrator().await;
    let created = orchestrator
        .create_sandbox(create_request(
            Some(60),
            &[("team", "remediation-stale-record")],
        ))
        .await?;
    let sandbox_id = created.id;

    let handle = orchestrator.sandboxes.write().await.remove(&sandbox_id);
    let handle = handle.expect("created sandbox should have a runtime handle");
    handle.lock().await.stop().await?;
    orchestrator.proxy_routes.write().await.remove(&sandbox_id);
    assert!(!orchestrator.has_live_sandbox_handle(&sandbox_id).await);

    orchestrator.finalize_reclaimed_sandbox(sandbox_id).await?;
    assert!(
        orchestrator.get_sandbox(&sandbox_id).await?.is_none(),
        "stale metadata should be removed after resource reclamation"
    );
    Ok(())
}


#[tokio::test]
async fn create_sandbox_with_id_rejects_duplicate_before_second_build() -> Result<()> {
    setup();
    let behavior = Arc::new(MockBehavior::new());
    let orchestrator =
        make_orchestrator_with_factory(MockBackendFactory::with_behavior(Arc::clone(&behavior)))
            .await;
    let sandbox_id = SandboxId::new();

    let created = orchestrator
        .create_sandbox_with_id(sandbox_id, create_request(Some(60), &[]))
        .await?;
    assert_eq!(created.id, sandbox_id);

    let err = orchestrator
        .create_sandbox_with_id(sandbox_id, create_request(Some(60), &[]))
        .await
        .expect_err("duplicate native sandbox UUID must be rejected");
    assert!(matches!(
        err,
        OrchestratorError::SandboxAlreadyExists(id) if id == sandbox_id
    ));
    assert_eq!(behavior.build_calls(), 1);

    Ok(())
}



#[tokio::test(flavor = "multi_thread", worker_threads = 3)]
async fn create_sandbox_with_id_concurrent_duplicate_builds_once() -> Result<()> {
    setup();
    let behavior = Arc::new(MockBehavior::new());
    behavior.push_action(
        MockOperation::Build,
        MockAction::SucceedAfter(Duration::from_millis(100)),
    );
    let orchestrator =
        make_orchestrator_with_factory(MockBackendFactory::with_behavior(Arc::clone(&behavior)))
            .await;
    let sandbox_id = SandboxId::new();

    let (first, second) = tokio::join!(
        orchestrator.create_sandbox_with_id(sandbox_id, create_request(Some(60), &[])),
        orchestrator.create_sandbox_with_id(sandbox_id, create_request(Some(60), &[]))
    );
    let duplicate_err = match (first, second) {
        (Ok(created), Err(err)) | (Err(err), Ok(created)) => {
            assert_eq!(created.id, sandbox_id);
            err
        }
        _ => panic!("exactly one concurrent create should succeed"),
    };

    assert!(matches!(
        duplicate_err,
        OrchestratorError::SandboxAlreadyExists(id) if id == sandbox_id
    ));
    assert_eq!(behavior.build_calls(), 1);
    assert_eq!(orchestrator.sandboxes.read().await.len(), 1);

    Ok(())
}



#[tokio::test]
async fn create_sandbox_with_id_failure_releases_reservation_for_retry() -> Result<()> {
    setup();
    let behavior = Arc::new(MockBehavior::new());
    behavior.push_action(
        MockOperation::Build,
        MockAction::Fail {
            message: "forced first build failure".to_string(),
        },
    );
    let orchestrator =
        make_orchestrator_with_factory(MockBackendFactory::with_behavior(Arc::clone(&behavior)))
            .await;
    let sandbox_id = SandboxId::new();

    let err = orchestrator
        .create_sandbox_with_id(sandbox_id, create_request(Some(60), &[]))
        .await
        .expect_err("first create should fail during build");
    assert!(matches!(
        err,
        OrchestratorError::SandboxOperationFailed {
            operation: SandboxOperation::Build,
            ..
        }
    ));

    let retried = orchestrator
        .create_sandbox_with_id(sandbox_id, create_request(Some(60), &[]))
        .await?;
    assert_eq!(retried.id, sandbox_id);
    assert_eq!(behavior.build_calls(), 2);

    Ok(())
}



#[tokio::test]
async fn create_sandbox_with_id_allows_distinct_uuids() -> Result<()> {
    setup();
    let behavior = Arc::new(MockBehavior::new());
    let orchestrator =
        make_orchestrator_with_factory(MockBackendFactory::with_behavior(Arc::clone(&behavior)))
            .await;
    let first_id = SandboxId::new();
    let second_id = SandboxId::new();

    let (first, second) = tokio::join!(
        orchestrator.create_sandbox_with_id(first_id, create_request(Some(60), &[])),
        orchestrator.create_sandbox_with_id(second_id, create_request(Some(60), &[]))
    );
    assert_eq!(first?.id, first_id);
    assert_eq!(second?.id, second_id);
    assert_eq!(behavior.build_calls(), 2);
    assert_eq!(orchestrator.sandboxes.read().await.len(), 2);

    Ok(())
}



#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn create_sandbox_with_id_releases_reservation_after_caller_cancellation() -> Result<()> {
    setup();
    let behavior = Arc::new(MockBehavior::new());
    behavior.push_action(
        MockOperation::Build,
        MockAction::FailAfter {
            delay: Duration::from_millis(100),
            message: "forced cancelled create failure".to_string(),
        },
    );
    let build_started = Arc::new(tokio::sync::Notify::new());
    let build_started_hook = Arc::clone(&build_started);
    behavior.set_on_operation(
        MockOperation::Build,
        Arc::new(move || build_started_hook.notify_one()),
    );
    let orchestrator =
        make_orchestrator_with_factory(MockBackendFactory::with_behavior(Arc::clone(&behavior)))
            .await;
    let sandbox_id = SandboxId::new();

    let create_orchestrator = Arc::clone(&orchestrator);
    let create_task = tokio::spawn(async move {
        create_orchestrator
            .create_sandbox_with_id(sandbox_id, create_request(Some(60), &[]))
            .await
    });
    build_started.notified().await;
    create_task.abort();
    let _ = create_task.await;
    assert!(
        orchestrator
            .sandbox_creation_reservations
            .lock()
            .unwrap()
            .contains(&sandbox_id)
    );

    // run_cancellation_safe 会让内部任务继续完成；失败后 RAII 守卫释放 UUID。
    tokio::time::timeout(Duration::from_secs(2), async {
        loop {
            let reserved = orchestrator
                .sandbox_creation_reservations
                .lock()
                .unwrap()
                .contains(&sandbox_id);
            if !reserved {
                break;
            }
            sleep(Duration::from_millis(10)).await;
        }
    })
    .await
    .expect("creation reservation should be released after the internal task fails");
    let retried = orchestrator
        .create_sandbox_with_id(sandbox_id, create_request(Some(60), &[]))
        .await?;
    assert_eq!(retried.id, sandbox_id);
    assert_eq!(behavior.build_calls(), 2);
    orchestrator.shutdown().await?;

    Ok(())
}


#[tokio::test]
async fn live_network_info_requires_running_metadata_and_current_handle() -> Result<()> {
    let orchestrator = make_orchestrator_without_background(InMemoryMetadataStore::new());
    let id = SandboxId::new();
    let behavior = Arc::new(MockBehavior::new());
    let expected = crate::sandbox::SandboxNetworkInfo {
        external_cni: false,
        pod_ip: Ipv4Addr::new(10, 11, 0, 7),
        netns_path: PathBuf::from("/run/aenv/netns/current"),
    };
    behavior.set_network_info(Some(expected.clone()));
    let handle: SandboxHandle = Arc::new(Mutex::new(Box::new(MockSandboxBackend::new(behavior))));
    assert!(orchestrator.live_sandbox_network_info(&id).await?.is_none());
    orchestrator
        .sandboxes
        .write()
        .await
        .insert(id, handle.clone());
    assert!(orchestrator.live_sandbox_network_info(&id).await?.is_none());
    let mut metadata = SandboxMetadata {
        id,
        state: SandboxState::Running,
        ..SandboxMetadata::default()
    };
    orchestrator.store.add(metadata.clone()).await?;
    assert_eq!(
        orchestrator.live_sandbox_network_info(&id).await?,
        Some(expected)
    );
    metadata.state = SandboxState::Paused;
    orchestrator.store.update(metadata.clone()).await?;
    assert!(orchestrator.live_sandbox_network_info(&id).await?.is_none());
    metadata.state = SandboxState::Running;
    orchestrator.store.update(metadata).await?;
    // 查询等待旧 backend mutex 时替换 handle，必须拒绝旧实例的网络事实。
    let held = handle.lock().await;
    let lookup = orchestrator.live_sandbox_network_info(&id);
    tokio::pin!(lookup);
    assert!(futures::poll!(lookup.as_mut()).is_pending());
    let replacement: SandboxHandle = Arc::new(Mutex::new(Box::new(MockSandboxBackend::new(
        Arc::new(MockBehavior::new()),
    ))));
    orchestrator.sandboxes.write().await.insert(id, replacement);
    drop(held);
    assert!(lookup.await?.is_none());
    orchestrator.sandboxes.write().await.remove(&id);
    assert!(orchestrator.live_sandbox_network_info(&id).await?.is_none());
    Ok(())
}
