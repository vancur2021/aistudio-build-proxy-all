import time
import os
import random
from playwright.sync_api import Page, expect

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

def handle_untrusted_dialog(page: Page, logger=None):
    """
    检查并处理 "This app is from another developer" 或 "Last modified by..." 的弹窗。
    如果弹窗出现，则点击 "Continue to the app" 或 "OK" 按钮。
    """
    # 尝试定位 "Continue to the app" 按钮 (新版 UI)
    continue_button_locator = page.get_by_role("button", name="Continue to the app")
    # 尝试定位 "OK" 按钮 (旧版 UI)
    ok_button_locator = page.get_by_role("button", name="OK")

    try:
        # 优先检查新版按钮
        if continue_button_locator.is_visible(timeout=10000): # 等待最多10秒
            logger.info(f"检测到安全提示弹窗，正在点击 'Continue to the app' 按钮...")
            continue_button_locator.click(force=True)
            logger.info(f"'Continue to the app' 按钮已点击。")
            expect(continue_button_locator).to_be_hidden(timeout=1000)
            logger.info(f"弹窗已确认关闭。")
        # 如果没有新版按钮，检查旧版按钮
        elif ok_button_locator.is_visible(timeout=1000):
            logger.info(f"检测到弹窗，正在点击 'OK' 按钮...")
            ok_button_locator.click(force=True)
            logger.info(f"'OK' 按钮已点击。")
            expect(ok_button_locator).to_be_hidden(timeout=1000)
            logger.info(f"弹窗已确认关闭。")
        else:
            logger.info(f"在10秒内未检测到任何已知弹窗，继续执行...")
    except Exception as e:
        logger.info(f"检查弹窗时发生意外：{e}，将继续执行...")

def handle_successful_navigation(page: Page, logger, cookie_file_config):
    """
    在成功导航到目标页面后，执行后续操作（处理弹窗、截图、保持运行）。
    """
    logger.info("已成功到达目标页面。")
    page.click('body') # 给予页面焦点

    # 检查并处理 "Last modified by..." 的弹窗
    handle_untrusted_dialog(page, logger=logger)

    # 等待页面加载和渲染后截图
    logger.info("等待15秒以便页面完全渲染...")
    time.sleep(15)
    
    screenshot_dir = 'logs'
    screenshot_filename = os.path.join(screenshot_dir, f"screenshot_{cookie_file_config}_{int(time.time())}.png")
    try:
        page.screenshot(path=screenshot_filename, full_page=True)
        logger.info(f"已截屏到: {screenshot_filename}")
    except Exception as e:
        logger.error(f"截屏时出错: {e}")
        
    logger.info("实例将保持运行状态。每10秒进行一次拟人化鼠标滑动并点击页面以保持活动。")
    trigger_file = os.path.join('logs', f"take_screenshot_{cookie_file_config}.trigger")
    
    viewport = page.viewport_size
    width = viewport['width'] if viewport else 1280
    height = viewport['height'] if viewport else 720
    
    # 初始鼠标位置
    current_x = random.randint(0, width)
    current_y = random.randint(0, height)

    while True:
        try:
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
                manual_screenshot = os.path.join('logs', f"manual_screenshot_{cookie_file_config}_{int(time.time())}.png")
                page.screenshot(path=manual_screenshot, full_page=True)
                logger.info(f"手动截图已保存至: {manual_screenshot}")
                try:
                    os.remove(trigger_file) # 截图后删除触发文件
                except OSError as e:
                    logger.error(f"删除触发文件失败: {e}")
                    
            time.sleep(10)
        except Exception as e:
            logger.error(f"在保持活动循环中出错: {e}")
            break # 如果页面关闭或出错，则退出循环
