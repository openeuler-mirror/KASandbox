"""九接口契约检查器回归：拒绝漂移和新增用户字段，保留 team_id。"""
import copy
import tempfile
import unittest
from pathlib import Path

import yaml

from check_agentenv_contract import check, check_user_fields


class ContractTests(unittest.TestCase):
    def test_user_fields_rejected(self):
        for name in ("user", "user_id", "userId", "username", "user_name"):
            with self.subTest(name=name), self.assertRaises(ValueError):
                check_user_fields({"properties": {"nested": {"properties": {name: {"type": "string"}}}}})
            with self.subTest(parameter=name), self.assertRaises(ValueError):
                check_user_fields({"in": "query", "name": name})

    def test_team_and_metadata_allowed(self):
        check_user_fields({"properties": {"team_id": {"type": "string"}, "metadata": {"type": "object"}}})

    def test_required_drift_detected(self):
        spec = Path(__file__).resolve().parents[3] / "spec/openapi.yml"
        document = yaml.safe_load(spec.read_text(encoding="utf-8"))
        document["paths"] = {p: op for p, op in document["paths"].items() if p.startswith("/internal/")}
        changed = copy.deepcopy(document)
        operation = changed["paths"]["/internal/templates/{templateID}"]["delete"]
        next(p for p in operation["parameters"] if p.get("name") == "team_id")["required"] = False
        with tempfile.TemporaryDirectory() as directory:
            first, second = Path(directory) / "a.yaml", Path(directory) / "b.yaml"
            first.write_text(yaml.safe_dump(document), encoding="utf-8")
            second.write_text(yaml.safe_dump(document), encoding="utf-8")
            self.assertEqual(len(check(first, second)["routes"]), 9)
            second.write_text(yaml.safe_dump(changed), encoding="utf-8")
            with self.assertRaisesRegex(ValueError, "请求契约不一致"):
                check(first, second)


if __name__ == "__main__":
    unittest.main()
