#!/usr/bin/env python3
"""验收原生网络安装态，仅创建和清理本次唯一 Pod，不重启任何服务。"""
import argparse
import ipaddress
import json
import shlex
import subprocess as sp
import time
import tomllib
import urllib.request
import uuid
from pathlib import Path


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--output', required=True)
    parser.add_argument('--template', required=True)
    parser.add_argument('--node', default='node1')
    parser.add_argument('--node-ip', required=True)
    parser.add_argument('--socket', default='/run/cri-multiplex.sock')
    parser.add_argument('--portforward-helper', required=True)
    args = parser.parse_args()
    out = Path(args.output); out.mkdir(parents=True, exist_ok=False)
    ns = 'agentenv-e2e'
    pod_name = 'native-' + uuid.uuid4().hex[:10]
    key = tomllib.loads(Path('/root/.config/aenv/credentials').read_text())['api_key']
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def command(argv, timeout=120):
        return sp.check_output(argv, text=True, stderr=sp.STDOUT, timeout=timeout).strip()

    def save(name, value):
        text = value if isinstance(value, str) else json.dumps(value, indent=2)
        assert key not in text
        (out / name).write_text(text)

    def api(path):
        return json.load(opener.open(urllib.request.Request('http://127.0.0.1:8000' + path,
                                                           headers={'X-API-Key': key}), timeout=15))

    def cri(*argv):
        return command(['crictl', '--runtime-endpoint=unix://' + args.socket, *argv])

    def snapshot():
        return {'health': api('/health/ready'), 'sandboxes': len(api('/sandboxes')),
                'boot': Path('/proc/sys/kernel/random/boot_id').read_text(),
                'services': command(['systemctl', 'show', 'aenv', 'cri-multiplex', 'kubelet', 'containerd',
                                     'e2b-webhook-agentenv', '-p', 'Id', '-p', 'MainPID', '-p', 'ActiveEnterTimestamp']),
                'netns': sorted(x.split()[0] for x in command(['ip', 'netns', 'list']).splitlines()),
                'pods': json.loads(command(['kubectl', 'get', 'pods', '-A', '-o', 'json']))}

    def wait(predicate, description, timeout=180):
        end = time.monotonic() + timeout
        while time.monotonic() < end:
            if predicate(): return
            time.sleep(1)
        raise RuntimeError('timeout: ' + description)

    before = snapshot(); save('before.json', before)
    assert before['sandboxes'] == 0 and before['health']['ready']
    assert not any(p['spec'].get('runtimeClassName') in ('agentenv', 'aenv') for p in before['pods']['items'])
    assert command(['kubectl', 'get', 'runtimeclass', 'agentenv', '-o', 'jsonpath={.handler}']) == 'agentenv'
    # 既有 Webhook 按 namespace 名称筛选，只复用专用测试 namespace。
    command(['kubectl', 'get', 'namespace', ns])
    existing = json.loads(command(['kubectl', 'get', 'pods', '-n', ns, '-o', 'json']))
    assert all(p['metadata']['name'] != pod_name for p in existing['items'])
    uid = None
    try:
        pod = {'apiVersion': 'v1', 'kind': 'Pod', 'metadata': {'name': pod_name, 'namespace': ns, 'labels': {'native-installed-probe': pod_name},
               'annotations': {'sandbox.cri/template': args.template, 'aenv.dev/secure': 'false',
                               'sandbox.cri/metadata': json.dumps({'native-installed-probe': pod_name})}},
               'spec': {'nodeName': args.node, 'runtimeClassName': 'agentenv', 'restartPolicy': 'Never',
                        'terminationGracePeriodSeconds': 5, 'containers': [{'name': 'app', 'image': 'agentenv.dev/app:v1',
                        'imagePullPolicy': 'IfNotPresent', 'command': ['/bin/sh', '-c', 'sleep 600'],
                        'ports': [{'name': 'http', 'containerPort': 18080, 'protocol': 'TCP'}],
                        'readinessProbe': {'exec': {'command': ['/bin/sh', '-c', 'true']},
                                           'periodSeconds': 2, 'timeoutSeconds': 2, 'failureThreshold': 30}}]}}
        save('pod-input.json', pod)
        dry = json.loads(command(['kubectl', 'create', '--dry-run=server', '-f', str(out/'pod-input.json'), '-o', 'json']))
        save('pod-dryrun.json', dry)
        for name in ['aenv.dev/template-id', 'sandbox.cri/build']: assert dry['metadata']['annotations'].get(name)
        command(['kubectl', 'create', '-f', str(out/'pod-input.json')])
        command(['kubectl', 'wait', '-n', ns, 'pod/' + pod_name, '--for=condition=Ready', '--timeout=180s'], timeout=190)
        live = json.loads(command(['kubectl', 'get', 'pod', pod_name, '-n', ns, '-o', 'json'])); save('pod-live.json', live)
        uid = live['metadata']['uid']; ip = live['status']['podIP']; assert ipaddress.ip_address(ip).is_global or ipaddress.ip_address(ip).is_private
        assert not ipaddress.ip_address(ip).is_link_local
        cid = live['status']['containerStatuses'][0]['containerID'].split('://', 1)[1]
        status = json.loads(cri('inspectp', uid)); save('cri-pod.json', status)
        assert status['status']['state'] == 'SANDBOX_READY' and status['status']['network']['ip'] == ip
        container = json.loads(cri('inspect', cid)); save('cri-container.json', container)
        assert container['status']['state'] == 'CONTAINER_RUNNING'
        exec_prefix = ['kubectl', 'exec', '-n', ns, pod_name, '--', '/bin/sh', '-c']
        assert command(exec_prefix + ['printf NATIVE_INSTALLED_EXEC_OK']) == 'NATIVE_INSTALLED_EXEC_OK'
        code = sp.run(exec_prefix + ['exit 37'], capture_output=True, text=True, timeout=30)
        assert code.returncode == 37
        stats = json.loads(cri('stats', '--output', 'json', cid)); save('stats.json', stats)
        assert len(stats.get('stats', [])) == 1
        measured = stats['stats'][0]
        assert measured['attributes']['id'] == cid
        annotations = measured['attributes']['annotations']
        # 现有 Runtime 返回 CPU 百分比，不伪造 CRI 累计 CPU 时间。
        assert annotations['agentenv.dev/stats-scope'] == 'sandbox-aggregate'
        assert float(annotations['agentenv.dev/cpu-used-percent']) >= 0
        assert int(measured['memory']['workingSetBytes']['value']) >= 0
        assert int(measured['writableLayer']['usedBytes']['value']) >= 0
        body = 'NATIVE_INSTALLED_' + pod_name
        # 持续监听并先读取请求头，避免循环 nc 的非标准报文和监听间隙。
        handler = '#!/bin/sh\nwhile IFS= read -r line; do\n [ -z "$line" ] && break\n [ "$line" = "$(printf \'\\r\')" ] && break\ndone\nprintf \'HTTP/1.1 200 OK\\r\\nContent-Length: ' + str(len(body)) + '\\r\\nConnection: close\\r\\n\\r\\n' + body + "'\n"
        server = 'printf %s ' + shlex.quote(handler) + ' >/tmp/native-handler.sh && chmod 700 /tmp/native-handler.sh && nohup nc -lk -p 18080 -e /tmp/native-handler.sh >/tmp/native-http.log 2>&1 </dev/null &'
        save('http-server-fixture.txt', command(exec_prefix + [server]))
        def http_ok():
            try:
                with opener.open('http://' + ip + ':18080/', timeout=2) as response:
                    return response.status == 200 and response.read() == body.encode()
            except OSError: return False
        wait(http_ok, 'PodIP HTTP', timeout=30)
        native_id = next(x['sandboxID'] for x in api('/sandboxes') if x.get('metadata', {}).get('native-installed-probe') == pod_name)
        request = urllib.request.Request('http://127.0.0.1:8000/proxy/', headers={'X-API-Key': key,
                    'x-agentenv-sandbox-id': native_id, 'x-agentenv-target-port': '18080'})
        with opener.open(request, timeout=10) as response: assert response.status == 200 and response.read() == body.encode()
        save('portforward.txt', command([args.portforward_helper, '-socket', args.socket, '-pod', uid,
                                        '-port', '18080', '-node-ip', args.node_ip, '-expected', body], timeout=30))
        save('live-result.json', {'uid': uid, 'native_id': native_id, 'pod_ip': ip, 'webhook': True,
                                 'ready_probe': True, 'exec': True, 'exit37': True, 'stats': True,
                                 'pod_ip_http': True, 'native_proxy_http': True, 'portforward_http': True})
        print('PASS native installed functional checks', flush=True)
    finally:
        # 只删除带本次唯一标签的 Pod，保留已有 namespace。
        try: save('pod-final.json', json.loads(command(['kubectl', 'get', 'pods', '-n', ns, '-o', 'json'])))
        except sp.CalledProcessError: pass
        remaining = json.loads(command(['kubectl', 'get', 'pods', '-n', ns, '-o', 'json']))
        for item in remaining['items']:
            if item['metadata']['name'] == pod_name:
                assert item['metadata'].get('labels', {}).get('native-installed-probe') == pod_name
                command(['kubectl', 'delete', 'pod', pod_name, '-n', ns, '--wait=true', '--timeout=180s'], timeout=190)
        def drained():
            health = api('/health/ready')
            return not api('/sandboxes') and health['ready'] and all(health.get(k, 0) == before['health'].get(k, 0)
                for k in ['pausedSandboxes', 'unknownOperations', 'quarantinedOperations', 'firecrackerWorkDirs', 'runtimeNetns'])
        wait(drained, 'Runtime cleanup')
        if uid:
            wait(lambda: not any(p['id'] == uid for p in json.loads(cri('pods', '-o', 'json')).get('items', [])), 'CRI cleanup')
        after = snapshot(); save('after.json', after)
        for k in ['boot', 'services', 'netns']: assert before[k] == after[k], k
        assert {p['metadata']['uid'] for p in before['pods']['items']} == {p['metadata']['uid'] for p in after['pods']['items']}
    save('result.json', {'result': 'PASS', 'namespace': ns, 'pod': pod_name, 'cleanup': True})
    print('PASS native installed cleanup and shared service audit', flush=True)


if __name__ == '__main__': main()
