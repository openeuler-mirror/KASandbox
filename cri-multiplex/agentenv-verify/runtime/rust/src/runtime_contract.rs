#[cfg(test)]
mod tests {
    use prost::Message;

    #[test]
    fn runtime_descriptor_exposes_lifecycle_and_admin_services() {
        let descriptor = prost_types::FileDescriptorSet::decode(
            include_bytes!(concat!(env!("OUT_DIR"), "/agentenv_runtime_descriptor.bin")).as_slice(),
        )
        .expect("runtime descriptor must be valid protobuf");

        let services = descriptor
            .file
            .iter()
            .flat_map(|file| file.service.iter())
            .filter_map(|service| service.name.as_deref())
            .collect::<Vec<_>>();
        assert!(services.contains(&"SandboxLifecycleService"));
        assert!(services.contains(&"SandboxAdminService"));
        assert!(!services.contains(&"SandboxExecService"));
        let admin = descriptor
            .file
            .iter()
            .flat_map(|file| file.service.iter())
            .find(|service| service.name.as_deref() == Some("SandboxAdminService"))
            .expect("admin service descriptor");
        assert!(
            !admin
                .method
                .iter()
                .any(|method| method.name.as_deref() == Some("GetStats"))
        );
    }
}
