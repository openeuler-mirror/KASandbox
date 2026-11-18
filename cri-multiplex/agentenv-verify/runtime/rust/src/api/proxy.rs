// agentenv-verify: insert-tests
    #[tokio::test]
    async fn internal_path_does_not_bypass_private_proxy_auth() {
        async fn internal_status(app: &axum::Router, headers: &[(&str, &str)]) -> StatusCode {
            // 不复用会追加 localhost Host 的 helper，确保 Host 路由只有一个头。
            let mut request = Request::builder().uri("/internal/private");
            for (name, value) in headers {
                request = request.header(*name, *value);
            }
            app.clone()
                .oneshot(request.body(Body::empty()).unwrap())
                .await
                .unwrap()
                .status()
        }
        let upstream_addr = start_upstream_server().await;
        let api = build_api_with_sandbox_proxy_domains(vec!["sandbox.test".to_string()]).await;
        let sandbox_id = SandboxId::new();
        api.orchestrator()
            .set_proxy_target_for_test(
                sandbox_id,
                ProxyTarget::new(Ipv4Addr::LOCALHOST),
                crate::orchestrator::SandboxState::Running,
            )
            .await;
        api.orchestrator()
            .set_allow_public_traffic_for_test(&sandbox_id, false)
            .await
            .unwrap();
        let app = server::new(Arc::clone(&api));
        let sandbox_id_text = sandbox_id.to_string();
        let port = upstream_addr.port().to_string();
        let host = format!("{port}-{sandbox_id_text}.sandbox.test");
        let token = api.traffic_access_token(sandbox_id);
        for mut headers in [
            vec![
                (SANDBOX_ID_HEADER, sandbox_id_text.as_str()),
                (TARGET_PORT_HEADER, port.as_str()),
            ],
            vec![(header::HOST.as_str(), host.as_str())],
        ] {
            // guest 内部路径不能借控制面的免 API-key 分支绕过 traffic token。
            assert_eq!(
                internal_status(&app, &headers).await,
                StatusCode::UNAUTHORIZED
            );
            headers.push((TRAFFIC_ACCESS_TOKEN_HEADER, token.as_str()));
            assert_eq!(internal_status(&app, &headers).await, StatusCode::OK);
        }
    }
