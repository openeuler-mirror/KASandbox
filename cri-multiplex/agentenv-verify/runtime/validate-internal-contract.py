#!/usr/bin/env python3
"""验证九接口响应 schema，并检查两仓库是否出现不同的请求契约。"""
import argparse
import copy
import json
import re
from pathlib import Path
from urllib.parse import urlsplit

import jsonschema
import yaml


def expand(doc, value):
    if isinstance(value, list):
        return [expand(doc, item) for item in value]
    if not isinstance(value, dict):
        return value
    if '$ref' in value:
        target = doc
        for part in value['$ref'].split('/')[1:]:
            target = target[part]
        return expand(doc, target)
    result = {key: expand(doc, item) for key, item in value.items() if key not in ('description', 'example', 'examples', 'summary')}
    if result.pop('nullable', False) and 'type' in result:
        result['type'] = [result['type'], 'null']
    return result


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--agentenv', type=Path, required=True)
    parser.add_argument('--kasandbox', type=Path, required=True)
    parser.add_argument('--results', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    agent = yaml.safe_load(args.agentenv.read_text(encoding='utf-8'))
    kas = yaml.safe_load(args.kasandbox.read_text(encoding='utf-8'))
    rows = json.loads(args.results.read_text(encoding='utf-8'))['requests']
    routes = []
    for path, item in agent['paths'].items():
        if not path.startswith('/internal/'):
            continue
        for method, op in item.items():
            if method not in ('get', 'post', 'delete', 'put', 'patch'):
                continue
            other = kas['paths'][path][method]
            for key in ('parameters', 'requestBody'):
                assert expand(agent, op.get(key)) == expand(kas, other.get(key)), f'请求契约不一致: {method} {path} {key}'
            routes.append((method.upper(), path, op))
    assert len(routes) == 9
    coverage = {method + ' ' + path: 0 for method, path, _ in routes}
    for row in rows:
        path = urlsplit(row['path']).path
        matches = [(method, template, op) for method, template, op in routes
                   if method == row['method'] and re.fullmatch(re.sub(r'\{[^}]+\}', '[^/]+', template), path)]
        assert len(matches) == 1, path
        method, template, op = matches[0]
        response = op['responses'].get(str(row['status']))
        assert response is not None, f'未声明状态码: {method} {template} {row["status"]}'
        response = expand(agent, response)
        schema = response.get('content', {}).get('application/json', {}).get('schema')
        if schema:
            jsonschema.Draft7Validator(copy.deepcopy(schema), format_checker=jsonschema.FormatChecker()).validate(row['response'])
        else:
            assert row['response'] is None, f'无响应体接口出现 JSON: {template}'
        coverage[method + ' ' + template] += 1
    assert all(coverage.values())
    args.output.write_text(json.dumps(dict(result='PASS', responses=len(rows), coverage=coverage,
                                          request_contracts_equal=True), indent=2), encoding='utf-8')
    print(f'PASS: {len(rows)} responses, 9 schemas, request contracts equal')


if __name__ == '__main__':
    main()
