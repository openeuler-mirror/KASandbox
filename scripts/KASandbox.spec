%define debug_package %{nil}
%global pkg_version 1.0.0

# rpmbuild --with mooncake 时 %build 走 ./build.sh -m（CGo 链接 Mooncake 库，
# 需构建机已安装 /usr/lib64 下的 mooncake/urma 等依赖）；默认关闭
%bcond_with mooncake

Name:           KASandbox
Version:        %{pkg_version}
Release:        1
Summary:        KASandbox sandbox orchestration and deployment tools
License:        Apache-2.0
URL:            https://gitcode.com/src-openeuler/KASandbox
Source0:        %{name}-%{version}.tar.gz

BuildRequires:  gcc
BuildRequires:  golang
BuildRequires:  make

%description
KASandbox provides the sandbox orchestration services, CRI multiplexing
shim, Firecracker integration, and deployment tools used to run E2B
workloads on Nomad or Kubernetes.

%prep
%autosetup -n %{name}-%{version}

%build
# Build the Go services using the project's reproducible build entry point.
export GOPROXY=https://mirrors.huaweicloud.com/repository/goproxy/,direct
# export GOSUMDB=sum.huaweicloud.com
export GONOSUMDB=*

%if %{with mooncake}
./build.sh -m
%else
./build.sh
%endif
./build.sh -c


%install
install -d %{buildroot}/opt/e2b-infra/bin
install -d %{buildroot}/opt/e2b-infra/dep
install -d %{buildroot}/opt/e2b-infra/helm
install -d %{buildroot}/opt/e2b-infra/nomad

# The deployment scripts use /opt/e2b-infra as their runtime directory.
install -D -m 0755 deploy/build.sh %{buildroot}/opt/e2b-infra/build.sh
install -D -m 0755 deploy/k8s-deploy.sh %{buildroot}/opt/e2b-infra/k8s-deploy.sh
install -D -m 0755 deploy/deploy-worker.sh %{buildroot}/opt/e2b-infra/deploy-worker.sh
cp -a deploy/dep/. %{buildroot}/opt/e2b-infra/dep/
# Several entry points invoke these files as siblings from the runtime root;
# retain the dep directory as well for download payloads referenced there.
cp -a deploy/dep/. %{buildroot}/opt/e2b-infra/
chmod -R a+rX %{buildroot}/opt/e2b-infra/dep

# Copy all deploy directory contents to /opt/e2b-infra/bin
cp -a deploy/. %{buildroot}/opt/e2b-infra/
chmod -R a+rX %{buildroot}/opt/e2b-infra/bin

# Install architecture-specific service binaries into the flat layout used
# by the Nomad and Kubernetes deployment scripts.
case "%{_target_cpu}" in
    aarch64) binary_arch=arm64 ;;
    x86_64)  binary_arch=amd64 ;;
    *)        echo "Unsupported architecture: %{_target_cpu}" >&2; exit 1 ;;
esac
for binary in api client-proxy envd orchestrator e2b-webhook \
              migrator seed-db fc-netns-exec cri-multiplex; do
    binary_path="bin/${binary_arch}/${binary}"
    [ -x "$binary_path" ] || {
        echo "Missing built binary: $binary_path" >&2
        exit 1
    }
    install -D -m 0755 "$binary_path" \
        "%{buildroot}/opt/e2b-infra/bin/${binary}"
done
# Install only the Dockerfiles required by the deployment.
install -D -m 0644 packages/api/Dockerfile \
    %{buildroot}/opt/e2b-infra/bin/api.Dockerfile
install -D -m 0644 packages/orchestrator/Dockerfile \
    %{buildroot}/opt/e2b-infra/bin/orchestrator.Dockerfile
install -D -m 0644 packages/client-proxy/Dockerfile \
    %{buildroot}/opt/e2b-infra/bin/client-proxy.Dockerfile
install -D -m 0644 packages/db/Dockerfile \
    %{buildroot}/opt/e2b-infra/bin/db-migrator.Dockerfile
install -D -m 0644 e2b-webhook/webhook.Dockerfile \
    %{buildroot}/opt/e2b-infra/bin/e2b-webhook.Dockerfile

# Install only the Helm templates required by the K8s deployment.
# rm -rf %{buildroot}/opt/e2b-infra/helm
install -D -m 0644 helm/Chart.yaml \
    %{buildroot}/opt/e2b-infra/helm/Chart.yaml
install -D -m 0644 helm/values-template.yaml \
    %{buildroot}/opt/e2b-infra/helm/values-template.yaml
install -d %{buildroot}/opt/e2b-infra/helm/templates
for template in api edge postgres redis template-manager rbac-orchestrator e2b-webhook; do
    install -D -m 0644 "helm/templates/${template}.yaml" \
        %{buildroot}/opt/e2b-infra/helm/templates/${template}.yaml
done
chmod -R a+rX %{buildroot}/opt/e2b-infra/helm

# Install only the Nomad jobs required by the deployment.
# rm -rf %{buildroot}/opt/e2b-infra/nomad
install -d %{buildroot}/opt/e2b-infra/nomad
for job in api edge redis template-manager; do
    install -D -m 0644 "deploy/nomad/${job}.hcl" \
        %{buildroot}/opt/e2b-infra/nomad/${job}.hcl
done

# Keep both migration sets available to the runtime scripts.
if [ -d packages/db/migrations ]; then
    install -d %{buildroot}/opt/e2b-infra/bin/migrations
    cp -a packages/db/migrations/. %{buildroot}/opt/e2b-infra/bin/migrations/
fi
if [ -d packages/clickhouse/migrations ]; then
    install -d %{buildroot}/opt/e2b-infra/bin/migrations-clickhouse
    cp -a packages/clickhouse/migrations/. %{buildroot}/opt/e2b-infra/bin/migrations-clickhouse/
fi

%files
/opt/e2b-infra

%changelog
* Mon Jul 27 2026 fly_1997 <flylove7@outlook.com> - 1.0.0-1
- feat: init code
