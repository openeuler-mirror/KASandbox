#[cfg(test)]
mod tests {
    use super::prepare_runtime;

    #[test]
    fn prepare_runtime_preserves_existing_netns_for_recovery_audit() {
        let temp = tempfile::tempdir().expect("create temp dir");
        let netns_dir = temp.path().join("netns");
        std::fs::create_dir_all(&netns_dir).expect("create netns dir");
        let netns = netns_dir.join("agentenv-ns-test");
        let manifest = netns_dir.join("agentenv-ns-test.ownership.json");
        std::fs::write(&netns, b"").expect("write netns placeholder");
        std::fs::write(&manifest, b"{}").expect("write ownership manifest");

        prepare_runtime(temp.path()).expect("prepare runtime");

        assert!(netns.exists(), "startup preparation must not delete netns");
        assert!(
            manifest.exists(),
            "startup preparation must not delete manifest"
        );
    }
}
