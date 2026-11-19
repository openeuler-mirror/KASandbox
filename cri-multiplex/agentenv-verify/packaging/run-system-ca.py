#!/usr/bin/env python3
"""用已应用补丁的 Runtime vendor 离线验证系统 CA。依赖 cargo 和 cryptography。"""
import argparse
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--runtime-source", type=Path, required=True)
    args = parser.parse_args()

    vendor = args.runtime_source.resolve() / "vendor"
    relay_tls = Path("iroh-relay-1.0.3/src/tls.rs")
    harness = Path(__file__).with_name("system-ca-test")
    with tempfile.TemporaryDirectory(prefix="agentenv-system-ca-") as directory:
        root = Path(directory)
        probe = root / "probe"
        shutil.copytree(harness, probe, ignore=shutil.ignore_patterns("target", "fixtures", "Cargo.lock"))
        source_file = root / "vendor" / relay_tls
        source_file.parent.mkdir(parents=True)
        shutil.copy2(vendor / relay_tls, source_file)
        subprocess.run([sys.executable, "generate-fixtures.py"], cwd=probe, check=True)

        env = dict(os.environ, CARGO_TARGET_DIR=str(root / "target"))
        env.setdefault("CARGO_BUILD_JOBS", "2")
        cargo_source = [
            "--config", 'source.crates-io.replace-with="vendored-sources"',
            "--config", f"source.vendored-sources.directory={json.dumps(vendor.as_posix())}",
            "--offline",
        ]
        subprocess.run(["cargo", "build", *cargo_source], cwd=probe, env=env, check=True)
        subprocess.run(["cargo", "clippy", *cargo_source, "--", "-D", "warnings"], cwd=probe, env=env, check=True)
        executable = root / "target/debug" / ("system-ca-probe.exe" if os.name == "nt" else "system-ca-probe")
        fixtures = probe / "fixtures"
        env["SSL_CERT_DIR"] = str(fixtures / "empty-dir")
        for case in ("valid", "wrong-host", "untrusted", "expired", "empty"):
            env["SSL_CERT_FILE"] = str(fixtures / ("empty.pem" if case == "empty" else "ca.pem"))
            subprocess.run([str(executable), case], cwd=probe, env=env, check=True, timeout=30)


if __name__ == "__main__":
    main()
