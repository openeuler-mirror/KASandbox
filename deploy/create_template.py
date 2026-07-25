import os
import json
import argparse
from e2b import Template, default_build_logger, wait_for_port, Sandbox

if __name__ == '__main__':
    # 默认配置
    DEFAULT_SERVER_IP = "10.10.10.10"
    DEFAULT_HARBOR_IP = "10.10.10.10"
    
    # 解析命令行参数
    parser = argparse.ArgumentParser(description='构建 E2B Ubuntu 模板')
    parser.add_argument('--server-ip', default=DEFAULT_SERVER_IP, help=f'Server IP 地址 (默认：{DEFAULT_SERVER_IP})')
    parser.add_argument('--harbor-ip', default=DEFAULT_HARBOR_IP, help=f'Harbor IP 地址 (默认：{DEFAULT_HARBOR_IP})')
    args = parser.parse_args()
    
    SERVER_IP = args.server_ip
    HARBOR_IP = args.harbor_ip
    
    print(f"使用配置：")
    print(f"  SERVER_IP: {SERVER_IP}")
    print(f"  HARBOR_IP: {HARBOR_IP}")
    
    # 设置E2B环境变量
    os.environ["E2B_API_URL"] = f"http://{SERVER_IP}:3000"
    os.environ["E2B_HTTP_SSL"] = "false"
    config_path = "/root/.e2b/config.json"
    os.environ["E2B_DOMAIN"] = "e2b.app"
    
    print(f"\n读取配置文件路径：{config_path}")
    
    # 初始化变量，避免异常时变量未定义
    access_token = None
    team_api_key = None
    
    # 1. 打开并读取文件内容
    with open(config_path, "r", encoding="utf-8") as f:
        # 2. 解析JSON内容为Python字典
        data = json.load(f)

    # 3. 提取目标字段（使用get方法避免键不存在报错）
    access_token = data.get("accessToken")
    team_api_key = data.get("teamApiKey")

    # 4. 输出结果
    print("提取结果：")
    print(f"accessToken: {access_token}")
    print(f"teamApiKey: {team_api_key}")

    # 验证字段是否存在
    if not access_token or not team_api_key:
        print("警告：文件中未找到accessToken或teamApiKey字段！")
        # 字段缺失时直接退出，避免后续执行失败
        exit(1)
    
    # 设置E2B相关环境变量
    os.environ["E2B_ACCESS_TOKEN"] = access_token
    os.environ["E2B_API_KEY"] = team_api_key
    
    # 构建E2B模板
    print("\n开始构建E2B模板...")
    Template.build(
        Template().from_dockerfile(f'FROM {HARBOR_IP}:30443/e2b-orchestration/ubuntu:22.04-custom'),
        alias="openclaw",
        cpu_count=2,
        memory_mb=2048,
        on_build_logs=default_build_logger(),
        skip_cache=True
    )
    print("模板构建完成！")