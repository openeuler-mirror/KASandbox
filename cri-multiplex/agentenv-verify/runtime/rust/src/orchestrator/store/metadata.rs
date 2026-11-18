// agentenv-verify: insert-tests
    #[test]
    fn identity_associations_are_optional_and_backward_compatible() {
        let metadata = SandboxMetadata {
            product_sandbox_id: Some("product-1".to_string()),
            alias: Some("demo".to_string()),
            ..Default::default()
        };
        let mut serialized = serde_json::to_value(&metadata).unwrap();
        assert_eq!(serialized["product_sandbox_id"].as_str(), Some("product-1"));
        assert_eq!(serialized["alias"].as_str(), Some("demo"));

        let object = serialized.as_object_mut().unwrap();
        object.remove("product_sandbox_id");
        object.remove("alias");
        let restored: SandboxMetadata = serde_json::from_value(serialized).unwrap();
        assert_eq!(restored.product_sandbox_id, None);
        assert_eq!(restored.alias, None);
    }
