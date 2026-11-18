#[cfg(test)]
mod tests {
    use std::time::SystemTime;

    use tempfile::TempDir;

    use super::*;

    fn running_metadata() -> SandboxMetadata {
        SandboxMetadata {
            id: SandboxId::new(),
            state: SandboxState::Running,
            created_at: SystemTime::now(),
            ..SandboxMetadata::default()
        }
    }

    #[tokio::test]
    async fn persists_loads_and_deletes_running_metadata() {
        let temp = TempDir::new().expect("tempdir");
        let store = RunningSandboxStore::open(temp.path().join("records.db"))
            .await
            .expect("open store");
        let metadata = running_metadata();
        store.persist(&metadata).await.expect("persist");

        let loaded = store.load_all().await.expect("load");
        assert_eq!(loaded.len(), 1);
        assert_eq!(loaded[0].id, metadata.id);
        assert_eq!(loaded[0].state, SandboxState::Running);

        store.delete(&metadata.id).await.expect("delete");
        assert!(store.load_all().await.expect("reload").is_empty());
    }

    #[tokio::test]
    async fn rejects_non_running_record_and_corrupt_payload() {
        let temp = TempDir::new().expect("tempdir");
        let store = RunningSandboxStore::open(temp.path().join("records.db"))
            .await
            .expect("open store");
        let mut metadata = running_metadata();
        metadata.state = SandboxState::Paused;
        assert!(store.persist(&metadata).await.is_err());

        metadata.state = SandboxState::Running;
        store
            .db
            .put(metadata.id.to_string(), b"not-json".to_vec())
            .await
            .expect("put corrupt record");
        assert!(store.load_all().await.is_err());
    }
}
