import time
import os
import random
import requests
import json
from playwright.sync_api import Page, expect
from utils.cookie_handler import save_cookies_to_file

def generate_bezier_curve(start_x, start_y, end_x, end_y, num_points=50):
    """生成三次贝塞尔曲线轨迹点，用于模拟真实鼠标滑动轨迹"""
    cp1_x = start_x + (end_x - start_x) * random.uniform(0.2, 0.8) + random.uniform(-50, 50)
    cp1_y = start_y + (end_y - start_y) * random.uniform(0.2, 0.8) + random.uniform(-50, 50)
    cp2_x = start_x + (end_x - start_x) * random.uniform(0.2, 0.8) + random.uniform(-50, 50)
    cp2_y = start_y + (end_y - start_y) * random.uniform(0.2, 0.8) + random.uniform(-50, 50)

    points = []
    for i in range(num_points):
        t = i / (num_points - 1)
        x = (1-t)**3 * start_x + 3*(1-t)**2 * t * cp1_x + 3*(1-t) * t**2 * cp2_x + t**3 * end_x
        y = (1-t)**3 * start_y + 3*(1-t)**2 * t * cp1_y + 3*(1-t) * t**2 * cp2_y + t**3 * end_y
        points.append({'x': x, 'y': y})
    return points

def human_like_mouse_move(page: Page, start_x, start_y, dest_x, dest_y):
    """模拟人类鼠标滑动"""
    page.mouse.move(start_x, start_y)
    points = generate_bezier_curve(start_x, start_y, dest_x, dest_y)
    for point in points:
        page.mouse.move(point['x'], point['y'])
        # 模拟人类的不均匀速度
        time.sleep(random.uniform(0.002, 0.01))

def handle_untrusted_dialog(page: Page, timeout_ms=5000, logger=None):
    """
    检查并处理 "This app is from another developer" 等弹窗，以及 "Terms of Service" 提示横幅。
    使用模糊和包含的文本定位而非严格的 role，兼容各种复杂的嵌套 DOM。
    """
    if logger is None:
        import logging
        logger = logging.getLogger(__name__)

    try:
        # 1. 优先扫荡顶部可能残留的 "Terms of Service"
        dismiss_locator = page.get_by_text("Dismiss", exact=True)
        if dismiss_locator.is_visible(timeout=1000):
            logger.info("检测到顶部 Terms of Service 横幅，正在点击 Dismiss...")
            dismiss_locator.click(force=True)
            try:
                expect(dismiss_locator).to_be_hidden(timeout=2000)
            except Exception:
                pass
    except Exception as e:
        logger.debug(f"寻找或点击 Dismiss 时发生小失误，可忽略: {e}")

    try:
        # 2. 检查 "Continue to the app" 核心拦截页
        # 终极定位策略：组合 CSS 类名、XPath 和模糊文本匹配，应对 Angular 动态 DOM 和隐藏字符
        
        # 策略 A: 精确的 CSS 类名 + 模糊文本包含 (最稳健)
        css_locator = page.locator("button.ms-button-primary:has-text('Continue to the app')").first
        
        # 策略 B: 纯文本模糊匹配 (忽略大小写和前后空白)
        text_locator = page.get_by_text("Continue to the app", exact=False).first
        
        # 策略 C: 绝对 XPath (作为最后的兜底)
        xpath_locator = page.locator("xpath=/html/body/div[1]/div/div[2]/mat-dialog-container/div/div/ms-untrusted-dialog/mat-dialog-actions/button").first
        
        # 策略 D: 旧版 OK 按钮
        ok_locator = page.get_by_role("button", name="OK").first
        
        # 将所有策略组合起来，只要有一个命中即可
        combined_locator = css_locator.or_(text_locator).or_(xpath_locator).or_(ok_locator)

        # 尝试等待任意一个元素变为可见
        try:
            combined_locator.wait_for(state="visible", timeout=timeout_ms)
        except Exception:
            # wait_for 超时说明没有任何弹窗出现
            logger.info(f"在 {timeout_ms}ms 内未检测到任何已知安全弹窗，继续执行...")
            return

        # 依次检查哪个定位器命中了，并执行点击
        if css_locator.is_visible():
            logger.info("检测到安全提示弹窗 (CSS策略命中)，正在点击 'Continue to the app'...")
            css_locator.click(force=True)
        elif text_locator.is_visible():
            logger.info("检测到安全提示弹窗 (文本策略命中)，正在点击 'Continue to the app'...")
            text_locator.click(force=True)
        elif xpath_locator.is_visible():
            logger.info("检测到安全提示弹窗 (XPath策略命中)，正在点击 'Continue to the app'...")
            xpath_locator.click(force=True)
        elif ok_locator.is_visible():
            logger.info("检测到弹窗 (旧版OK命中)，正在点击 'OK' 按钮...")
            ok_locator.click(force=True)

        logger.info("弹窗按钮已触发点击指令。")
        # 让出一点协程时间让页面切换动作完成
        page.wait_for_timeout(1500)
            
    except Exception as e:
        logger.warning(f"处理安全弹窗过程中发生意外: {e}，将强制跳过...")

