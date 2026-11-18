// agentenv-verify: insert-tests
    #[test]
    fn p1_failed_tap_creation_cannot_arm_external_cleanup() {
        let mut resources = ExternalNetworkResources::default();
        assert!(
            resources
                .create_tap(|| bail!("tap0 already exists"))
                .is_err()
        );
        assert!(resources.is_empty());
        resources.create_tap(|| Ok(())).unwrap();
        assert!(!resources.is_empty());
    }



    #[test]
    fn p1_external_cleanup_without_owned_resources_does_not_enter_namespace() {
        let config = CNIExternalNetworkConfig {
            netns_path: PathBuf::from("/does-not-exist/foreign-netns"),
            if_name: "eth0".to_string(),
            pod_ip: Ipv4Addr::new(10, 0, 0, 2),
            gateway: None,
            dns_servers: Vec::new(),
            mtu: None,
        };
        let mut slot = Slot::external(&config, EgressProxy::new()).unwrap();
        slot.cleanup_armed = true;
        slot.cleanup(true).unwrap();
        assert!(!slot.cleanup_armed);
    }


    #[test]
    fn external_slot_uses_cni_netns_and_canonical_vm_link() {
        let netns_path = std::env::temp_dir().join("agentenv-external-test-netns");
        let config = CNIExternalNetworkConfig {
            netns_path: netns_path.clone(),
            if_name: "eth0".to_string(),
            pod_ip: Ipv4Addr::new(10, 244, 0, 5),
            gateway: Some(Ipv4Addr::new(10, 244, 0, 1)),
            dns_servers: vec![Ipv4Addr::new(10, 96, 0, 10)],
            mtu: Some(1500),
        };
        let slot = Slot::external(&config, EgressProxy::new()).expect("external slot");

        let network = slot.live_network_info();
        assert!(network.external_cni);
        assert_eq!(network.pod_ip, config.pod_ip);
        assert_eq!(network.netns_path, netns_path);
        assert!(slot.is_external());
        assert_eq!(slot.host_interaction_ip, Ipv4Addr::new(10, 244, 0, 5));
        assert_eq!(slot.namespace_path(), netns_path);
        assert_eq!(slot.guest_dns_server(), Ipv4Addr::new(10, 96, 0, 10));
        assert_eq!(
            slot.build_ip_boot_arg(),
            "ip=169.254.0.21::169.254.0.22:255.255.255.252:instance:eth0:off:10.96.0.10"
        );
    }
