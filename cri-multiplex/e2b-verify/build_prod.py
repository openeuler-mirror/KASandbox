# build_prod.py
from dotenv import load_dotenv
load_dotenv()
import os
import sys
from e2b import Template, default_build_logger

if __name__ == '__main__':
    if len(sys.argv) < 2:
        print("Usage: python build_prod.py <image_name>")
        print("Example: python build_prod.py golang:1.24")
        sys.exit(1)
    
    image_name = sys.argv[1]
    # 将镜像名中的 : 和 . 都替换为 -
    alias = image_name.replace(':', '-').replace('.', '-')
    
    dockerfile = f'FROM harbor:443/e2b-orchestration/{image_name}'
    
    print(f"Building template: {alias}")
    print(f"Dockerfile: {dockerfile}")
    
    Template.build(
        Template().from_dockerfile(dockerfile),
        alias=alias,
        cpu_count=1,
        memory_mb=1024,
        on_build_logs=default_build_logger()
    )
