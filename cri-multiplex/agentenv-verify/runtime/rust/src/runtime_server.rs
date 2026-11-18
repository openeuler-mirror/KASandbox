#[cfg(test)]
mod tests {
    use super::*;
    use crate::runtime_contract::v1::{RuntimeNetworkConfig, SandboxSource};
    use crate::snapshot::{CommittedSnapshot, SnapshotAlias, SnapshotId};
    use std::net::Ipv4Addr;
    #[test]
    fn operation_codes_roundtrip_and_accept_legacy_names() {
        let cases = [
            (tonic::Code::Ok, "OK"),
            (tonic::Code::Cancelled, "CANCELLED"),
            (tonic::Code::Unknown, "UNKNOWN"),
            (tonic::Code::InvalidArgument, "INVALID_ARGUMENT"),
            (tonic::Code::DeadlineExceeded, "DEADLINE_EXCEEDED"),
            (tonic::Code::NotFound, "NOT_FOUND"),
            (tonic::Code::AlreadyExists, "ALREADY_EXISTS"),
            (tonic::Code::PermissionDenied, "PERMISSION_DENIED"),
            (tonic::Code::ResourceExhausted, "RESOURCE_EXHAUSTED"),
            (tonic::Code::FailedPrecondition, "FAILED_PRECONDITION"),
            (tonic::Code::Aborted, "ABORTED"),
            (tonic::Code::OutOfRange, "OUT_OF_RANGE"),
            (tonic::Code::Unimplemented, "UNIMPLEMENTED"),
            (tonic::Code::Internal, "INTERNAL"),
            (tonic::Code::Unavailable, "UNAVAILABLE"),
            (tonic::Code::DataLoss, "DATA_LOSS"),
            (tonic::Code::Unauthenticated, "UNAUTHENTICATED"),
        ];
        for (code, name) in cases {
            assert_eq!(operation_error_code(code), name);
            assert_eq!(operation_code_from_name(name), code);
            assert_eq!(
                operation_code_from_name(&format!("{code:?}").to_ascii_uppercase()),
                code
            );
        }
        assert_eq!(
            operation_code_from_name("unrecognized"),
            tonic::Code::Internal
        );
    }


    #[tokio::test]
    async fn prepare_runtime_uds_refuses_live_listener() {
        let directory = tempfile::tempdir().expect("tempdir");
        let socket_path = directory.path().join("agentenv-runtime.sock");
        let listener =
            std::os::unix::net::UnixListener::bind(&socket_path).expect("bind live socket");

        let error = match prepare_runtime_uds(socket_path.clone()) {
            Ok(_) => panic!("live listener must not be replaced"),
            Err(error) => error,
        };
        assert!(error.to_string().contains("live listener"), "{error:#}");
        std::os::unix::net::UnixStream::connect(&socket_path).expect("original listener remains");
        drop(listener);
    }

    #[tokio::test]
    async fn prepare_runtime_uds_replaces_stale_socket() {
        let directory = tempfile::tempdir().expect("tempdir");
        let socket_path = directory.path().join("agentenv-runtime.sock");
        drop(
            std::os::unix::net::UnixListener::bind(&socket_path).expect("create stale socket file"),
        );

        let prepared = prepare_runtime_uds(socket_path.clone()).expect("replace stale socket");
        assert!(socket_path.exists());
        drop(prepared);
        std::fs::remove_file(&socket_path).expect("remove test socket");
    }

    fn valid_request() -> SandboxCreateRequest {
        SandboxCreateRequest {
            operation_id: "admin-create:product-1".to_string(),
            request_fingerprint: vec![0x5a; 32],
            spec: Some(SandboxSpec {
                native_sandbox_id: "018f47d0-0000-7000-8000-000000000001".to_string(),
                product_sandbox_id: "product-1".to_string(),
                alias: "demo".to_string(),
                source: Some(SandboxSource {
                    kind: Some(sandbox_source::Kind::Template(
                        crate::runtime_contract::v1::TemplateSource {
                            template_id: "base".to_string(),
                            build_id: String::new(),
                        },
                    )),
                }),
                resources: Some(ResourceSpec::default()),
                auto_pause: Some(false),
                ..SandboxSpec::default()
            }),
            timeout_seconds: 60,
        }
    }






