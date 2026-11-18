#[cfg(test)]
impl OperationStore {
    async fn open_with_policy(
        path: impl Into<PathBuf>,
        durability: LocalStoreDurability,
        retention: Duration,
    ) -> Result<Self, OperationStoreError> {
        Self::open_with_policies(
            path.into(),
            durability,
            retention,
            DEFAULT_QUARANTINE_RETENTION,
        )
        .await
    }
}
#[cfg(test)]
mod tests {
    use std::collections::BTreeMap;

    use super::*;

    async fn test_store() -> (tempfile::TempDir, OperationStore) {
        let temp = tempfile::tempdir().expect("tempdir");
        let store = OperationStore::open_with_policy(
            temp.path().join("operations"),
            LocalStoreDurability::Sync,
            DEFAULT_OPERATION_RETENTION,
        )
        .await
        .expect("operation store");
        (temp, store)
    }

    #[tokio::test]
    async fn corrupt_records_are_quarantined_before_removal() {
        let temp = tempfile::tempdir().expect("tempdir");
        let path = temp.path().join("operations");
        let raw = LocalKvStore::open(&path, LocalStoreDurability::Sync)
            .await
            .expect("raw operation store");
        raw.put(
            operation_key("corrupt-operation").into_bytes(),
            b"not-json".to_vec(),
        )
        .await
        .expect("write corrupt operation");
        drop(raw);

        let store = OperationStore::open_with_policy(
            path.clone(),
            LocalStoreDurability::Sync,
            DEFAULT_OPERATION_RETENTION,
        )
        .await
        .expect("reopen operation store");
        assert_eq!(store.quarantined_records(), 1);
        assert!(
            store
                .get("corrupt-operation")
                .await
                .expect("get corrupt operation")
                .is_none()
        );
        assert!(store.list_unknown().await.expect("list unknown").is_empty());

        let quarantine_root = quarantine_root_for(&path);
        let mut entries = tokio::fs::read_dir(&quarantine_root)
            .await
            .expect("read quarantine directory");
        let entry = entries
            .next_entry()
            .await
            .expect("read quarantine entry")
            .expect("quarantine record");
        let quarantined: serde_json::Value = serde_json::from_slice(
            &tokio::fs::read(entry.path())
                .await
                .expect("read quarantine record"),
        )
        .expect("decode quarantine record");
        assert_eq!(quarantined["operation_key"], "operations/corrupt-operation");
        assert_eq!(
            quarantined["payload"],
            serde_json::json!([110, 111, 116, 45, 106, 115, 111, 110])
        );
        drop(store);

        let reopened = OperationStore::open_with_policy(
            path,
            LocalStoreDurability::Sync,
            DEFAULT_OPERATION_RETENTION,
        )
        .await
        .expect("reopen quarantined store");
        assert_eq!(reopened.quarantined_records(), 1);
    }

    #[tokio::test]
    async fn unknown_scan_quarantines_corrupt_records_without_failing() {
        let (_temp, store) = test_store().await;
        store
            .inner
            .store
            .put(
                operation_key("late-corrupt").into_bytes(),
                b"not-json".to_vec(),
            )
            .await
            .expect("inject corrupt operation");

        assert!(store.list_unknown().await.expect("list unknown").is_empty());
        assert_eq!(store.quarantined_records(), 1);
        assert!(
            store
                .get("late-corrupt")
                .await
                .expect("get corrupt operation")
                .is_none()
        );
    }

