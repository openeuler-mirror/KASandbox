"""Filesystem, watcher, and signed URL SDK cases."""

from __future__ import annotations

import time
from urllib.parse import parse_qs, urlparse

from .e2e_models import CaseStatus
from .e2e_sdk_common import evidence, run_path, sdk_sandbox


def handle(case, context: dict[str, object], owner):
    sandbox = sdk_sandbox(owner)
    mode = str(case.parameters["mode"])
    root = run_path(context, "filesystem")
    sandbox.files.make_dir(root)

    if mode == "read-formats":
        path = f"{root}/formats.txt"
        payload = "filesystem-\u4e00\u81f4\u6027"
        sandbox.files.write(path, payload)
        streamed = b"".join(sandbox.files.read(path, format="stream"))
        passed = sandbox.files.read(path) == payload and bytes(sandbox.files.read(path, format="bytes")) == payload.encode() and streamed == payload.encode()
        return CaseStatus.PASS if passed else CaseStatus.FAIL, "text/bytes/stream compared", []

    if mode == "overwrite":
        path = f"{root}/overwrite.txt"
        sandbox.files.write(path, "old")
        sandbox.files.write(path, "replacement")
        actual = sandbox.files.read(path)
        return CaseStatus.PASS if actual == "replacement" else CaseStatus.FAIL, f"content={actual!r}", []

    if mode == "write-files":
        files = [{"path": f"{root}/batch-a.txt", "data": "A"}, {"path": f"{root}/batch-b.txt", "data": "B"}]
        written = sandbox.files.write_files(files)
        passed = len(written) == 2 and sandbox.files.read(files[0]["path"]) == "A" and sandbox.files.read(files[1]["path"]) == "B"
        return CaseStatus.PASS if passed else CaseStatus.FAIL, f"written={len(written)}", [evidence(written)]

    if mode == "empty-write-files":
        written = sandbox.files.write_files([])
        return CaseStatus.PASS if written == [] else CaseStatus.FAIL, f"result={written!r}", []

    if mode == "directory-list":
        nested = f"{root}/a/b"
        sandbox.files.make_dir(nested)
        target = f"{nested}/entry.txt"
        sandbox.files.write(target, "entry")
        entries = sandbox.files.list(root, depth=2)
        visible_paths = {item.path.rstrip("/") for item in entries}
        passed = nested in visible_paths and target not in visible_paths
        return CaseStatus.PASS if passed else CaseStatus.FAIL, f"entries={len(entries)}, nested_visible={nested in visible_paths}", [evidence(entries)]

    if mode == "info":
        path = f"{root}/info.txt"
        sandbox.files.write(path, "info")
        exists = sandbox.files.exists(path)
        info = sandbox.files.get_info(path)
        passed = exists and info.path == path and info.size == 4
        return CaseStatus.PASS if passed else CaseStatus.FAIL, f"exists={exists}, path={info.path}, size={info.size}", [evidence(info)]

    if mode == "rename-remove":
        original = f"{root}/before.txt"
        renamed = f"{root}/after.txt"
        sandbox.files.write(original, "rename")
        sandbox.files.rename(original, renamed)
        renamed_ok = not sandbox.files.exists(original) and sandbox.files.exists(renamed)
        sandbox.files.remove(renamed)
        passed = renamed_ok and not sandbox.files.exists(renamed)
        return CaseStatus.PASS if passed else CaseStatus.FAIL, f"rename_ok={renamed_ok}", []

    if mode == "invalid-depth":
        from e2b import InvalidArgumentException

        try:
            sandbox.files.list(root, depth=0)
        except InvalidArgumentException:
            return CaseStatus.PASS, "depth=0 rejected by SDK", []
        return CaseStatus.FAIL, "depth=0 unexpectedly accepted", []

    if mode in {"watch", "watch-recursive"}:
        recursive = mode == "watch-recursive"
        watch_root = f"{root}/watch-{'recursive' if recursive else 'flat'}"
        sandbox.files.make_dir(watch_root)
        watcher = sandbox.files.watch_dir(watch_root, recursive=recursive)
        label = f"{mode}-{case.case_id}"
        owner.ledger.record_watcher(sandbox.sandbox_id, label, case.case_id)
        watcher_map = context.setdefault("watchers", {})
        if isinstance(watcher_map, dict):
            watcher_map[label] = watcher
        created = f"{watch_root}/child/event.txt" if recursive else f"{watch_root}/event.txt"
        if recursive:
            sandbox.files.make_dir(f"{watch_root}/child")
        sandbox.files.write(created, "watch")
        events = []
        for _ in range(5):
            events.extend(watcher.get_new_events())
            if any(event.name.endswith("event.txt") for event in events):
                break
            time.sleep(0.4)
        watcher.stop()
        if isinstance(watcher_map, dict):
            watcher_map.pop(label, None)
        passed = any(event.name.endswith("event.txt") for event in events)
        return CaseStatus.PASS if passed else CaseStatus.FAIL, f"events={len(events)}", [evidence(events)]

    if mode == "signed-urls":
        path = f"{root}/signed.txt"
        sandbox.files.write(path, "url")
        download = sandbox.download_url(path, user="user", use_signature_expiration=120)
        upload = sandbox.upload_url(path, user="user", use_signature_expiration=120)
        parsed_urls = [urlparse(download), urlparse(upload)]
        queries = [parse_qs(parsed.query) for parsed in parsed_urls]
        required = {"path", "username", "signature", "signature_expiration"}
        passed = all(
            parsed.scheme in {"http", "https"}
            and query.get("path") == [path]
            and query.get("username") == ["user"]
            and required.issubset(query)
            for parsed, query in zip(parsed_urls, queries)
        )
        return CaseStatus.PASS if passed else CaseStatus.FAIL, "upload/download URLs generated", [evidence({"download": download, "upload": upload})]

    raise ValueError(f"unknown extended filesystem mode: {mode}")