    #[test]
    fn validates_minimal_template_create() {
        let validated = validate_create_request(&valid_request()).expect("valid create request");

        assert_eq!(validated.template_id, "base");
        assert_eq!(validated.product_sandbox_id.as_deref(), Some("product-1"));
        assert_eq!(validated.alias.as_deref(), Some("demo"));
        assert!(matches!(
            validated.timeout_action,
            SandboxTimeoutAction::Delete
        ));
        assert_eq!(validated.request_fingerprint, vec![0x5a; 32]);
        assert_eq!(validated.operation_timeout_seconds, 60);
    }

    #[test]
    fn validates_template_build_selection() {
        let mut request = valid_request();
        let Some(sandbox_source::Kind::Template(template)) = request
            .spec
            .as_mut()
            .unwrap()
            .source
            .as_mut()
            .unwrap()
            .kind
            .as_mut()
        else {
            unreachable!("valid request uses template source");
        };
        template.build_id = " 018f47d0-0000-7000-8000-000000000002 ".to_string();

        let validated = validate_create_request(&request).expect("valid build selection");
        assert_eq!(
            validated.template_build_id.as_deref(),
            Some("018f47d0-0000-7000-8000-000000000002")
        );
    }

    #[test]
    fn matches_build_only_when_it_belongs_to_template() {
        let build_id =
            SnapshotId::parse("018f47d0-0000-7000-8000-000000000002").expect("valid snapshot id");
        let mut record = SnapshotRecord::mock_ready(CommittedSnapshot::mock());
        record.id = build_id;
        record.alias = Some(SnapshotAlias::parse("base").expect("valid alias"));

        assert!(snapshot_matches_template(&record, "base"));
        assert!(snapshot_matches_template(
            &record,
            "018f47d0-0000-7000-8000-000000000002"
        ));
        assert!(!snapshot_matches_template(&record, "other-template"));
    }

    #[test]
    fn validates_external_cni_network_without_taking_ownership() {
        let directory = tempfile::tempdir().expect("tempdir");
        let netns = directory.path().join("cni-netns");
        std::fs::write(&netns, []).expect("write netns placeholder");

        let mut request = valid_request();
        request.spec.as_mut().unwrap().runtime_network = Some(RuntimeNetworkConfig {
            mode: RuntimeNetworkMode::CniExternalNetns as i32,
            netns_path: netns.to_string_lossy().into_owned(),
            if_name: "eth0".to_string(),
            pod_ip: "10.244.0.5".to_string(),
            gateway: "10.244.0.1".to_string(),
            dns_servers: vec!["10.96.0.10".to_string(), "fd00::10".to_string()],
            mtu: 1500,
        });

        let validated = validate_create_request(&request).expect("valid external network");
        let external = validated.external_network.expect("external network");
        assert_eq!(external.netns_path, netns);
        assert_eq!(external.if_name, "eth0");
        assert_eq!(external.pod_ip.to_string(), "10.244.0.5");
        assert_eq!(external.dns_servers, vec![Ipv4Addr::new(10, 96, 0, 10)]);
        assert_eq!(external.mtu, Some(1500));
        assert!(
            netns.exists(),
            "validation must not delete the engine-owned netns"
        );
    }

    #[test]
    fn rejects_external_cni_network_with_missing_netns() {
        let mut request = valid_request();
        request.spec.as_mut().unwrap().runtime_network = Some(RuntimeNetworkConfig {
            mode: RuntimeNetworkMode::CniExternalNetns as i32,
            netns_path: "/definitely/missing/agentenv-netns".to_string(),
            if_name: "eth0".to_string(),
            pod_ip: "10.244.0.5".to_string(),
            ..RuntimeNetworkConfig::default()
        });
        assert_eq!(
            validate_create_request(&request).unwrap_err().code(),
            tonic::Code::InvalidArgument
        );
    }