    #[tokio::test]
    async fn records_survive_reopen() {
        let (temp, store) = test_store().await;
        let started = store
            .begin("op-1", "create", "sandbox-1", &[0x5a; 32])
            .await
            .expect("begin operation");
        let OperationStart::Started(record) = started else {
            panic!("first begin must start");
        };
        let result = BTreeMap::from([
            ("native_sandbox_id".to_string(), "sandbox-1".to_string()),
            ("product_sandbox_id".to_string(), "product-1".to_string()),
        ]);
        store
            .succeed("op-1", record.revision, result.clone())
            .await
            .expect("finish operation");
        drop(store);

        let reopened = OperationStore::open_with_policy(
            temp.path().join("operations"),
            LocalStoreDurability::Sync,
            DEFAULT_OPERATION_RETENTION,
        )
        .await
        .expect("reopen operation store");
        let record = reopened.get("op-1").await.unwrap().expect("record");
        assert_eq!(record.state, OperationState::Succeeded);
        assert_eq!(record.result, result);
        assert_eq!(record.revision, 2);
    }

    #[tokio::test]
    async fn same_operation_is_idempotent_and_fingerprint_conflicts_are_rejected() {
        let (_temp, store) = test_store().await;
        let first = store
            .begin("op-1", "create", "sandbox-1", &[0x11; 32])
            .await
            .expect("begin operation");
        assert!(matches!(first, OperationStart::Started(_)));

        let duplicate = store
            .begin("op-1", "create", "sandbox-1", &[0x11; 32])
            .await
            .expect("duplicate operation");
        assert!(matches!(duplicate, OperationStart::Existing(_)));

        let conflict = store
            .begin("op-1", "create", "sandbox-1", &[0x22; 32])
            .await
            .expect_err("fingerprint conflict");
        assert!(matches!(
            conflict,
            OperationStoreError::FingerprintConflict(ref id) if id == "op-1"
        ));
    }

    #[tokio::test]
    async fn retry_with_generated_create_id_reuses_existing_record() {
        let (_temp, store) = test_store().await;
        let first = store
            .begin("op-1", "create", "generated-uuid-1", &[0x12; 32])
            .await
            .expect("begin operation");
        let OperationStart::Started(first) = first else {
            panic!("first begin must start");
        };

        let duplicate = store
            .begin("op-1", "create", "generated-uuid-2", &[0x12; 32])
            .await
            .expect("duplicate operation");
        let OperationStart::Existing(duplicate) = duplicate else {
            panic!("duplicate must reuse existing operation");
        };
        assert_eq!(duplicate.sandbox_id, first.sandbox_id);
        assert_eq!(duplicate.revision, first.revision);
    }

    #[tokio::test]
    async fn cas_revision_rejects_stale_completion() {
        let (_temp, store) = test_store().await;
        let OperationStart::Started(record) = store
            .begin("op-1", "delete", "sandbox-1", &[0x33; 32])
            .await
            .expect("begin operation")
        else {
            panic!("first begin must start");
        };
        store
            .succeed("op-1", record.revision, BTreeMap::new())
            .await
            .expect("first completion");
        let error = store
            .fail("op-1", record.revision, "INTERNAL", "late failure")
            .await
            .expect_err("stale completion must fail");
        assert!(matches!(
            error,
            OperationStoreError::RevisionConflict {
                expected: 1,
                actual: 2,
                ..
            }
        ));
    }

    #[tokio::test]
    async fn restart_marks_incomplete_operation_unknown() {
        let (temp, store) = test_store().await;
        store
            .begin("op-1", "create", "sandbox-1", &[0x44; 32])
            .await
            .expect("begin operation");
        drop(store);

        let reopened = OperationStore::open_with_policy(
            temp.path().join("operations"),
            LocalStoreDurability::Sync,
            Duration::ZERO,
        )
        .await
        .expect("reopen operation store");
        let record = reopened.get("op-1").await.unwrap().expect("record");
        assert_eq!(record.state, OperationState::Unknown);
        assert_eq!(record.error_code, "UNKNOWN_AFTER_RESTART");
        assert_eq!(record.revision, 2);

        let retry = reopened
            .begin("op-1", "create", "sandbox-1", &[0x44; 32])
            .await
            .expect_err("unknown operation must not retry");
        assert!(matches!(retry, OperationStoreError::OutcomeUnknown(_)));
    }

