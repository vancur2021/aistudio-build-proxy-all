import argparse
import base64
import json
import logging
import os
import sys

from browser.instance import run_browser_instance
from utils.logger import setup_logging


def main():
    """
    通过 Go 进程传递的 JSON (Base64) 配置，独立运行一个 Camoufox 实例。
    主要给 Go 进程调用使用:
    python run_single.py --config-b64 <base64_encoded_json>
    """
    log_dir = 'logs'
    os.makedirs(log_dir, exist_ok=True)
    logger = setup_logging(os.path.join(log_dir, 'app.log'))

    parser = argparse.ArgumentParser(description="独立运行一个 Camoufox 实例。")
    parser.add_argument("--config-b64", required=True, help="Base64 编码的 JSON 配置字符串。")
    args = parser.parse_args()

    try:
        # 1. 解码传入的配置
        json_str = base64.b64decode(args.config_b64).decode('utf-8')
        config = json.loads(json_str)
    except Exception as e:
        logger.error(f"[run_single] 解析配置失败: {e}")
        sys.exit(1)

    logger.info(f"--------------------- 独立 Camoufox 实例启动 ---------------------")
    logger.info(f"目标 URL: {config.get('url')}")
    logger.info(f"Cookie 文件: {config.get('cookie_file')}")

    try:
        # 直接运行 browser/instance.py 里的通用逻辑
        run_browser_instance(config)
    except KeyboardInterrupt:
        logger.info("[run_single] 接收到终止信号，实例退出。")
    except Exception as e:
        logger.exception(f"[run_single] 实例运行发生异常: {e}")
        sys.exit(1)


if __name__ == "__main__":
    main()
