#[cfg(test)]
mod tests {
    use super::*;
    use axum::body::Body;
    use http::Request as HttpRequest;
    use serde_json::json;
    use tempfile::TempDir;
    use tower::ServiceExt;

    const TOKEN: &str = "0123456789abcdef0123456789abcdef";
    const TEAM_ID: &str = "11111111-1111-1111-1111-111111111111";

    include!("internal_regression_tests.rs");

    async fn test_router() -> (Router, TempDir) {
        let temp = TempDir::new().expect("tempdir");
        let store = InternalMetadataStore::open_with_durability(
            temp.path().join("catalog"),
            LocalStoreDurability::Memory,
        )
        .await
        .expect("store");
        let state = InternalApiState::new(
            Arc::new(store),
            ApiKey::new(TOKEN).expect("valid test token"),
        );
        (router(state), temp)
    }

    fn metadata_body(operation_id: &str) -> Value {
        json!({
            "operation_id": operation_id,
            "team_id": TEAM_ID,
            "base_template_id": "base-template",
            "source_pod_uid": "pod-uid",
            "source_node_name": "node-a",
            "sandbox_started_at": "2026-11-10T00:00:00Z",
            "metadata": {"app": "demo"},
            "vcpu": 2,
            "ram_mb": 1024,
            "total_disk_size_mb": 4096,
            "kernel_version": "vmlinux-test",
            "firecracker_version": "fc-test",
            "envd_version": "envd-test",
            "secure": true,
            "auto_pause": true,
            "network": {"ingress": {"allowPublicAccess": true}},
            "source_machine_info": {"cpu_architecture": "aarch64"}
        })
    }

    async fn request(
        app: &Router,
        method: &str,
        uri: &str,
        token: Option<&str>,
        body: Option<Value>,
    ) -> (StatusCode, Value) {
        let mut builder = HttpRequest::builder().method(method).uri(uri);
        if let Some(token) = token {
            builder = builder.header(INTERNAL_ADMIN_TOKEN_HEADER, token);
        }
        let body = match body {
            Some(body) => {
                builder = builder.header("content-type", "application/json");
                Body::from(serde_json::to_vec(&body).expect("json"))
            }
            None => Body::empty(),
        };
        let response = app
            .clone()
            .oneshot(builder.body(body).expect("request"))
            .await
            .expect("response");
        let status = response.status();
        let bytes = axum::body::to_bytes(response.into_body(), 1024 * 1024)
            .await
            .expect("body");
        let value = if bytes.is_empty() {
            Value::Null
        } else {
            serde_json::from_slice(&bytes).expect("json response")
        };
        (status, value)
    }

    #[tokio::test]
    async fn internal_router_requires_admin_token() {
        let (app, _temp) = test_router().await;
        let (status, body) = request(
            &app,
            "GET",
            "/internal/sandboxes/sb-1/snapshot-state",
            None,
            None,
        )
        .await;
        assert_eq!(status, StatusCode::UNAUTHORIZED);
        assert_eq!(body["code"], 401);
    }

    #[tokio::test]
    async fn internal_nine_endpoint_flow_handles_state_and_deletes() {
        let (app, _temp) = test_router().await;

        let (status, pause) = request(
            &app,
            "POST",
            "/internal/sandboxes/sb-1/pause-snapshot",
            Some(TOKEN),
            Some(metadata_body("op-pause")),
        )
        .await;
        assert_eq!(status, StatusCode::OK, "{pause}");
        let pause_template = pause["template_id"]
            .as_str()
            .expect("pause template")
            .to_string();
        let pause_build = pause["build_id"].as_str().expect("pause build").to_string();

        let (status, state) = request(
            &app,
            "GET",
            "/internal/sandboxes/sb-1/snapshot-state",
            Some(TOKEN),
            None,
        )
        .await;
        assert_eq!(status, StatusCode::OK);
        assert_eq!(state["paused"], true);
        assert_eq!(state["build_id"], pause_build);

        let (status, _) = request(
            &app,
            "POST",
            &format!("/internal/builds/{pause_build}/status"),
            Some(TOKEN),
            Some(json!({"operation_id": "op-pause", "status": "success"})),
        )
        .await;
        assert_eq!(status, StatusCode::OK);

        let (status, last) = request(
            &app,
            "GET",
            "/internal/sandboxes/sb-1/last-snapshot",
            Some(TOKEN),
            None,
        )
        .await;
        assert_eq!(status, StatusCode::OK, "{last}");
        assert_eq!(last["template_id"], pause_template);
        assert_eq!(last["origin_node"], "node-a");

        let (status, checkpoint) = request(
            &app,
            "POST",
            "/internal/sandboxes/sb-1/snapshot-templates",
            Some(TOKEN),
            Some(metadata_body("op-checkpoint")),
        )
        .await;
        assert_eq!(status, StatusCode::OK, "{checkpoint}");
        let snapshot_id = checkpoint["snapshot_id"]
            .as_str()
            .expect("snapshot id")
            .to_string();
        let checkpoint_build = checkpoint["build_id"]
            .as_str()
            .expect("checkpoint build")
            .to_string();

        let (status, _) = request(
            &app,
            "POST",
            &format!("/internal/builds/{checkpoint_build}/status"),
            Some(TOKEN),
            Some(json!({"operation_id": "op-checkpoint", "status": "success"})),
        )
        .await;
        assert_eq!(status, StatusCode::OK);

        let (status, resolved) = request(
            &app,
            "GET",
            &format!("/internal/snapshots/{snapshot_id}/resolve?team_id={TEAM_ID}"),
            Some(TOKEN),
            None,
        )
        .await;
        assert_eq!(status, StatusCode::OK, "{resolved}");
        assert_eq!(resolved["build_id"], checkpoint_build);

        let (status, _) = request(
            &app,
            "DELETE",
            "/internal/sandboxes/sb-1/snapshot",
            Some(TOKEN),
            None,
        )
        .await;
        assert_eq!(status, StatusCode::NO_CONTENT);

        let (status, state) = request(
            &app,
            "GET",
            "/internal/sandboxes/sb-1/snapshot-state",
            Some(TOKEN),
            None,
        )
        .await;
        assert_eq!(status, StatusCode::OK);
        assert_eq!(state["paused"], false);

        let (status, _) = request(
            &app,
            "DELETE",
            &format!("/internal/snapshot-templates/{snapshot_id}?team_id={TEAM_ID}"),
            Some(TOKEN),
            None,
        )
        .await;
        assert_eq!(status, StatusCode::NO_CONTENT);

        let (status, _) = request(
            &app,
            "GET",
            &format!("/internal/snapshots/{snapshot_id}/resolve?team_id={TEAM_ID}"),
            Some(TOKEN),
            None,
        )
        .await;
        assert_eq!(status, StatusCode::NOT_FOUND);

        let (status, _) = request(
            &app,
            "DELETE",
            &format!("/internal/templates/base-template?team_id={TEAM_ID}"),
            Some(TOKEN),
            None,
        )
        .await;
        assert_eq!(status, StatusCode::NO_CONTENT);
    }