    #[test]
    fn rejects_missing_operation_id_and_wrong_fingerprint() {
        let mut request = valid_request();
        request.operation_id.clear();
        assert_eq!(
            validate_create_request(&request).unwrap_err().code(),
            tonic::Code::InvalidArgument
        );

        let mut request = valid_request();
        request.request_fingerprint.pop();
        assert_eq!(
            validate_create_request(&request).unwrap_err().code(),
            tonic::Code::InvalidArgument
        );
    }

    #[test]
    fn rejects_image_and_unsupported_template_capabilities() {
        let mut request = valid_request();
        request.spec.as_mut().unwrap().source = Some(SandboxSource {
            kind: Some(sandbox_source::Kind::Image(
                crate::runtime_contract::v1::ImageSource {
                    image: "ubuntu:24.04".to_string(),
                },
            )),
        });
        assert_eq!(
            validate_create_request(&request).unwrap_err().code(),
            tonic::Code::Unimplemented
        );

        let mut request = valid_request();
        request.spec.as_mut().unwrap().volume_mounts.push(
            crate::runtime_contract::v1::VolumeMount {
                id: "volume-1".to_string(),
                name: "data".to_string(),
                path: "/data".to_string(),
                r#type: "exclusive".to_string(),
            },
        );
        assert_eq!(
            validate_create_request(&request).unwrap_err().code(),
            tonic::Code::Unimplemented
        );
    }