    #[tokio::test]
    async fn unknown_records_reconcile_once_with_cas() {
        let (_temp, store) = test_store().await;
        let OperationStart::Started(record) = store
            .begin("op-unknown", "delete", "sandbox-1", &[0x45; 32])
            .await
            .expect("begin operation")
        else {
            panic!("first begin must start");
        };
        let unknown = store
            .mark_unknown("op-unknown", record.revision, "runtime outcome unknown")
            .await
            .expect("mark unknown");
        let listed = store.list_unknown().await.expect("list unknown");
        assert_eq!(listed.len(), 1);
        assert_eq!(listed[0].operation_id, "op-unknown");

        let resolved = store
            .resolve_unknown_succeeded(
                "op-unknown",
                unknown.revision,
                BTreeMap::from([("native_sandbox_id".to_string(), "sandbox-1".to_string())]),
            )
            .await
            .expect("resolve unknown");
        assert_eq!(resolved.state, OperationState::Succeeded);
        assert_eq!(
            resolved.result.get("native_sandbox_id").map(String::as_str),
            Some("sandbox-1")
        );
        assert!(store.list_unknown().await.expect("list unknown").is_empty());

        let stale = store
            .resolve_unknown_failed("op-unknown", unknown.revision, "LATE", "late resolution")
            .await
            .expect_err("stale revision must fail");
        assert!(matches!(
            stale,
            OperationStoreError::RevisionConflict {
                expected,
                actual,
                ..
            } if expected == unknown.revision && actual == resolved.revision
        ));
        let duplicate = store
            .resolve_unknown_failed(
                "op-unknown",
                resolved.revision,
                "DUPLICATE",
                "duplicate resolution",
            )
            .await
            .expect_err("terminal record must not reconcile again");
        assert!(matches!(
            duplicate,
            OperationStoreError::InvalidTransition {
                state: OperationState::Succeeded,
                target: OperationState::Failed,
                ..
            }
        ));
    }
    #[tokio::test]
    async fn active_operation_scan_filters_sandbox_and_terminal_states() {
        let (_temp, store) = test_store().await;
        store
            .begin("op-running", "pause", "sandbox-1", &[0x51; 32])
            .await
            .expect("begin running operation");
        let OperationStart::Started(record) = store
            .begin("op-unknown", "delete", "sandbox-1", &[0x52; 32])
            .await
            .expect("begin unknown operation")
        else {
            panic!("first begin must start");
        };
        store
            .mark_unknown("op-unknown", record.revision, "outcome unknown")
            .await
            .expect("mark unknown");
        let OperationStart::Started(record) = store
            .begin("op-done", "create", "sandbox-1", &[0x53; 32])
            .await
            .expect("begin terminal operation")
        else {
            panic!("first begin must start");
        };
        store
            .succeed("op-done", record.revision, BTreeMap::new())
            .await
            .expect("complete operation");
        store
            .begin("op-other", "delete", "sandbox-2", &[0x54; 32])
            .await
            .expect("begin other sandbox operation");

        let mut ids = store
            .list_active_for_sandbox("sandbox-1")
            .await
            .expect("list active operations")
            .into_iter()
            .map(|record| record.operation_id)
            .collect::<Vec<_>>();
        ids.sort();
        assert_eq!(ids, vec!["op-running", "op-unknown"]);
    }
    #[tokio::test]
    async fn ttl_only_removes_terminal_records() {
        let (_temp, store) = test_store().await;
        let OperationStart::Started(terminal) = store
            .begin("terminal", "create", "sandbox-1", &[0x55; 32])
            .await
            .expect("begin terminal operation")
        else {
            panic!("terminal operation must start");
        };
        store
            .succeed("terminal", terminal.revision, BTreeMap::new())
            .await
            .expect("finish terminal operation");
        let OperationStart::Started(unknown) = store
            .begin("unknown", "create", "sandbox-2", &[0x66; 32])
            .await
            .expect("begin unknown operation")
        else {
            panic!("unknown operation must start");
        };
        store
            .mark_unknown("unknown", unknown.revision, "outcome unknown")
            .await
            .expect("mark unknown");

        let removed = store
            .purge_expired_at(
                SystemTime::now() + DEFAULT_OPERATION_RETENTION + Duration::from_secs(1),
            )
            .await
            .expect("purge expired operations");
        assert_eq!(removed, 1);
        assert!(store.get("terminal").await.unwrap().is_none());
        assert_eq!(
            store
                .get("unknown")
                .await
                .unwrap()
                .expect("unknown record")
                .state,
            OperationState::Unknown
        );
    }

