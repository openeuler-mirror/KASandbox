#!/usr/bin/env python3
"""批量创建 E2B 模板

根据基础镜像派生多个不同的镜像（Dockerfile 变体），并发创建指定数量的模板，
创建完成后输出所有创建的模板名（alias）。

用法示例：
    # 无参数运行（使用默认值：镜像 ubuntu:22.04-custom，创建 10 个模板）
    python3 create_templates.py

    # 指定参数运行
    python3 create_templates.py \
        --image 10.10.10.10:30443/e2b-orchestration/ubuntu:22.04-custom \
        --template-count 10 \
        --concurrency 5 \
        --image-count 3
"""

import os
import re
import json
import argparse
from concurrent.futures import ThreadPoolExecutor, as_completed

from e2b import Template

DEFAULT_SERVER_IP = "10.10.10.10"
DEFAULT_HARBOR_IP = "10.10.10.10"
E2B_CONFIG_PATH = "/root/.e2b/config.json"
HARBOR_PORT = 30443

CPU_COUNT = 2
MEMORY_MB = 2048

DEFAULT_BASE_IMAGE = "ubuntu:22.04-custom"
DEFAULT_TEMPLATE_COUNT = 1


def setup_e2b_env(server_ip: str) -> None:
    """设置 E2B 环境变量，并从配置文件读取凭证。"""
    os.environ["E2B_API_URL"] = f"http://{server_ip}:3000"
    os.environ["E2B_HTTP_SSL"] = "false"
    os.environ["E2B_DOMAIN"] = "e2b.app"

    try:
        with open(E2B_CONFIG_PATH, "r", encoding="utf-8") as f:
            data = json.load(f)
    except (OSError, json.JSONDecodeError) as e:
        raise RuntimeError(f"读取配置文件 {E2B_CONFIG_PATH} 失败: {e}")

    access_token = data.get("accessToken")
    team_api_key = data.get("teamApiKey")
    if not access_token or not team_api_key:
        raise RuntimeError(f"配置文件 {E2B_CONFIG_PATH} 中未找到 accessToken 或 teamApiKey 字段")

    os.environ["E2B_ACCESS_TOKEN"] = access_token
    os.environ["E2B_API_KEY"] = team_api_key


def normalize_image(image: str, harbor_ip: str) -> str:
    """镜像名不含仓库地址时，自动补全 Harbor 仓库前缀。"""
    if "/" in image:
        registry = image.split("/", 1)[0]
        if "." in registry or ":" in registry or registry == "localhost":
            return image
    return f"{harbor_ip}:{HARBOR_PORT}/e2b-orchestration/{image}"


def alias_prefix_from_image(image: str) -> str:
    """从镜像名生成模板别名前缀（小写字母、数字、短横线）。"""
    name = image.split("/")[-1]
    prefix = re.sub(r"[^a-z0-9]+", "-", name.lower()).strip("-")
    return prefix[:30] or "template"


def make_dockerfile(base_image: str, variant: int) -> str:
    """基于基础镜像生成第 variant 个派生镜像的 Dockerfile。"""
    return f'FROM {base_image}\nLABEL e2b.image-variant="{variant}"\n'


def make_logger(alias: str):
    """构建带模板名前缀的日志函数，便于并发时区分日志。"""
    def log(line: str) -> None:
        print(f"[{alias}] {line}", flush=True)
    return log


def create_template(index: int, base_image: str, prefix: str, image_count: int) -> dict:
    """创建第 index 个模板，返回创建结果信息。"""
    variant = (index - 1) % image_count + 1
    alias = f"{prefix}-{index}"
    dockerfile = make_dockerfile(base_image, variant)
    info = {"index": index, "alias": alias, "image_variant": variant, "error": None}
    try:
        print(f"[{alias}] 开始创建（基于镜像变体 {variant}）", flush=True)
        Template.build(
            Template().from_dockerfile(dockerfile),
            alias=alias,
            cpu_count=CPU_COUNT,
            memory_mb=MEMORY_MB,
            on_build_logs=make_logger(alias),
            skip_cache=True,
        )
        print(f"[{alias}] 创建成功", flush=True)
    except Exception as e:
        info["error"] = str(e)
        print(f"[{alias}] 创建失败: {e}", flush=True)
    return info


def main():
    parser = argparse.ArgumentParser(description="批量创建 E2B 模板")
    parser.add_argument("--image", default=DEFAULT_BASE_IMAGE,
                        help=f'基础镜像名（默认：{DEFAULT_BASE_IMAGE}），'
                             '例如 10.10.10.10:30443/e2b-orchestration/ubuntu:22.04-custom')
    parser.add_argument("--template-count", type=int, default=DEFAULT_TEMPLATE_COUNT,
                        help=f'创建模板个数（默认：{DEFAULT_TEMPLATE_COUNT}）')
    parser.add_argument("--concurrency", type=int, default=1,
                        help="并发数（同时创建模板的个数，默认 1）")
    parser.add_argument("--image-count", type=int, default=1,
                        help="镜像个数（根据基础镜像派生的不同镜像数，默认 1）")
    parser.add_argument("--server-ip", default=DEFAULT_SERVER_IP,
                        help=f'E2B Server IP（默认：{DEFAULT_SERVER_IP}）')
    parser.add_argument("--harbor-ip", default=DEFAULT_HARBOR_IP,
                        help=f'Harbor IP（默认：{DEFAULT_HARBOR_IP}，镜像名不含仓库地址时用于补全）')
    args = parser.parse_args()

    if args.template_count < 1:
        parser.error("--template-count 必须 >= 1")
    if args.concurrency < 1:
        parser.error("--concurrency 必须 >= 1")
    if args.image_count < 1:
        parser.error("--image-count 必须 >= 1")

    base_image = normalize_image(args.image, args.harbor_ip)
    prefix = alias_prefix_from_image(base_image)

    setup_e2b_env(args.server_ip)

    print("使用配置：")
    print(f"  SERVER_IP: {args.server_ip}")
    print(f"  基础镜像: {base_image}")
    print(f"  镜像个数: {args.image_count}（根据基础镜像派生）")
    print(f"  创建模板个数: {args.template_count}")
    print(f"  并发数: {args.concurrency}")
    print(f"  模板别名前缀: {prefix}\n")

    results = []
    with ThreadPoolExecutor(max_workers=args.concurrency) as pool:
        futures = [
            pool.submit(create_template, i, base_image, prefix, args.image_count)
            for i in range(1, args.template_count + 1)
        ]
        for future in as_completed(futures):
            results.append(future.result())

    results.sort(key=lambda r: r["index"])
    ok = [r for r in results if not r["error"]]
    failed = [r for r in results if r["error"]]

    print("\n========== 创建结果 ==========")
    print(f"成功: {len(ok)}/{len(results)}")
    if ok:
        print("创建的模板名：")
        for r in ok:
            print(f"  {r['alias']}（镜像变体 {r['image_variant']}）")
    if failed:
        print("失败的模板：")
        for r in failed:
            print(f"  {r['alias']}: {r['error']}")
        raise SystemExit(1)


if __name__ == "__main__":
    main()
