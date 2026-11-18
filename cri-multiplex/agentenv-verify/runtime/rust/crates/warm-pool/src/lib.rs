// agentenv-verify: insert-tests
    #[test]
    fn for_each_idle_observes_all_resources_without_draining() {
        let pool = WarmPool::<u32>::new(PoolConfig {
            low_watermark: 0,
            high_watermark: 10,
            maintenance_enabled: false,
            startup_prewarm: false,
        });
        pool.release(11).unwrap();
        pool.release(22).unwrap();
        let mut seen = Vec::new();
        pool.for_each_idle(|value| seen.push(*value));
        assert_eq!(seen, vec![11, 22]);
        assert_eq!(pool.len(), 2);
    }
