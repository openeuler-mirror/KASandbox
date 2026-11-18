#!/usr/bin/env python3
"""九个内部接口的 HTTP 回归；只操作本次唯一合成元数据，不操作 VM。"""
import concurrent.futures
import copy
import http.client
import json
import os
import threading
import time
import unittest
import uuid
from pathlib import Path
from urllib.parse import urlsplit

BASE = os.environ.get('AENV_API_URL', 'http://127.0.0.1:8000')
TOKEN = os.environ.get('AENV_INTERNAL_ADMIN_TOKEN', '')
if not TOKEN:
    import tomllib
    TOKEN = tomllib.loads(Path(os.environ.get('AENV_CREDENTIALS', '/root/.config/aenv/credentials')).read_text())['api_key']
PREFIX = 'api-regression-' + uuid.uuid4().hex
TEAM = str(uuid.uuid4())
OTHER = str(uuid.uuid4())
RECORDS = []
ALLOCATIONS = {}
LOCK = threading.Lock()
REQUIRED = ['operation_id', 'team_id', 'base_template_id', 'source_pod_uid', 'source_node_name',
            'sandbox_started_at', 'vcpu', 'ram_mb', 'total_disk_size_mb', 'kernel_version',
            'firecracker_version', 'envd_version', 'secure']


def body():
    return dict(operation_id=PREFIX + '-' + uuid.uuid4().hex, team_id=TEAM,
                base_template_id=PREFIX + '-base', source_pod_uid='test-pod', source_node_name='test-node',
                sandbox_started_at='2026-11-13T00:00:00Z', vcpu=1, ram_mb=128,
                total_disk_size_mb=0, kernel_version='test', firecracker_version='test',
                envd_version='test', secure=False)


def sb():
    return '/internal/sandboxes/' + PREFIX + '-' + uuid.uuid4().hex


def call(method, path, data=None, expected=200, token=TOKEN, raw=None, content_type='application/json', duplicate=False):
    path = path.replace('/unknown', '/' + PREFIX + '-missing')
    url = urlsplit(BASE)
    cls = http.client.HTTPSConnection if url.scheme == 'https' else http.client.HTTPConnection
    conn = cls(url.hostname, url.port, timeout=20)
    payload = raw if raw is not None else (json.dumps(data).encode() if data is not None else b'')
    try:
        conn.putrequest(method, url.path.rstrip('/') + path)
        if token is not None:
            conn.putheader('X-Admin-Token', token)
            if duplicate:
                conn.putheader('X-Admin-Token', token)
        if content_type is not None:
            conn.putheader('Content-Type', content_type)
        conn.putheader('Content-Length', str(len(payload)))
        start = time.monotonic()
        conn.endheaders(payload)
        response = conn.getresponse()
        raw_response = response.read()
        try:
            result = json.loads(raw_response) if raw_response else None
        except ValueError:
            result = None
        with LOCK:
            RECORDS.append(dict(method=method, path=path, expected=expected, status=response.status,
                                duration_ms=round((time.monotonic() - start) * 1000, 2), response=result))
            # 失败的断言也登记已分配记录，保证清理不漏掉意外成功的请求。
            if method == 'POST' and isinstance(result, dict) and 'build_id' in result and data:
                ALLOCATIONS[result['build_id']] = (path.rsplit('/', 1)[0], copy.deepcopy(data), result)
        allowed = expected if isinstance(expected, tuple) else (expected,)
        assert response.status in allowed, f'{method} {path}: expected={expected}, actual={response.status}'
        if response.status >= 400:
            assert isinstance(result, dict) and result.get('code') == response.status, '错误响应必须符合 Error JSON schema'
        if response.status == 204:
            assert not raw_response, '204 不应包含响应体'
        return result
    finally:
        conn.close()