def handle_successful_navigation(page: Page, context, logger, cookie_file_config, cookie_file_path, original_cookie_names):
    """
    在成功导航到目标页面后，执行后续操作（处理弹窗、截图、保持运行、持久化Cookie）。
    """
    logger.info("已成功到达目标页面。")
    page.click('body') # 给予页面焦点

    # 检查并处理 "Last modified by..." 的弹窗
    handle_untrusted_dialog(page, logger=logger)

    # 等待页面加载和渲染
    logger.info("等待10秒以便页面完全渲染...")
    time.sleep(10)
    
    screenshot_dir = 'logs'
    os.makedirs(screenshot_dir, exist_ok=True)
        
    logger.info("实例将保持运行状态。每10秒进行一次拟人化鼠标滑动并点击页面以保持活动。")
    trigger_file = os.path.join('logs', f"take_screenshot_{cookie_file_config}.trigger")
    refresh_trigger_file = os.path.join('logs', f"refresh_cookie_{cookie_file_config}.trigger")
    reload_trigger_file = os.path.join('logs', f"reload_page_{cookie_file_config}.trigger")
    
    viewport = page.viewport_size
    width = viewport['width'] if viewport else 1280
    height = viewport['height'] if viewport else 720
    
    # 初始鼠标位置
    current_x = random.randint(0, width)
    current_y = random.randint(0, height)
    
    # 用于记录上报给GoのGuest ID状态
    last_reported_guest_id = None
    bind_api_url = f"http://127.0.0.1:5345/api/process/{cookie_file_config}/bind"
    
    # 记录上次保存 Cookie 的时间
    last_cookie_save_time = time.time()
    cookie_save_interval = 3600 # 每小时保存一次

    while True:
        try:
            # 尝试在所有frame中寻找 Guest ID (websocket-proxy-logger 生成的 client_id)
            current_guest_id = None
            try:
                for frame in page.frames:
                    # 使用您提供的 xpath 定位器
                    locator = frame.locator('xpath=//*[@id="root"]/div/div[1]/div[2]/div/span[2]')
                    if locator.count() > 0 and locator.is_visible(timeout=500):
                        text = locator.inner_text().strip()
                        if text and text != last_reported_guest_id:
                            current_guest_id = text
                            break
            except Exception as find_id_e:
                logger.debug(f"尝试获取 Guest ID 时发生小错误(可忽略): {find_id_e}")
            
            # 如果找到了新的 Guest ID，向 Go 服务端发起绑定上报
            if current_guest_id:
                try:
                    payload = {"client_id": current_guest_id}
                    headers = {"Content-Type": "application/json"}
                    resp = requests.post(bind_api_url, json=payload, headers=headers, timeout=5)
                    if resp.status_code == 200:
                        logger.info(f"成功将实例与 Client ID 绑定: {current_guest_id} -> {cookie_file_config}")
                        last_reported_guest_id = current_guest_id
                        
                        # 首次绑定成功后，立即进行一次 Cookie 持久化
                        logger.info("首次绑定成功，执行初始 Cookie 持久化...")
                        save_cookies_to_file(context, cookie_file_path, original_cookie_names, logger)
                        last_cookie_save_time = time.time()
                    else:
                        logger.warning(f"上报绑定失败: HTTP {resp.status_code} - {resp.text}")
                except Exception as req_e:
                    logger.error(f"向 Go 服务端上报 Client ID 时发生异常: {req_e}")

            # 定期持久化 Cookie
            current_time = time.time()
            if current_time - last_cookie_save_time > cookie_save_interval:
                logger.info("执行定期 Cookie 持久化...")
                save_cookies_to_file(context, cookie_file_path, original_cookie_names, logger)
                last_cookie_save_time = current_time

            # 随机生成下一个目标点
            dest_x = random.randint(0, width)
            dest_y = random.randint(0, height)
            
            # 拟人化滑动到目标点
            human_like_mouse_move(page, current_x, current_y, dest_x, dest_y)
            current_x, current_y = dest_x, dest_y
            
            page.click('body')
            
            # 检查是否存在触发文件，如果存在则执行手动截图
            if os.path.exists(trigger_file):
                logger.info("检测到截图触发文件，正在执行手动截图...")
                # 为了方便Go服务端读取，保存为固定文件名的截图（覆盖之前的手动截图）
                manual_screenshot = os.path.join(screenshot_dir, f"manual_screenshot_{cookie_file_config}.png")
                page.screenshot(path=manual_screenshot, full_page=True)
                logger.info(f"手动截图已保存至: {manual_screenshot}")
                try:
                    os.remove(trigger_file) # 截图完成后删除触发文件以通知Go服务端
                except OSError as e:
                    logger.error(f"删除触发文件失败: {e}")
                    
            # 检查是否存在手动回刷 Cookie 的触发文件
            if os.path.exists(refresh_trigger_file):
                logger.info("检测到手动回刷 Cookie 触发文件，正在执行回刷...")
                try:
                    save_cookies_to_file(context, cookie_file_path, original_cookie_names, logger)
                    last_cookie_save_time = time.time() # 更新最后保存时间
                except Exception as e:
                    logger.error(f"手动回刷 Cookie 失败: {e}")
                finally:
                    try:
                        os.remove(refresh_trigger_file)
                    except OSError as e:
                        logger.error(f"删除回刷触发文件失败: {e}")
                        
            # 检查是否存在刷新页面的触发文件
            if os.path.exists(reload_trigger_file):
                logger.info("检测到页面刷新触发文件，正在刷新页面...")
                try:
                    page.reload()
                    logger.info("页面刷新完成。")
                except Exception as e:
                    logger.error(f"页面刷新失败: {e}")
                finally:
                    try:
                        os.remove(reload_trigger_file)
                    except OSError as e:
                        logger.error(f"删除刷新触发文件失败: {e}")

            # 短暂心跳睡眠，提高响应 trigger 的速度
            # 因为整个大循环里还有 time.sleep 的模拟人类停顿，所以这里可以适当缩小
            time.sleep(2)
        except Exception as e:
            logger.error(f"在保持活动循环中出错: {e}")
            break # 如果页面关闭或出错，则退出循环
