use std::{
    fs,
    io::{Read, Write},
    net::{TcpListener, TcpStream},
    sync::Arc,
    thread,
};
#[allow(dead_code, unexpected_cfgs)]
#[path = "../../vendor/iroh-relay-1.0.3/src/tls.rs"]
mod relay_tls;
fn main() {
    let args: Vec<String> = std::env::args().collect();
    let case = &args[1];
    let expected = case == "valid";
    let provider = Arc::new(rustls::crypto::ring::default_provider());
    let _ = rustls::crypto::ring::default_provider().install_default();
    let cert =
        rustls::pki_types::CertificateDer::from(fs::read(format!("fixtures/{case}.der")).unwrap());
    let key = rustls::pki_types::PrivateKeyDer::Pkcs8(
        fs::read("fixtures/server-key.der").unwrap().into(),
    );
    let server_config = Arc::new(
        rustls::ServerConfig::builder_with_provider(provider.clone())
            .with_safe_default_protocol_versions()
            .unwrap()
            .with_no_client_auth()
            .with_single_cert(vec![cert], key)
            .unwrap(),
    );
    // 仅在隔离 network namespace 内监听随机端口，不访问外部网络。
    for client in ["relay", "platform", "reqwest", "ureq"] {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let port = listener.local_addr().unwrap().port();
        let cfg = server_config.clone();
        let server = thread::spawn(move || {
            let (socket, _) = listener.accept().unwrap();
            socket
                .set_read_timeout(Some(std::time::Duration::from_secs(5)))
                .unwrap();
            let mut tls =
                rustls::StreamOwned::new(rustls::ServerConnection::new(cfg).unwrap(), socket);
            let mut buf = [0u8; 2048];
            if tls.read(&mut buf).is_ok() {
                let _ = tls.write_all(
                    b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nOK",
                );
                let _ = tls.flush();
            }
        });
        let url = format!("https://localhost:{port}/");
        let success = match client {
            "relay" | "platform" => {
                let config = if client == "relay" {
                    relay_tls::CaTlsConfig::default().client_config(provider.clone())
                } else {
                    rustls_platform_verifier::Verifier::new(provider.clone())
                        .map(|verifier| {
                            rustls::ClientConfig::builder_with_provider(provider.clone())
                                .with_safe_default_protocol_versions()
                                .unwrap()
                                .dangerous()
                                .with_custom_certificate_verifier(Arc::new(verifier))
                                .with_no_client_auth()
                        })
                        .map_err(std::io::Error::other)
                };
                // 空 CA 库的配置失败仍连接测试服务，以使服务线程正常退出。
                let socket = TcpStream::connect(("127.0.0.1", port)).unwrap();
                match config {
                    Ok(cfg) => {
                        let conn = rustls::ClientConnection::new(
                            Arc::new(cfg),
                            "localhost".try_into().unwrap(),
                        )
                        .unwrap();
                        let mut stream = rustls::StreamOwned::new(conn, socket);
                        stream
                            .write_all(b"GET / HTTP/1.1\r\nHost: localhost\r\n\r\n")
                            .is_ok()
                            && {
                                let mut buf = [0u8; 512];
                                stream
                                    .read(&mut buf)
                                    .is_ok_and(|n| n > 0 && buf[..n].starts_with(b"HTTP/1.1 200"))
                            }
                    }
                    Err(_) => false,
                }
            }
            "reqwest" => reqwest::blocking::Client::builder()
                .no_proxy()
                .timeout(std::time::Duration::from_secs(5))
                .build()
                .unwrap()
                .get(&url)
                .send()
                .is_ok(),
            _ => ureq::AgentBuilder::new()
                .timeout(std::time::Duration::from_secs(5))
                .build()
                .get(&url)
                .call()
                .is_ok(),
        };
        server.join().unwrap();
        assert_eq!(success, expected, "{client}: {case}");
        println!("PASS {client}: {case}");
    }
}