    #[tokio::test]
    async fn lifecycle_lock_serializes_same_sandbox() {
        let locks = LifecycleLockTable::default();
        let sandbox_id = SandboxId::new();
        let first_guard = locks.lock(sandbox_id).await;
        let waiter_locks = locks.clone();
        let (acquired_tx, mut acquired_rx) = tokio::sync::oneshot::channel();
        let waiter = tokio::spawn(async move {
            let _guard = waiter_locks.lock(sandbox_id).await;
            let _ = acquired_tx.send(());
        });

        assert!(
            tokio::time::timeout(Duration::from_millis(20), &mut acquired_rx)
                .await
                .is_err(),
            "second lock must wait for the first owner"
        );
        drop(first_guard);
        tokio::time::timeout(Duration::from_secs(1), &mut acquired_rx)
            .await
            .expect("second lock should acquire after release")
            .expect("lock waiter channel should remain open");
        waiter.await.expect("lock waiter task");
    }
    #[test]
    fn reconciles_unknown_operations_from_authoritative_state() {
        assert_eq!(
            unknown_operation_resolution("create", Some(SandboxState::Running)),
            UnknownOperationResolution::Succeeded
        );
        assert_eq!(
            unknown_operation_resolution("create", Some(SandboxState::Paused)),
            UnknownOperationResolution::Succeeded
        );
        assert_eq!(
            unknown_operation_resolution("create", Some(SandboxState::Creating)),
            UnknownOperationResolution::KeepUnknown
        );
        assert_eq!(
            unknown_operation_resolution("create", None),
            UnknownOperationResolution::Failed {
                error_code: "RECONCILED_SANDBOX_NOT_FOUND",
                error_message: "sandbox does not exist after create reconciliation",
            }
        );
        assert_eq!(
            unknown_operation_resolution("delete", None),
            UnknownOperationResolution::Succeeded
        );
        assert_eq!(
            unknown_operation_resolution("delete", Some(SandboxState::Killing)),
            UnknownOperationResolution::KeepUnknown
        );
        assert_eq!(
            unknown_operation_resolution("delete", Some(SandboxState::Running)),
            UnknownOperationResolution::Failed {
                error_code: "RECONCILED_SANDBOX_STILL_PRESENT",
                error_message: "sandbox still exists after delete reconciliation",
            }
        );
        assert_eq!(
            unknown_operation_resolution("pause", None),
            UnknownOperationResolution::KeepUnknown
        );
    }
    #[test]
    fn runtime_info_reports_persisted_metadata_and_active_operations() {
        let metadata = SandboxMetadata {
            product_sandbox_id: Some("product-1".to_string()),
            alias: Some("demo".to_string()),
            snapshot_id: "base-template".to_string(),
            snapshot_alias: Some("base".to_string()),
            state: SandboxState::Paused,
            runtime_versions: crate::snapshot::SnapshotRuntimeVersions::new(
                "6.6".to_string(),
                "1.12".to_string(),
                "0.4".to_string(),
                "2026.11".to_string(),
            ),
            volume_mounts: HashMap::from([("/workspace/data".to_string(), "volume-1".to_string())]),
            ..SandboxMetadata::default()
        };
        let active_operation = OperationStatus {
            operation_id: "op-unknown".to_string(),
            action: "delete".to_string(),
            state: "UNKNOWN".to_string(),
            ..OperationStatus::default()
        };

        let info = sandbox_runtime_info(&metadata, vec![active_operation]);

        assert_eq!(info.native_sandbox_id, metadata.id.to_string());
        assert_eq!(info.product_sandbox_id, "product-1");
        assert_eq!(info.phase, SandboxPhase::Paused as i32);
        assert_eq!(info.vm_state, "paused");
        assert_eq!(info.active_operations.len(), 1);
        assert_eq!(info.active_operations[0].state, "UNKNOWN");
        assert_eq!(info.storage["snapshot_id"], "base-template");
        assert_eq!(info.storage["volume./workspace/data"], "volume-1");
        assert_eq!(info.versions["kernel"], "6.6");
        assert_eq!(info.versions["tools_drive"], "2026.11");
        assert_eq!(info.extra["alias"], "demo");
        assert_eq!(info.extra["template_alias"], "base");
        assert!(info.netns_path.is_empty());
        assert!(info.hostports.is_empty());
    }
    #[test]
    fn runtime_info_reports_only_live_running_network() {
        let network = crate::sandbox::SandboxNetworkInfo {
            external_cni: false,
            pod_ip: "10.11.0.7".parse().expect("IPv4"),
            netns_path: PathBuf::from("/run/aenv/netns/live"),
        };
        for state in [
            SandboxState::Paused,
            SandboxState::Killing,
            SandboxState::Creating,
        ] {
            let metadata = SandboxMetadata {
                state,
                ..SandboxMetadata::default()
            };
            let mut info = sandbox_runtime_info(&metadata, Vec::new());
            apply_live_network_info(&mut info, Some(network.clone()));
            assert!(info.network.is_empty());
            assert!(info.netns_path.is_empty());
        }
        let metadata = SandboxMetadata {
            state: SandboxState::Running,
            ..SandboxMetadata::default()
        };
        let mut info = sandbox_runtime_info(&metadata, Vec::new());
        apply_live_network_info(&mut info, None);
        assert!(info.network.is_empty());
        for external_cni in [false, true] {
            let mut network = network.clone();
            network.external_cni = external_cni;
            apply_live_network_info(&mut info, Some(network));
            assert_eq!(
                info.network["mode"],
                if external_cni {
                    "cni-external-netns"
                } else {
                    "runtime-managed"
                }
            );
            assert_eq!(info.network["pod_ip"], "10.11.0.7");
            assert_eq!(info.network["netns_path"], info.netns_path);
            assert_eq!(info.netns_path, "/run/aenv/netns/live");
        }
    }

