def convert_cookie_editor_to_playwright(cookies_from_editor, logger=None):
    """
    将从 Cookie-Editor 插件导出的 cookie 列表转换为 Playwright 兼容的格式。
    """
    playwright_cookies = []
    allowed_keys = {'name', 'value', 'domain', 'path', 'expires', 'httpOnly', 'secure', 'sameSite'}

    for cookie in cookies_from_editor:
        pw_cookie = {}
        for key in ['name', 'value', 'domain', 'path', 'httpOnly', 'secure']:
            if key in cookie:
                pw_cookie[key] = cookie[key]
        if cookie.get('session', False):
            pw_cookie['expires'] = -1
        elif 'expirationDate' in cookie:
            if cookie['expirationDate'] is not None:
                pw_cookie['expires'] = int(cookie['expirationDate'])
            else:
                pw_cookie['expires'] = -1
        
        if 'sameSite' in cookie:
            same_site_value = str(cookie['sameSite']).lower()
            if same_site_value == 'no_restriction':
                pw_cookie['sameSite'] = 'None'
            elif same_site_value in ['lax', 'strict']:
                pw_cookie['sameSite'] = same_site_value.capitalize()
            elif same_site_value == 'unspecified':
                pw_cookie['sameSite'] = 'Lax'

        if all(key in pw_cookie for key in ['name', 'value', 'domain', 'path']):
            playwright_cookies.append(pw_cookie)
        else:
            if logger:
                logger.warning(f"跳过一个格式不完整的 cookie: {cookie}")
            
    return playwright_cookies

def save_cookies_to_file(context, cookie_file_path, original_cookie_names, logger=None):
    """
    从 Playwright 上下文中提取最新的 Cookie，过滤出原始存在的 Cookie，
    转换为 Cookie-Editor 格式，并保存回文件。
    """
    import json
    try:
        current_cookies = context.cookies()
        filtered_cookies = []
        
        for pw_cookie in current_cookies:
            if pw_cookie['name'] in original_cookie_names:
                # 转换为 Cookie-Editor 格式
                editor_cookie = {
                    "domain": pw_cookie.get('domain', ''),
                    "hostOnly": False, # Playwright 不直接提供，默认 False
                    "httpOnly": pw_cookie.get('httpOnly', False),
                    "name": pw_cookie['name'],
                    "path": pw_cookie.get('path', '/'),
                    "secure": pw_cookie.get('secure', False),
                    "session": pw_cookie.get('expires', -1) == -1,
                    "storeId": None,
                    "value": pw_cookie['value']
                }
                
                # 处理过期时间
                if pw_cookie.get('expires', -1) != -1:
                    editor_cookie['expirationDate'] = float(pw_cookie['expires'])
                
                # 处理 sameSite
                same_site = pw_cookie.get('sameSite', '').lower()
                if same_site == 'none':
                    editor_cookie['sameSite'] = 'no_restriction'
                elif same_site in ['lax', 'strict']:
                    editor_cookie['sameSite'] = same_site
                else:
                    editor_cookie['sameSite'] = None
                    
                filtered_cookies.append(editor_cookie)
                
        if filtered_cookies:
            with open(cookie_file_path, 'w', encoding='utf-8') as f:
                json.dump(filtered_cookies, f, indent=4)
            if logger:
                logger.info(f"成功将 {len(filtered_cookies)} 个核心 Cookie 持久化到 {cookie_file_path}")
        else:
            if logger:
                logger.warning(f"未找到任何匹配的原始 Cookie，跳过持久化。")
                
    except Exception as e:
        if logger:
            logger.error(f"保存 Cookie 到文件 {cookie_file_path} 时发生错误: {e}")