    #[tokio::test]
    async fn build_status_is_idempotent_and_conflict_safe() {
        let (app, _temp) = test_router().await;
        let (_, pause) = request(
            &app,
            "POST",
            "/internal/sandboxes/sb-1/pause-snapshot",
            Some(TOKEN),
            Some(metadata_body("op-pause")),
        )
        .await;
        let build_id = pause["build_id"].as_str().expect("build").to_string();
        let uri = format!("/internal/builds/{build_id}/status");

        let (status, _) = request(
            &app,
            "POST",
            &uri,
            Some(TOKEN),
            Some(json!({"operation_id": "op-pause", "status": "success"})),
        )
        .await;
        assert_eq!(status, StatusCode::OK);
        let (status, _) = request(
            &app,
            "POST",
            &uri,
            Some(TOKEN),
            Some(json!({"operation_id": "op-pause", "status": "success"})),
        )
        .await;
        assert_eq!(status, StatusCode::OK);
        let (status, _) = request(
            &app,
            "POST",
            &uri,
            Some(TOKEN),
            Some(json!({"operation_id": "op-pause", "status": "failed"})),
        )
        .await;
        assert_eq!(status, StatusCode::CONFLICT);
    }

    #[tokio::test]
    async fn team_mismatch_returns_422_and_live_template_delete_returns_409() {
        let (app, _temp) = test_router().await;
        let (_, checkpoint) = request(
            &app,
            "POST",
            "/internal/sandboxes/sb-1/snapshot-templates",
            Some(TOKEN),
            Some(metadata_body("op-checkpoint")),
        )
        .await;
        let snapshot_id = checkpoint["snapshot_id"]
            .as_str()
            .expect("snapshot")
            .to_string();
        let build_id = checkpoint["build_id"].as_str().expect("build").to_string();

        let (status, _) = request(
            &app,
            "GET",
            &format!(
                "/internal/snapshots/{snapshot_id}/resolve?team_id=22222222-2222-2222-2222-222222222222"
            ),
            Some(TOKEN),
            None,
        )
        .await;
        assert_eq!(status, StatusCode::UNPROCESSABLE_ENTITY);

        let (status, _) = request(
            &app,
            "DELETE",
            &format!("/internal/snapshot-templates/{snapshot_id}?team_id={TEAM_ID}"),
            Some(TOKEN),
            None,
        )
        .await;
        assert_eq!(status, StatusCode::CONFLICT);

        let (status, _) = request(
            &app,
            "POST",
            &format!("/internal/builds/{build_id}/status"),
            Some(TOKEN),
            Some(json!({"operation_id": "op-checkpoint", "status": "success"})),
        )
        .await;
        assert_eq!(status, StatusCode::OK);

        let (status, _) = request(
            &app,
            "DELETE",
            &format!("/internal/templates/base-template?team_id={TEAM_ID}"),
            Some(TOKEN),
            None,
        )
        .await;
        assert_eq!(status, StatusCode::CONFLICT);
    }

    #[tokio::test]
    async fn allocation_is_idempotent_and_rejects_operation_fingerprint_conflicts() {
        let (app, _temp) = test_router().await;
        let body = metadata_body("op-idempotent");
        let (status, first) = request(
            &app,
            "POST",
            "/internal/sandboxes/sb-1/pause-snapshot",
            Some(TOKEN),
            Some(body.clone()),
        )
        .await;
        assert_eq!(status, StatusCode::OK, "{first}");
        let (status, replay) = request(
            &app,
            "POST",
            "/internal/sandboxes/sb-1/pause-snapshot",
            Some(TOKEN),
            Some(body.clone()),
        )
        .await;
        assert_eq!(status, StatusCode::OK, "{replay}");
        assert_eq!(first["template_id"], replay["template_id"]);
        assert_eq!(first["build_id"], replay["build_id"]);

        let mut changed = body;
        changed["source_node_name"] = json!("node-b");
        let (status, _) = request(
            &app,
            "POST",
            "/internal/sandboxes/sb-1/pause-snapshot",
            Some(TOKEN),
            Some(changed),
        )
        .await;
        assert_eq!(status, StatusCode::CONFLICT);
    }
}