    #[test]
    fn maps_sandbox_phases_without_collapsing_paused_state() {
        assert_eq!(sandbox_phase(SandboxState::Running), SandboxPhase::Running);
        assert_eq!(sandbox_phase(SandboxState::Paused), SandboxPhase::Paused);
        assert_eq!(sandbox_phase(SandboxState::Killing), SandboxPhase::Deleting);
        assert_eq!(sandbox_phase(SandboxState::Forking), SandboxPhase::Creating);
    }

    #[test]
    fn maps_stable_orchestrator_errors() {
        let id = SandboxId::new();
        assert_eq!(
            orchestrator_error(OrchestratorError::SandboxAlreadyExists(id)).code(),
            tonic::Code::AlreadyExists
        );
        assert_eq!(
            orchestrator_error(OrchestratorError::SandboxNotFound(id)).code(),
            tonic::Code::NotFound
        );
        assert_eq!(
            orchestrator_error(OrchestratorError::ShuttingDown).code(),
            tonic::Code::Unavailable
        );
    }

    #[test]
    fn operation_status_preserves_persisted_state_and_result() {
        let record = OperationRecord {
            version: 1,
            operation_id: "op-1".to_string(),
            action: "create".to_string(),
            sandbox_id: "018f47d0-0000-7000-8000-000000000001".to_string(),
            request_fingerprint: vec![0x5a; 32],
            state: OperationState::Succeeded,
            started_at_ms: 1_700_000_000_000,
            finished_at_ms: Some(1_700_000_001_250),
            result: BTreeMap::from([("native_sandbox_id".to_string(), "sandbox-1".to_string())]),
            error_code: String::new(),
            error_message: String::new(),
            instance_id: "instance-1".to_string(),
            revision: 2,
            updated_at_ms: 1_700_000_001_250,
        };

        let status = operation_status_from_record(&record);
        assert_eq!(status.state, "SUCCEEDED");
        assert_eq!(status.result["native_sandbox_id"], "sandbox-1");
        assert_eq!(status.started_at.unwrap().seconds, 1_700_000_000);
        assert_eq!(status.finished_at.unwrap().nanos, 250_000_000);
    }

    #[test]
    fn maps_operation_store_errors_to_stable_grpc_codes() {
        assert_eq!(
            operation_store_error(OperationStoreError::FingerprintConflict("op-1".to_string()))
                .code(),
            tonic::Code::AlreadyExists
        );
        assert_eq!(
            operation_store_error(OperationStoreError::OutcomeUnknown("op-1".to_string())).code(),
            tonic::Code::Unavailable
        );
        assert_eq!(
            operation_store_error(OperationStoreError::RevisionConflict {
                operation_id: "op-1".to_string(),
                expected: 1,
                actual: 2,
            })
            .code(),
            tonic::Code::Aborted
        );
    }

    #[test]
    fn operation_fingerprint_is_stable_and_target_sensitive() {
        let first = operation_fingerprint("delete", &["sandbox-1"]);
        let duplicate = operation_fingerprint("delete", &["sandbox-1"]);
        let other = operation_fingerprint("delete", &["sandbox-2"]);
        assert_eq!(first, duplicate);
        assert_ne!(first, other);
    }

    #[test]
    fn checkpoint_metadata_fingerprint_is_order_independent_and_unambiguous() {
        let first = HashMap::from([
            ("build_id".to_string(), "build-1".to_string()),
            ("template_id".to_string(), "tpl-1".to_string()),
        ]);
        let reordered = HashMap::from([
            ("template_id".to_string(), "tpl-1".to_string()),
            ("build_id".to_string(), "build-1".to_string()),
        ]);
        let different = HashMap::from([
            ("build_id".to_string(), "build-1x".to_string()),
            ("template_id".to_string(), "tpl-1".to_string()),
        ]);
        assert_eq!(
            checkpoint_metadata_fingerprint(&first),
            checkpoint_metadata_fingerprint(&reordered)
        );
        assert_ne!(
            checkpoint_metadata_fingerprint(&first),
            checkpoint_metadata_fingerprint(&different)
        );
    }
}