class InternalAPIRegression(unittest.TestCase):
    def test_auth_all_nine_routes(self):
        routes = [('POST', sb() + '/pause-snapshot'), ('POST', sb() + '/snapshot-templates'),
                  ('GET', sb() + '/last-snapshot'), ('POST', '/internal/builds/unknown/status'),
                  ('GET', sb() + '/snapshot-state'), ('DELETE', sb() + '/snapshot'),
                  ('DELETE', '/internal/snapshot-templates/unknown?team_id=' + TEAM),
                  ('GET', '/internal/snapshots/unknown/resolve?team_id=' + TEAM),
                  ('DELETE', '/internal/templates/unknown?team_id=' + TEAM)]
        for method, path in routes:
            for token in (None, '', 'invalid-regression-token'):
                with self.subTest(method=method, path=path, token_kind='missing' if token is None else 'invalid'):
                    call(method, path, expected=401, token=token)
            with self.subTest(method=method, duplicate=True):
                call(method, path, expected=401, duplicate=True)

    def test_all_required_fields_and_types(self):
        for endpoint in ('pause-snapshot', 'snapshot-templates'):
            for field in REQUIRED:
                for variant in ('missing', 'null', 'wrong-type'):
                    with self.subTest(endpoint=endpoint, field=field, variant=variant):
                        b = body()
                        if variant == 'missing':
                            del b[field]
                        else:
                            b[field] = None if variant == 'null' else []
                        call('POST', sb() + '/' + endpoint, b, expected=400)

    def test_json_rejections(self):
        for suffix in ('pause-snapshot', 'snapshot-templates'):
            for raw, ct in ((b'{', 'application/json'), (b'', 'application/json'),
                            (b'[]', 'application/json'), (b'{}', 'text/plain')):
                with self.subTest(suffix=suffix, raw=raw, content_type=ct):
                    call('POST', sb() + '/' + suffix, expected=400, raw=raw, content_type=ct)

    def test_metadata_bounds(self):
        invalid = [('operation_id', ''), ('operation_id', 'x' * 129), ('team_id', 'not-a-uuid'),
                   ('source_node_name', ' '), ('base_template_id', 'x' * 513),
                   ('source_pod_uid', ''), ('sandbox_started_at', 'bad-time'),
                   ('vcpu', 0), ('vcpu', -1), ('ram_mb', 0), ('ram_mb', -1),
                   ('total_disk_size_mb', -1), ('vcpu', 2**63),
                   ('metadata', {str(i): 'v' for i in range(129)}),
                   ('metadata', {'x' * 257: 'v'}), ('metadata', {'k': 'x' * 16385}),
                   ('metadata', {'k': 1}), ('allow_internet_access', 'true'), ('auto_pause', 1)]
        for endpoint in ('pause-snapshot', 'snapshot-templates'):
            for field, value in invalid:
                with self.subTest(endpoint=endpoint, field=field, value_type=type(value).__name__):
                    b = body(); b[field] = value
                    call('POST', sb() + '/' + endpoint, b, expected=400)
            # 边界值应成功，包括 secure=false 和零磁盘大小。
            b = body()
            b.update(operation_id='x' * 96 + uuid.uuid4().hex, base_template_id=PREFIX + 'x' * (512 - len(PREFIX)),
                     metadata={str(i): 'v' for i in range(127)} | {'k' * 256: 'v' * 16384})
            call('POST', sb() + '/' + endpoint, b)

    def test_optional_object_shapes(self):
        for endpoint in ('pause-snapshot', 'snapshot-templates'):
            for field in ('network', 'auto_resume', 'source_machine_info', 'volume_mounts'):
                for value in (1, 'bad', True, [1]):
                    with self.subTest(endpoint=endpoint, field=field, value=value):
                        b = body(); b[field] = value
                        call('POST', sb() + '/' + endpoint, b, expected=400)

    def test_query_validation(self):
        for method, path in [('DELETE', '/internal/snapshot-templates/unknown'),
                             ('GET', '/internal/snapshots/unknown/resolve'),
                             ('DELETE', '/internal/templates/unknown')]:
            for query in ('', '?team_id=', '?team_id=bad', '?team_id=' + TEAM + '&team_id=' + OTHER):
                with self.subTest(path=path, query=query):
                    call(method, path + query, expected=400)

    def test_missing_resources_and_invalid_paths(self):
        call('GET', sb() + '/last-snapshot', expected=404)
        self.assertEqual(call('GET', sb() + '/snapshot-state'), {'paused': False})
        call('DELETE', sb() + '/snapshot', expected=404)
        call('POST', '/internal/builds/unknown/status', {'operation_id': 'x', 'status': 'success'}, 404)
        for method, tail in [('DELETE', '/snapshot-templates/unknown'), ('GET', '/snapshots/unknown/resolve'),
                             ('DELETE', '/templates/unknown')]:
            call(method, '/internal' + tail + '?team_id=' + TEAM, expected=404)
        for suffix in ('last-snapshot', 'snapshot-state', 'snapshot'):
            method = 'DELETE' if suffix == 'snapshot' else 'GET'
            for identifier in ('%20', 'x' * 513):
                with self.subTest(suffix=suffix, identifier_len=len(identifier)):
                    call(method, '/internal/sandboxes/' + identifier + '/' + suffix, expected=400)

    def test_lifecycle_and_isolation(self):
        path = sb(); b = body()
        pause = call('POST', path + '/pause-snapshot', b)
        self.assertEqual(call('POST', path + '/pause-snapshot', b), pause)
        call('POST', path + '/pause-snapshot', dict(b, ram_mb=256), 409)
        call('POST', sb() + '/pause-snapshot', b, 409)
        call('POST', path + '/snapshot-templates', b, 409)
        call('GET', path + '/last-snapshot', expected=404)
        state = call('GET', path + '/snapshot-state')
        self.assertTrue(state['paused']); self.assertEqual(state['build_id'], pause['build_id'])
        status = '/internal/builds/' + pause['build_id'] + '/status'
        call('POST', status, {'operation_id': 'wrong', 'status': 'success'}, 409)
        for bad in ({}, {'operation_id': b['operation_id']}, {'status': 'success'},
                    {'operation_id': b['operation_id'], 'status': 'running'},
                    {'operation_id': '', 'status': 'success'}):
            with self.subTest(bad_status=bad):
                call('POST', status, bad, 400)
        callback = {'operation_id': b['operation_id'], 'status': 'success'}
        call('POST', status, callback); call('POST', status, callback)
        call('POST', status, dict(callback, status='failed'), 409)
        last = call('GET', path + '/last-snapshot')
        self.assertEqual(last['build_id'], pause['build_id']); self.assertFalse(last['env_secure'])
        cp_body = body()
        cp = call('POST', path + '/snapshot-templates', cp_body)
        self.assertEqual(call('POST', path + '/snapshot-templates', cp_body), cp)
        resolve = '/internal/snapshots/' + cp['snapshot_id'] + '/resolve?team_id='
        delete = '/internal/snapshot-templates/' + cp['snapshot_id'] + '?team_id='
        call('GET', resolve + TEAM, expected=404)
        call('GET', resolve + OTHER, expected=422)
        call('DELETE', delete + OTHER, expected=422)
        call('DELETE', delete + TEAM, expected=409)
        call('DELETE', '/internal/templates/' + b['base_template_id'] + '?team_id=' + OTHER, expected=422)
        call('DELETE', '/internal/templates/' + b['base_template_id'] + '?team_id=' + TEAM, expected=409)
        cp_status = '/internal/builds/' + cp['build_id'] + '/status'
        failed = {'operation_id': cp_body['operation_id'], 'status': 'failed', 'error': 'synthetic failure'}
        call('POST', cp_status, failed); call('POST', cp_status, failed)
        call('POST', cp_status, dict(failed, status='success'), 409)
        call('GET', resolve + TEAM, expected=404)
        call('DELETE', delete + TEAM, expected=204); call('DELETE', delete + TEAM, expected=204)
        call('DELETE', path + '/snapshot', expected=204)
        call('DELETE', path + '/snapshot', expected=404)
        call('GET', path + '/last-snapshot', expected=404)
        self.assertFalse(call('GET', path + '/snapshot-state')['paused'])

    def test_optional_roundtrip_and_aliases(self):
        path = sb(); b = body()
        b.update(metadata={'中文': '验证'}, network={'ingress': {'allowPublicAccess': False}},
                 auto_resume={'enabled': True}, volume_mounts=[{'id': 'v1', 'path': '/data'}],
                 source_machine_info={'cpu_architecture': 'aarch64'}, auto_pause=True, allow_internet_access=False)
        cp = call('POST', path + '/snapshot-templates', b)
        call('POST', '/internal/builds/' + cp['build_id'] + '/status', {'operation_id': b['operation_id'], 'status': 'success'})
        for ref in (cp['snapshot_id'], cp['template_id']):
            resolved = call('GET', '/internal/snapshots/' + ref + '/resolve?team_id=' + TEAM)
            for field in ('metadata', 'network', 'auto_resume', 'volume_mounts', 'auto_pause'):
                self.assertEqual(resolved[field], b[field])
            self.assertEqual(resolved['machine_info'], b['source_machine_info'])
        # 被其他活跃记录引用时，已完成的 checkpoint 也不能删除。
        child = sb(); child_body = body(); child_body['base_template_id'] = cp['template_id']
        call('POST', child + '/pause-snapshot', child_body)
        call('DELETE', '/internal/snapshot-templates/' + cp['template_id'] + '?team_id=' + TEAM, expected=409)
        call('DELETE', child + '/snapshot', expected=204)
        call('DELETE', '/internal/snapshot-templates/' + cp['template_id'] + '?team_id=' + TEAM, expected=204)
        call('GET', '/internal/snapshots/' + cp['snapshot_id'] + '/resolve?team_id=' + TEAM, expected=404)

    def test_concurrent_idempotency(self):
        for endpoint in ('pause-snapshot', 'snapshot-templates'):
            path = sb() + '/' + endpoint; b = body()
            with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:
                results = list(pool.map(lambda _: call('POST', path, b), range(16)))
            self.assertTrue(all(r == results[0] for r in results))
            callback = {'operation_id': b['operation_id'], 'status': 'success'}
            with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:
                list(pool.map(lambda _: call('POST', '/internal/builds/' + results[0]['build_id'] + '/status', callback), range(8)))

    def test_competing_terminal_callbacks(self):
        b = body(); allocated = call('POST', sb() + '/snapshot-templates', b)
        uri = '/internal/builds/' + allocated['build_id'] + '/status'
        barrier = threading.Barrier(2)
        def finish(status):
            barrier.wait()
            call('POST', uri, {'operation_id': b['operation_id'], 'status': status}, expected=(200, 409))
        start = len(RECORDS)
        with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
            list(pool.map(finish, ('success', 'failed')))
        self.assertEqual(sorted(r['status'] for r in RECORDS[start:]), [200, 409])