    #[tokio::test]
    async fn quarantine_retention_lists_and_purges_only_old_valid_records() {
        let temp = tempfile::tempdir().expect("tempdir");
        let path = temp.path().join("operations");
        let retention = Duration::from_secs(60);
        let store = OperationStore::open_with_policies(
            path.clone(),
            LocalStoreDurability::Sync,
            DEFAULT_OPERATION_RETENTION,
            retention,
        )
        .await
        .expect("operation store");
        let quarantine_root = quarantine_root_for(&path);
        tokio::fs::create_dir_all(&quarantine_root)
            .await
            .expect("create quarantine root");

        let old_record = QuarantinedOperationRecord {
            version: 1,
            operation_key: "operations/old".to_string(),
            reason: "old corrupt record".to_string(),
            payload: b"old".to_vec(),
            quarantined_at_ms: unix_millis(SystemTime::now()) - 120_000,
        };
        let fresh_record = QuarantinedOperationRecord {
            version: 1,
            operation_key: "operations/fresh".to_string(),
            reason: "fresh corrupt record".to_string(),
            payload: b"fresh".to_vec(),
            quarantined_at_ms: unix_millis(SystemTime::now()),
        };
        let old_path = quarantine_root.join("old.json");
        let fresh_path = quarantine_root.join("fresh.json");
        let malformed_path = quarantine_root.join("malformed.json");
        tokio::fs::write(&old_path, serde_json::to_vec(&old_record).unwrap())
            .await
            .unwrap();
        tokio::fs::write(&fresh_path, serde_json::to_vec(&fresh_record).unwrap())
            .await
            .unwrap();
        tokio::fs::write(&malformed_path, b"not-json")
            .await
            .unwrap();
        store.refresh_quarantined_count().await.unwrap();

        let listed = store.list_quarantined_records().await.unwrap();
        assert_eq!(listed.len(), 3);
        assert_eq!(listed.iter().filter(|record| record.expired).count(), 1);
        assert!(listed.iter().any(|record| record.decode_error.is_some()));

        let summary = store
            .purge_expired_quarantine_at(SystemTime::now())
            .await
            .expect("purge expired quarantine");
        assert_eq!(summary.removed, 1);
        assert_eq!(summary.remaining, 2);
        assert_eq!(summary.retention, retention);
        assert!(!old_path.exists());
        assert!(fresh_path.exists());
        assert!(malformed_path.exists());
        assert_eq!(store.quarantined_records(), 2);
    }

    #[tokio::test]
    async fn concurrent_begin_allows_only_one_side_effect_owner() {
        let (_temp, store) = test_store().await;
        let mut tasks = Vec::new();
        for _ in 0..16 {
            let store = store.clone();
            tasks.push(tokio::spawn(async move {
                store
                    .begin("op-1", "create", "sandbox-1", &[0x77; 32])
                    .await
            }));
        }

        let mut started = 0;
        for task in tasks {
            match task.await.expect("join begin task").expect("begin") {
                OperationStart::Started(_) => started += 1,
                OperationStart::Existing(_) => {}
            }
        }
        assert_eq!(started, 1);
    }
}