def cleanup():
    errors = []
    bases = set()
    for build, (path, b, result) in list(ALLOCATIONS.items()):
        bases.add((b['base_template_id'], b['team_id']))
        # 已终态记录返回 409，不影响后续删除；所有路径均来自本次分配结果。
        actions = [('POST', '/internal/builds/' + build + '/status', {'operation_id': b['operation_id'], 'status': 'failed'}),
                   ('DELETE', path + '/snapshot', None)]
        if 'snapshot_id' in result:
            actions.append(('DELETE', '/internal/snapshot-templates/' + result['snapshot_id'] + '?team_id=' + b['team_id'], None))
        for method, uri, data in actions:
            try:
                call(method, uri, data, (200, 409) if method == 'POST' else (204, 404))
            except AssertionError:
                errors.append(uri)
    for template, team in bases:
        try:
            call('DELETE', '/internal/templates/' + template + '?team_id=' + team, expected=204)
        except AssertionError:
            errors.append(template)
    return errors


if __name__ == '__main__':
    suite = unittest.defaultTestLoader.loadTestsFromTestCase(InternalAPIRegression)
    result = unittest.TextTestRunner(verbosity=2).run(suite)
    cleanup_errors = cleanup()
    report = dict(run_id=PREFIX, test_groups=result.testsRun, failures=len(result.failures),
                  errors=len(result.errors), cleanup_errors=cleanup_errors, requests=RECORDS)
    Path(os.environ.get('HTTP_RESULT', 'internal-api-regression.json')).write_text(json.dumps(report, indent=2), encoding='utf-8')
    raise SystemExit(0 if result.wasSuccessful() and not cleanup_errors else 1)
