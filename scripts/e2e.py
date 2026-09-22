# -*- coding: utf-8 -*-
"""
QuotaClock v0.2.0 E2E 演练（测试方案 L3；验收 §10 抽样自动化）。

用法:  python scripts/e2e.py <quotaclock 可执行文件路径>
覆盖:  首启模板 / 迁移方案 A / API 契约 / 热生效采集 / revision 判重 /
       双开拒绝 / 多配置并存 / kill -9 stale 接管 / 优雅退出删锁 /
       日志脱敏 / 启动 <1s / 页面出数 <1s
"""
import http.server
import json
import os
import re
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.request

HTTPServerModule = http.server  # 防止下方 http() 请求函数遮蔽模块名

BIN = sys.argv[1] if len(sys.argv) > 1 else None
IS_WIN = os.name == "nt"
BASE = "http://127.0.0.1"
# 当前配置 schema 版本（跟随 internal/config.CurrentVersion；升版时改这里）
CUR_VERSION = 5
FX1_TOKEN = '<script>&"\'\\中文🚀'  # 特殊字符 token（验收 #16 脱敏红线）

results = []


def check(name, ok, detail=""):
    # detail 宽松化：断言点上常直接传 p.get("id") / RPC 返回值，可能是 None。
    # 早先这里直接做字符串拼接，detail=None 时会抛 TypeError 把整轮演练打断 ——
    # 一条断言失败被放大成「后续场景全部没跑」，定位成本远高于它本身。
    results.append((name, ok, detail))
    print(("  PASS  " if ok else "  FAIL  ") + name + ("" if ok else "  -> " + ("<None>" if detail is None else str(detail))))


def free_port():
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    p = s.getsockname()[1]
    s.close()
    return p


def http(method, url, body=None, headers=None, timeout=10):
    req = urllib.request.Request(url, method=method)
    for k, v in (headers or {}).items():
        req.add_header(k, v)
    data = None
    if body is not None:
        data = json.dumps(body).encode()
        req.add_header("Content-Type", "application/json")
    # 连接级异常重试一次：被测服务端若在响应前强关连接（历史上「拒绝路径未读干 body」
    # 的缺陷），客户端会丢响应。修复已在服务端（server.drainBody），这里再兜一层并打告警——
    # 出现告警说明还有连接级问题，别当成静默通过。
    for attempt in (1, 2):
        try:
            with urllib.request.urlopen(req, data=data, timeout=timeout) as resp:
                return resp.status, dict(resp.headers), json.loads(resp.read().decode() or "{}")
        except urllib.error.HTTPError as e:
            raw = e.read().decode()
            try:
                return e.code, dict(e.headers), json.loads(raw or "{}")
            except Exception:
                return e.code, dict(e.headers), {"raw": raw}
        except Exception as e:
            if attempt == 2:
                raise
            print(f"  [WARN] {method} {url} 连接异常，重试一次: {e!r}")


class Instance:
    """一个被演练的 quotaclock 进程，附带 stdout 采集。"""

    def __init__(self, binpath, cfg, port, new_group=False, upstream=None):
        self.port = port
        kw = {}
        if IS_WIN and new_group:
            kw["creationflags"] = subprocess.CREATE_NEW_PROCESS_GROUP
        # v0.2.5：平台地址由代码内置，测试用环境开关把请求 origin 指向 mock（paths 不变）
        env = dict(os.environ)
        if upstream:
            env["QUOTACLOCK_UPSTREAM_OVERRIDE"] = upstream
        self.proc = subprocess.Popen([binpath, "-config", cfg, "-port", str(port)],
                                     stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                     universal_newlines=True, env=env, **kw)
        self.lines = []
        self.stop = threading.Event()
        self.reader = threading.Thread(target=self._read, daemon=True)
        self.reader.start()

    def _read(self):
        for line in self.proc.stdout:
            self.lines.append(line)
            if self.stop.is_set():
                break

    def log(self):
        return "".join(self.lines)

    def kill(self):
        if self.proc.poll() is None:
            self.proc.kill()
        self.proc.wait(timeout=10)

    def wait_ready(self, path="/api/quotas", timeout=10.0):
        url = f"{BASE}:{self.port}{path}"
        t0 = time.time()
        last = None
        while time.time() - t0 < timeout:
            if self.proc.poll() is not None:
                raise RuntimeError(f"进程提前退出 rc={self.proc.returncode}: {self.log()[-400:]}")
            try:
                code, _, body = http("GET", url, timeout=2)
                if code == 200:
                    return body, time.time() - t0
                last = code
            except Exception as e:
                last = e
            time.sleep(0.05)
        raise TimeoutError(f"{url} 未就绪: {last}")


def wait_snapshot(base, cond, timeout=15.0):
    t0 = time.time()
    snap = None
    while time.time() - t0 < timeout:
        _, _, snap = http("GET", base + "/api/quotas")
        if cond(snap):
            return snap
        time.sleep(0.1)
    return snap


class MockUpstream:
    """平台 mock：可切换应答内容，用于采集与 revision 判重演练。"""

    def __init__(self):
        outer = self
        outer.payload = {"success": True, "data": {"percentage": 50}}
        outer.hits = 0     # 成功应答计数（v0.2.4：停用平台不得产生请求）
        outer.delay = 0.0  # 应答前延迟（v0.2.4：制造「缓存数据」观察窗口）

        class H(HTTPServerModule.BaseHTTPRequestHandler):
            def do_GET(self):
                if outer.delay:
                    time.sleep(outer.delay)
                outer.hits += 1
                body = json.dumps(outer.payload).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, *a):
                pass

        class S(HTTPServerModule.ThreadingHTTPServer):
            # 被测进程被 kill / 退避重试时会留下半开的连接，ThreadingHTTPServer 默认
            # 会把这些连接重置打成 "Exception occurred during processing of request"
            # 一样的 traceback —— 属拆除噪音，不是断言失败，但混在结果里会干扰定位。
            # 只吞连接类异常，其他异常照常打印。
            def handle_error(self, request, client_address):
                import sys
                et = sys.exc_info()[0]
                if et in (ConnectionResetError, ConnectionAbortedError, BrokenPipeError, TimeoutError):
                    return
                super().handle_error(request, client_address)

        self.srv = S(("127.0.0.1", 0), H)
        self.port = self.srv.server_address[1]
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()

    def close(self):
        self.srv.shutdown()

    def url(self):
        return f"{BASE}:{self.port}"


def scenario_first_start(binpath, mock):
    print("\n== 场景 1：首启模板 / 默认密码检测 / 静态页 ==")
    d = tempfile.mkdtemp(prefix="qc-e2e-")
    port = free_port()
    cfg = os.path.join(d, "config.json")
    inst = Instance(binpath, cfg, port, upstream=mock.url())
    snap, boot = inst.wait_ready()
    check("首启就绪（观测 %.2fs）" % boot, boot < 10)
    check("空配置快照 providers 空", snap["providers"] == [])
    with open(cfg, encoding="utf-8") as f:
        created = json.load(f)
    check("模板 version=%d" % CUR_VERSION, created["version"] == CUR_VERSION)
    check("模板 auth=admin + providers 空", created["auth"]["mode"] == "admin" and created["providers"] == [])
    locks = [x for x in os.listdir(d) if x.startswith("quotaclock-") and x.endswith(".lock")]
    check("锁文件已创建", len(locks) == 1)
    req = urllib.request.urlopen(f"{BASE}:{port}/", timeout=5)
    page = req.read().decode()
    check("GET / 出静态页", "QuotaClock" in page and "text/html" in req.headers["Content-Type"])
    code, _, view = http("GET", f"{BASE}:{port}/api/config")
    s = json.dumps(view)
    check("GET config 200", code == 200)
    check("config providers 为数组 []（非 null，添加平台依赖）", view["providers"] == [])
    plats = [p["id"] for p in view.get("platforms", [])]
    check("内置平台目录 4 项（前端下拉源）",
          plats == ["zhipu-glm", "deepseek", "kimi-code", "opencode"], json.dumps(plats))
    check("password_is_default=true（默认密码未改）", view["auth"]["password_is_default"] is True)
    check("无 token/token_cipher 字段", "token_cipher" not in s and '"token":' not in s)
    out = subprocess.run([binpath, "-version"], capture_output=True, text=True)
    check("-version 输出版本", "QuotaClock" in out.stdout)
    return d, port, cfg, inst


def scenario_api_chain(d, port, cfg, inst, mock):
    print("\n== 场景 2：登录 / PUT 配置 / 热生效采集 / 判重 ==")
    base = f"{BASE}:{port}"
    try:
        _scenario_api_chain(d, port, cfg, inst, mock)
    except Exception:
        # 失败时把被测进程日志打出来（否则只剩客户端异常，无法定位）
        print("--- 被测进程日志（尾部）---")
        print(inst.log()[-1500:])
        raise


def _scenario_api_chain(d, port, cfg, inst, mock):
    base = f"{BASE}:{port}"
    code, hdr, body = http("POST", base + "/api/login", {"password": "wrong-pass"})
    check("错误密码 401 INVALID_CREDENTIALS", code == 401 and body["error"]["code"] == "INVALID_CREDENTIALS")
    code, hdr, body = http("POST", base + "/api/login", {"password": "Quota@2026090S"})
    setc = hdr.get("Set-Cookie", "")
    check("正确密码 200 + Set-Cookie", code == 200 and "qc_session=" in setc)
    check("cookie 属性 HttpOnly+Lax", "HttpOnly" in setc and "SameSite=Lax" in setc)
    cookie = re.search(r"qc_session=([^;]+)", setc).group(1)
    auth = {"Cookie": "qc_session=" + cookie}

    code, _, _ = http("PUT", base + "/api/config", {"version": 4})
    check("未登录 PUT 401", code == 401)

    put = {
        "version": 4,
        "listen": {"host": "127.0.0.1", "port": port},
        "collector": {"interval_base_s": 30, "jitter_min_s": 5, "jitter_max_s": 25,
                      "stagger_min_s": 1, "stagger_max_s": 5, "backoff_multiplier": 2, "backoff_max_s": 1800},
        "auth": {"mode": "admin"},
        "providers": [{"platform": "opencode",
                       "access_keys": [{"id": "mockk", "name": "Mock Key", "token": "sk-e2e-token-abcdef"}]}],
    }
    code, _, body = http("PUT", base + "/api/config", put, auth)
    check("PUT 配置 200 ok", code == 200 and body.get("ok") is True)
    with open(cfg, encoding="utf-8") as f:
        saved = json.load(f)
    check("config.json 无明文 token", "sk-e2e-token-abcdef" not in json.dumps(saved))
    check("config.json token_cipher 已落盘", "token_cipher" in json.dumps(saved))
    check("落盘不含 base_url/paths（地址由预设派生）",
          "base_url" not in json.dumps(saved) and '"paths"' not in json.dumps(saved))

    snap = wait_snapshot(base, lambda s: (s["providers"] or [{}])[0].get("status") == "ok", timeout=30)
    p = (snap["providers"] or [{}])[0]
    check("热生效后采集器拉到数据 status=ok", p.get("status") == "ok", json.dumps(p)[:200])
    check("运行时 ID = <平台>.<凭据>", p.get("id") == "opencode.mockk", p.get("id"))
    check("展示名 = 平台 · 凭据", p.get("name") == "OpenCode · Mock Key", p.get("name"))
    check("data 透传 percentage", (p.get("data") or {}).get("data", {}).get("percentage") == 50)
    check("last_success_at 本地时区（+08:00 而非 Z）",
          bool(re.match(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}[+-]\d{2}:\d{2}$", p.get("last_success_at") or "")),
          p.get("last_success_at"))

    t0 = time.time()
    http("GET", base + "/api/quotas")
    dt = time.time() - t0
    check("GET /api/quotas <1s（%.0fms）" % (dt * 1000), dt < 1.0)

    # 判重：数据变化 → revision +1（下一采集 tick = 30s 基准 + 5~25s 抖动）；无变化 → revision 不动
    rev1 = snap["revision"]
    mock.payload = {"success": True, "data": {"percentage": 60}}
    snap2 = wait_snapshot(base, lambda s: s["revision"] > rev1 and (s["providers"] or [{}])[0].get("data", {}).get("data", {}).get("percentage") == 60, timeout=70)
    check("data 变化 revision +1", snap2["revision"] == rev1 + 1, f"{rev1}→{snap2['revision']}")
    rev2 = snap2["revision"]
    snap3 = wait_snapshot(base, lambda s: s["revision"] != rev2, timeout=3.0)
    check("无变化 revision 不动", snap3 is not None and snap3["revision"] == rev2)

    code, _, view = http("GET", base + "/api/config", None, auth)
    pv = view["providers"][0]
    check("视图含平台目录名与凭据列表", pv["platform_name"] == "OpenCode" and len(pv["access_keys"]) == 1)
    check("登录态 token_masked", pv["access_keys"][0].get("token_masked") == "sk****ef",
          json.dumps(pv["access_keys"][0]))

    code, _, _ = http("POST", base + "/api/logout", None, auth)
    check("logout 200", code == 200)
    code, _, view2 = http("GET", base + "/api/config")
    check("登出后无 token_masked", "token_masked" not in json.dumps(view2))
    code, _, _ = http("PUT", base + "/api/config", put, auth)
    check("登出后旧会话 PUT 401（拒绝名单）", code == 401)

    # 特殊字符 token（FX-1）落配置 → 之后校验日志脱敏
    code, hdr, _ = http("POST", base + "/api/login", {"password": "Quota@2026090S"})
    cookie2 = re.search(r"qc_session=([^;]+)", hdr.get("Set-Cookie", "")).group(1)
    put["providers"][0]["access_keys"][0]["token"] = FX1_TOKEN
    code, _, _ = http("PUT", base + "/api/config", put, {"Cookie": "qc_session=" + cookie2})
    check("特殊字符 token PUT 成功", code == 200)
    time.sleep(0.3)  # 留给采集失败 WARN 日志（mock 改 401 验证失败态传播）
    return cookie2


def scenario_log_redaction(inst, cfg):
    print("\n== 场景 2b：日志脱敏红线（FX-1 特殊字符 token） ==")
    with open(cfg, encoding="utf-8") as f:
        saved = f.read()
    log = inst.log()
    fragments = [FX1_TOKEN, "中文🚀", "qc_session="]
    ok = True
    hit = ""
    for frag in fragments:
        if frag in log:
            ok = False
            hit = frag
    check("日志无 token/掩码/会话 cookie 字面量", ok and '"token_cipher"' not in log, f"命中: {hit}" if hit else "")
    # 状态快照失败态验证（mock 已改 60% 正常应答，无失败日志是正常路径）
    check("config.json 含密文不含明文", '"token_cipher"' in saved and FX1_TOKEN not in saved)


def scenario_migration(binpath, mock):
    print("\n== 场景 3：v0.1 旁路迁移（方案 A 自动改写 → 平台化） ==")
    d = tempfile.mkdtemp(prefix="qc-e2e-mig-")
    port = free_port()
    cfg = os.path.join(d, "config.json")
    v1 = {
        "version": 2, "active": "deepseek",
        "providers": [
            {"enabled": True, "id": "zhipu", "name": "智谱 GLM", "baseURL": "https://open.bigmodel.cn",
             "token": "fake-zhipu-token-aaaa", "endpoints": [
                 {"method": "GET", "path": "/api/monitor/usage/quota/limit", "params": "{", "note": "x"}]},
            {"enabled": True, "id": "deepseek", "name": "DeepSeek", "baseURL": "https://api.deepseek.com",
             "token": "sk-fake-bbbb", "endpoints": [{"method": "GET", "path": "/user/balance"}]},
            {"enabled": True, "id": "moonshot", "name": "Kimi Code", "baseURL": "http://127.0.0.1:8787/kimi",
             "token": "sk-fake-kimi", "endpoints": [
                 {"method": "GET", "path": "/usages"}, {"method": "GET", "path": "/me"}, {"method": "GET", "path": "/models"}]},
            {"enabled": True, "id": "opencode", "name": "OpenCode Go", "baseURL": "http://127.0.0.1:8787/opencode",
             "token": "auth=Fe26.2**fake; oc_locale=zh", "endpoints": [
                 {"method": "GET", "path": "/_server?id=deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef&args=%7B%22t%22%3A9%7D"}]},
            # 同一平台的第二个凭据且停用：覆盖「enabled=false 导入并保持停用」+「同平台归并」
            {"enabled": False, "id": "deepseek-key2", "name": "DeepSeek 备用", "baseURL": "https://api.deepseek.com",
             "token": "sk-fake-d2", "endpoints": [{"method": "GET", "path": "/user/balance"}]},
            # 未知平台：v0.2.5 不再允许自建平台 → 跳过 + WARN
            {"enabled": True, "id": "custom", "name": "自定义代理", "baseURL": "https://my-proxy.example.com",
             "token": "sk-fake-c", "endpoints": [{"method": "GET", "path": "/x"}]},
        ],
    }
    with open(cfg, "w", encoding="utf-8") as f:
        json.dump(v1, f, ensure_ascii=False)
    inst = Instance(binpath, cfg, port, upstream=mock.url())
    try:
        inst.wait_ready()
        with open(cfg, encoding="utf-8") as f:
            mig = json.load(f)
        s = json.dumps(mig)
        check("迁移升版 version=%d" % CUR_VERSION, mig["version"] == CUR_VERSION)
        check("迁移后无明文 token", not any(x in s for x in
              ["fake-zhipu-token", "sk-fake-bbbb", "sk-fake-kimi", "Fe26.2**fake", "sk-fake-d2", "sk-fake-c"]))
        byplat = {p["platform"]: p for p in mig["providers"]}
        check("归并为 4 个平台（未知平台条目被跳过）",
              sorted(byplat) == ["deepseek", "kimi-code", "opencode", "zhipu-glm"], json.dumps(sorted(byplat)))
        check("未知平台跳过 WARN", "不在已知平台清单" in inst.log())
        ds = byplat["deepseek"]
        kk = {k["id"]: k for k in ds["access_keys"]}
        check("同平台归并 2 个凭据", len(ds["access_keys"]) == 2, json.dumps(ds["access_keys"]))
        check("enabled=false 导入并保持停用", kk["deepseek-key2"].get("enabled") is False)
        check("停用凭据 token 仍加密落盘（不丢配置）", bool(kk["deepseek-key2"].get("token_cipher")))
        check("停用导入 INFO 日志", "保持停用" in inst.log())
        check("凭据名剥平台前缀派生（DeepSeek 备用 → 备用）", kk["deepseek-key2"].get("name") == "备用",
              json.dumps(kk["deepseek-key2"]))
        check("kimi 路径收敛为预设 /usages", "收敛" in inst.log() and "/me" in inst.log())
        check("opencode token 未迁移（Cookie 无法转 API Key）",
              not byplat["opencode"]["access_keys"][0].get("token_cipher")
              and "无法转换为 API Key" in inst.log())
        check("落盘不再含 base_url/paths/auth_style",
              not any(x in s for x in ["base_url", '"paths"', "auth_style"]))
        log = inst.log()
        check("迁移 INFO 对照日志", "迁移改写" in log and "api.kimi.com/coding/v1" in log and "zen/go/v1" in log)
        check("迁移后为默认密码态（红 banner 条件）", "正在使用默认密码" in log)

        # 迁移前备份（v0.2.5）：原文件整份留档，命名带旧版本号
        bak = cfg + ".v2.bak"
        check("迁移前已备份原配置 config.json.v2.bak", os.path.exists(bak), json.dumps(os.listdir(d)))
        check("备份日志可见", "已备份迁移前的配置" in log)
        with open(bak, encoding="utf-8") as f:
            bakdoc = json.load(f)
        check("备份内容 = 迁移前的原文件（version 2）", bakdoc["version"] == 2 and len(bakdoc["providers"]) == 6)
        check("v0.1 备份含明文 token → 有删除提示 WARN",
              "明文 token" in log and "请删除" in log)
        check("主配置已升版（备份不影响主文件）", mig["version"] == CUR_VERSION)
    finally:
        inst.kill()
    return d


def scenario_migrate_v3(binpath, mock):
    """v0.2.x（version 3）→ 当前版本：最常见的升级路径。

    密文必须是真的（能解）才能验证「采集照常」——故先用被测程序自己 PUT 一份带 token 的 v4 配置
    拿到密文，再把文件降级改写成 v3 形态（一 key 一 provider），重启验证迁移。
    """
    print("\n== 场景 8：v0.2.x → v0.2.5 配置迁移（一 key 一平台 → 一平台多凭据） ==")
    d = tempfile.mkdtemp(prefix="qc-e2e-v3-")
    port = free_port()
    cfg = os.path.join(d, "config.json")
    base = base_of(port)

    # ① 借程序自身产出真密文
    inst0 = Instance(binpath, cfg, port, upstream=mock.url())
    try:
        inst0.wait_ready()
        _, hdr, _ = http("POST", base + "/api/login", {"password": "Quota@2026090S"})
        cookie = re.search(r"qc_session=([^;]+)", hdr.get("Set-Cookie", "")).group(1)
        auth = {"Cookie": "qc_session=" + cookie}
        seed = {
            "version": 4,
            "listen": {"host": "127.0.0.1", "port": port},
            "collector": {"interval_base_s": 30, "jitter_min_s": 5, "jitter_max_s": 25,
                          "stagger_min_s": 1, "stagger_max_s": 5, "backoff_multiplier": 2, "backoff_max_s": 1800},
            "auth": {"mode": "admin"},
            "providers": [{"platform": "opencode",
                           "access_keys": [{"id": "seed", "name": "seed", "token": "sk-v3-migrate-token"}]}],
        }
        code, _, _ = http("PUT", base + "/api/config", seed, auth)
        check("准备阶段：PUT 成功（用于产出真密文）", code == 200)
    finally:
        inst0.kill()
    with open(cfg, encoding="utf-8") as f:
        seed_doc = json.load(f)
    cipher = seed_doc["providers"][0]["access_keys"][0]["token_cipher"]
    hash_before = seed_doc["auth"]["password_hash"]

    # ② 降级改写成 v3 形态（同平台两条：一条启用、一条停用）
    v3 = {
        "version": 3,
        "listen": {"host": "127.0.0.1", "port": port},
        "collector": seed_doc["collector"],
        "auth": seed_doc["auth"],
        "providers": [
            {"id": "opencode-m1", "name": "OpenCode M1", "base_url": "https://opencode.ai/zen/go/v1",
             "paths": ["/usage"], "auth_style": "bearer", "extra_headers": {}, "token_cipher": cipher},
            {"id": "opencode-m2", "name": "OpenCode M2", "base_url": "https://opencode.ai/zen/go/v1",
             "paths": ["/usage"], "auth_style": "bearer", "extra_headers": {}, "token_cipher": cipher,
             "enabled": False},
        ],
    }
    with open(cfg, "w", encoding="utf-8") as f:
        json.dump(v3, f, ensure_ascii=False)

    # ③ 重启 → 迁移
    inst = Instance(binpath, cfg, port, upstream=mock.url())
    try:
        inst.wait_ready()
        with open(cfg, encoding="utf-8") as f:
            mig = json.load(f)
        check("v3 → 当前版本已升版", mig["version"] == CUR_VERSION)
        check("listen/collector 原样保留",
              mig["listen"]["host"] == "127.0.0.1" and mig["listen"]["port"] == port
              and mig["collector"]["interval_base_s"] == 30)
        check("密码 hash 原样保留（不重置）", mig["auth"]["password_hash"] == hash_before)
        byplat = {p["platform"]: p for p in mig["providers"]}
        check("归并为 1 个平台（opencode）", sorted(byplat) == ["opencode"], json.dumps(sorted(byplat)))
        oc = byplat["opencode"]
        keys = {k["id"]: k for k in oc["access_keys"]}
        check("同平台两条合一（2 个凭据）", len(oc["access_keys"]) == 2, json.dumps(oc["access_keys"]))
        check("凭据密文原样保留（不再二次加密）",
              keys["opencode-m1"]["token_cipher"] == cipher and keys["opencode-m2"]["token_cipher"] == cipher)
        check("enabled=false 保持停用", keys["opencode-m2"].get("enabled") is False)
        check("凭据名剥平台前缀（M1/M2）",
              keys["opencode-m1"].get("name") == "M1" and keys["opencode-m2"].get("name") == "M2")
        check("归并 INFO 日志", "归并" in inst.log())
        check("落盘不再含 base_url/paths/auth_style",
              not any(x in json.dumps(mig) for x in ["base_url", '"paths"', "auth_style"]))
        # 采集：迁移后的启用凭据照常出数（证明密文与预设地址都接得住）
        snap = wait_snapshot(base, lambda s: {p["id"]: p for p in s["providers"]}.get(
            "opencode.opencode-m1", {}).get("status") == "ok", timeout=30)
        sp = {p["id"]: p for p in snap["providers"]}
        check("迁移后启用凭据采集成功（密文可解 + 预设地址生效）",
              sp["opencode.opencode-m1"]["status"] == "ok", json.dumps(sp.get("opencode.opencode-m1"))[:200])
        check("迁移后停用凭据为 disabled", sp["opencode.opencode-m2"]["status"] == "disabled",
              json.dumps(sp.get("opencode.opencode-m2"))[:200])

        # 迁移前备份（v0.2.5）：v0.2.x 的配置文件整份留档，可用于人工回退
        bak = cfg + ".v3.bak"
        check("迁移前已备份原配置 config.json.v3.bak", os.path.exists(bak), json.dumps(os.listdir(d)))
        with open(bak, encoding="utf-8") as f:
            bakdoc = json.load(f)
        check("备份 = 迁移前原文件（version 3 + 一 key 一 provider 形态）",
              bakdoc["version"] == 3 and [p["id"] for p in bakdoc["providers"]] == ["opencode-m1", "opencode-m2"])
        check("备份内保留原始字段（base_url/paths 仍可查）",
              bakdoc["providers"][0]["base_url"] == "https://opencode.ai/zen/go/v1"
              and bakdoc["providers"][0]["paths"] == ["/usage"])
        check("备份与主文件互不影响", mig["version"] == CUR_VERSION and bakdoc["version"] == 3)
    finally:
        inst.kill()
    return d


def base_of(port):
    return f"{BASE}:{port}"


def scenario_lock(binpath, d, port, cfg, inst):
    print("\n== 场景 4：双开拒绝 / stale 接管 / 多配置并存 ==")
    # 同配置双开 → 第二个退出并提示 PID
    p2 = Instance(binpath, cfg, free_port())
    rc = p2.proc.wait(timeout=15)
    out2 = p2.log()
    check("同配置双开 exit 1", rc == 1, f"rc={rc}")
    check("提示已在运行 (PID)", "已在运行 (PID" in out2, out2[-200:])

    # 不同配置并存
    cfg2 = os.path.join(d, "config2.json")
    port2 = free_port()
    p3 = Instance(binpath, cfg2, port2)
    try:
        p3.wait_ready()
        check("同目录两套配置并存", True)
    finally:
        p3.kill()

    # kill -9 → stale 接管
    pid = inst.proc.pid
    if IS_WIN:
        subprocess.run(["taskkill", "/F", "/PID", str(pid)], capture_output=True)
    else:
        os.kill(pid, signal.SIGKILL)
    inst.proc.wait(timeout=10)
    p4 = Instance(binpath, cfg, port)
    try:
        p4.wait_ready()
        check("kill -9 后重启 stale 接管", True)
    finally:
        p4.kill()


import hashlib


def lock_name(cfg):
    return "quotaclock-" + hashlib.sha256(os.path.abspath(cfg).encode()).hexdigest()[:16] + ".lock"


def scenario_graceful(binpath, d, port, cfg):
    print("\n== 场景 5：优雅退出（锁删除 + 退出码 0 + <26s） ==")
    if IS_WIN:
        # CTRL_BREAK → Go os.Interrupt；子进程须为独立进程组
        proc = subprocess.Popen([binpath, "-config", cfg, "-port", str(port)],
                                stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                universal_newlines=True,
                                creationflags=subprocess.CREATE_NEW_PROCESS_GROUP)
    else:
        proc = subprocess.Popen([binpath, "-config", cfg, "-port", str(port)],
                                stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                universal_newlines=True)
    inst = Instance.__new__(Instance)
    inst.proc, inst.lines, inst.stop = proc, [], threading.Event()
    inst.reader = threading.Thread(target=inst._read, args=(), daemon=True)
    inst.reader.start()
    inst.port = port
    inst.wait_ready()
    t0 = time.time()
    if IS_WIN:
        os.kill(proc.pid, signal.CTRL_BREAK_EVENT)
    else:
        os.kill(proc.pid, signal.SIGTERM)
    rc = proc.wait(timeout=30)
    dt = time.time() - t0
    gone = not os.path.exists(os.path.join(d, lock_name(cfg)))
    check("优雅退出 exit 0", rc == 0, f"rc={rc}")
    check("退出时长 <26s（%.1fs）" % dt, dt < 26)
    check("锁文件已删除", gone)


def scenario_enabled_and_cache(binpath, d, port, cfg, mock):
    """v0.2.4/v0.2.5：凭据级 enabled 停用（停采 + disabled 态）、同平台多 Key、cache.json 重启恢复。"""
    print("\n== 场景 7：多 Key 凭据级 enabled / cache.json 上次成功数据 ==")
    base = f"{BASE}:{port}"
    inst = Instance(binpath, cfg, port, upstream=mock.url())
    try:
        inst.wait_ready()
        code, hdr, body = http("POST", base + "/api/login", {"password": "Quota@2026090S"})
        cookie = re.search(r"qc_session=([^;]+)", hdr.get("Set-Cookie", "")).group(1)
        auth = {"Cookie": "qc_session=" + cookie}

        put = {
            "version": 4,
            "listen": {"host": "127.0.0.1", "port": port},
            "collector": {"interval_base_s": 30, "jitter_min_s": 5, "jitter_max_s": 25,
                          "stagger_min_s": 1, "stagger_max_s": 5, "backoff_multiplier": 2, "backoff_max_s": 1800},
            "auth": {"mode": "admin"},
            "providers": [
                {"platform": "opencode", "access_keys": [
                    {"id": "on", "name": "启用 Key", "token": "sk-fake-on-token"},      # enabled 缺省 = 启用
                    {"id": "off", "name": "停用 Key", "enabled": False, "token": "sk-fake-off-token"},
                ]},
            ],
        }
        code, _, _ = http("PUT", base + "/api/config", put, auth)
        check("PUT（同平台 1 启用 + 1 停用）200", code == 200)

        code, _, view = http("GET", base + "/api/config", None, auth)
        keys = {k["id"]: k for k in view["providers"][0]["access_keys"]}
        check("视图凭据 enabled 往返（缺省 true / 显式 false）",
              keys["on"]["enabled"] is True and keys["off"]["enabled"] is False, json.dumps(keys))
        check("视图凭据名与 has_token", keys["on"]["name"] == "启用 Key" and keys["off"]["has_token"] is True)
        with open(cfg, encoding="utf-8") as f:
            raw = f.read()
        check("停用凭据落盘 enabled: false", '"enabled": false' in raw)

        # 两个 key 同平台：on 立即采集（偏移 0），off 停用不发请求
        snap = wait_snapshot(base, lambda s: {p["id"]: p for p in s["providers"]}.get(
            "opencode.on", {}).get("status") == "ok", timeout=30)
        sp = {p["id"]: p for p in snap["providers"]}
        check("运行时 ID = 平台.凭据（多条）",
              sorted(sp) == ["opencode.off", "opencode.on"], json.dumps(sorted(sp)))
        check("停用凭据快照 status=disabled/DISABLED",
              sp["opencode.off"]["status"] == "disabled"
              and (sp["opencode.off"].get("error") or {}).get("code") == "DISABLED",
              json.dumps(sp.get("opencode.off"))[:200])
        check("启用凭据正常采集", sp["opencode.on"]["status"] == "ok")
        hits_before = mock.hits
        sleep_until = time.time() + 5
        while time.time() < sleep_until:
            time.sleep(0.25)
        check("停用凭据不发请求（5s 内上游计数不变）", mock.hits == hits_before,
              f"{hits_before}→{mock.hits}")

        # v0.2.5 r4.1：只改 API Key 的名称不该触发重新采集（名称是纯展示字段）
        on_last = sp["opencode.on"]["last_success_at"]
        rename_put = json.loads(json.dumps(put))
        rename_put["providers"][0]["access_keys"][0]["name"] = "改名后的 Key"
        for k in rename_put["providers"][0]["access_keys"]:
            k.pop("token", None)  # 改名时用户不会重填 token（保留原密文，等价于真实操作）
        hits_before = mock.hits
        code, _, _ = http("PUT", base + "/api/config", rename_put, auth)
        check("只改名称的 PUT 200", code == 200)
        snap = wait_snapshot(base, lambda s: {p["id"]: p for p in s["providers"]}.get(
            "opencode.on", {}).get("name") == "OpenCode · 改名后的 Key", timeout=10)
        sp2 = {p["id"]: p for p in snap["providers"]}
        check("改名后快照立即换名", sp2["opencode.on"]["name"] == "OpenCode · 改名后的 Key",
              json.dumps(sp2["opencode.on"])[:160])
        check("改名不改采集状态（仍 ok / 不进等待采集）",
              sp2["opencode.on"]["status"] == "ok", json.dumps(sp2["opencode.on"])[:160])
        check("改名不动 last_success_at", sp2["opencode.on"]["last_success_at"] == on_last,
              f"{on_last} → {sp2['opencode.on']['last_success_at']}")
        t0 = time.time()
        while time.time() - t0 < 5:
            time.sleep(0.25)
        check("只改名称不触发重新采集（5s 内上游计数不变）", mock.hits == hits_before,
              f"{hits_before}→{mock.hits}")

        # cache.json：白名单 + 内容（只含启用凭据的成功数据、无 token）
        cache_path = os.path.join(d, "cache.json")
        t0 = time.time()
        while time.time() - t0 < 5 and not os.path.exists(cache_path):
            time.sleep(0.1)
        check("cache.json 已落盘", os.path.exists(cache_path))
        with open(cache_path, encoding="utf-8") as f:
            cache = json.load(f)
        cids = [e["id"] for e in cache["providers"]]
        check("缓存只含成功凭据（停用凭据不入档）", cids == ["opencode.on"], json.dumps(cids))
        check("缓存无 token/密文", "token" not in json.dumps(cache) and "cipher" not in json.dumps(cache))
        cached_at = cache["providers"][0]["last_success_at"]
        files = sorted(os.listdir(d))
        allowed = all(re.fullmatch(r"config\d*\.json", x) is not None
                      or x in ("key.bin", "cache.json", "trends.json")  # trends.json：v0.4.0 用量趋势
                      or (x.startswith("quotaclock-") and x.endswith(".lock"))
                      or re.fullmatch(r"config\d*\.json\.v\d+(-\d+)?\.bak", x) is not None  # 迁移前备份（v0.2.5）
                      or x.endswith(".tmp") for x in files)
        check("写盘白名单：目录内仅运行时白名单文件（含 cache.json / trends.json）", allowed, json.dumps(files))

        # 重启：首屏应为 cached（上游故意慢），首轮采集后转 ok
        inst.kill()
        mock.delay = 8.0
        inst2 = Instance(binpath, cfg, port, upstream=mock.url())
        try:
            inst2.wait_ready()
            _, _, s0 = http("GET", base + "/api/quotas")
            p0 = {p["id"]: p for p in s0["providers"]}
            check("重启首屏 status=cached（不回到等待首次采集）",
                  p0["opencode.on"]["status"] == "cached", json.dumps(p0.get("opencode.on"))[:200])
            check("缓存态保留原 last_success_at", p0["opencode.on"]["last_success_at"] == cached_at,
                  f"{cached_at} vs {p0['opencode.on']['last_success_at']}")
            check("停用凭据重启后仍为 disabled", p0["opencode.off"]["status"] == "disabled")
            s1 = wait_snapshot(base, lambda s: all(p.get("status") != "cached" for p in s["providers"]), timeout=30)
            p1 = {p["id"]: p for p in s1["providers"]}
            check("首轮采集成功后转 ok", p1["opencode.on"]["status"] == "ok",
                  json.dumps(p1.get("opencode.on"))[:200])
        finally:
            inst2.kill()
        mock.delay = 0.0

        # 启用原停用凭据 → 进入采集（同平台第二个 key 的槽位在 +15s，故给足超时）
        inst3 = Instance(binpath, cfg, port, upstream=mock.url())
        try:
            inst3.wait_ready()
            put["providers"][0]["access_keys"][1]["enabled"] = True
            code, _, _ = http("PUT", base + "/api/config", put, auth)
            check("勾选启用 PUT 200", code == 200)
            h0 = mock.hits
            s2 = wait_snapshot(base, lambda s: all(p.get("status") == "ok" for p in s["providers"]), timeout=60)
            p2 = {p["id"]: p for p in s2["providers"]}
            check("重新启用后两条凭据均 ok（同平台等分错峰）",
                  p2["opencode.on"]["status"] == "ok" and p2["opencode.off"]["status"] == "ok",
                  json.dumps({k: v["status"] for k, v in p2.items()}))
            check("上游确实收到新凭据的请求", mock.hits > h0, f"{h0}→{mock.hits}")
        finally:
            inst3.kill()
    finally:
        inst.kill()  # 幂等：中途异常时确保首实例被回收


def scenario_startup_time(binpath):
    print("\n== 场景 6：非功能：启动 <1s ==")
    d = tempfile.mkdtemp(prefix="qc-e2e-boot-")
    port = free_port()
    cfg = os.path.join(d, "config.json")
    inst = Instance(binpath, cfg, port)
    try:
        _, boot = inst.wait_ready(timeout=10)
        check("启动到出数 <1.5s（%.0fms，含进程拉起）" % (boot * 1000), boot < 1.5)
    finally:
        inst.kill()


def scenario_v030(binpath, mock):
    """v0.2.5（version 4）→ v0.3.0（version 5）迁移 + 配置导出/导入契约。

    v0.3.0 新增三件事要在这里钉住：
      1. schema 4 → 5：只多 balance 段（余额展示设置），迁移前必须留 .v4.bak；
      2. GET /api/config/export：需登录、no-store、**含明文 token**（跨机器搬运的唯一可行形态）、
         且导出的文件能原样回灌 PUT（`_note` 被容忍）——这是「导入后只改部分 Key」的地基；
      3. balance 经完整链路落盘（回归：mergeForSave 漏字段会静默写回 0）。
    """
    print("\n== 场景 9：v4 → v5 迁移（余额设置）+ 配置导出/导入（v0.3.0） ==")
    d = tempfile.mkdtemp(prefix="qc-e2e-v4-")
    port = free_port()
    cfg = os.path.join(d, "config.json")
    base = base_of(port)
    seed_token = "sk-e2e-v4-seed-T0KEN"

    # ① 先用程序自身产出一份 v5 配置（拿到真密文），再降级改写成 v4（去掉 balance 段）
    inst0 = Instance(binpath, cfg, port, upstream=mock.url())
    try:
        inst0.wait_ready()
        _, hdr, _ = http("POST", base + "/api/login", {"password": "Quota@2026090S"})
        cookie = re.search(r"qc_session=([^;]+)", hdr.get("Set-Cookie", "")).group(1)
        auth = {"Cookie": "qc_session=" + cookie}
        seed = {
            "version": 5,
            "listen": {"host": "127.0.0.1", "port": port},
            "collector": {"interval_base_s": 30, "jitter_min_s": 5, "jitter_max_s": 25,
                          "stagger_min_s": 1, "stagger_max_s": 5, "backoff_multiplier": 2, "backoff_max_s": 1800},
            "balance": {"full": 100, "green_pct": 50, "warn_pct": 20},
            "auth": {"mode": "admin"},
            "providers": [{"platform": "opencode", "access_keys": [
                {"id": "seed", "name": "seed", "token": seed_token}]}],
        }
        code, _, _ = http("PUT", base + "/api/config", seed, auth)
        check("准备阶段：PUT v5 成功（产出真密文）", code == 200)
    finally:
        inst0.kill()
    with open(cfg, encoding="utf-8") as f:
        doc = json.load(f)
    cipher = doc["providers"][0]["access_keys"][0]["token_cipher"]
    hash_before = doc["auth"]["password_hash"]

    v4 = {k: v for k, v in doc.items() if k != "balance"}
    v4["version"] = 4
    with open(cfg, "w", encoding="utf-8") as f:
        json.dump(v4, f, ensure_ascii=False)

    # ② 重启 → v4 迁移到 v5
    inst = Instance(binpath, cfg, port, upstream=mock.url())
    try:
        inst.wait_ready()
        with open(cfg, encoding="utf-8") as f:
            mig = json.load(f)
        check("v4 → v5 已升版", mig["version"] == CUR_VERSION)
        check("balance 补默认（满额 100 / 绿 50 / 黄 20）",
              mig.get("balance") == {"full": 100, "green_pct": 50, "warn_pct": 20}, json.dumps(mig.get("balance")))
        check("其余字段原样保留（listen/collector/密码 hash）",
              mig["listen"]["port"] == port and mig["collector"]["interval_base_s"] == 30
              and mig["auth"]["password_hash"] == hash_before)
        check("凭据密文原样保留（不重加密）",
              mig["providers"][0]["access_keys"][0]["token_cipher"] == cipher)
        bak = cfg + ".v4.bak"
        ok_bak = os.path.exists(bak)
        if ok_bak:
            with open(bak, encoding="utf-8") as f:
                ok_bak = json.load(f)["version"] == 4
        check("迁移前留 config.json.v4.bak（内容为 v4）", ok_bak)
        check("迁移 INFO 日志（含余额设置提示）", "v4" in inst.log() and "余额" in inst.log())
        snap = wait_snapshot(base, lambda s: {p["id"]: p for p in s["providers"]}.get(
            "opencode.seed", {}).get("status") == "ok", timeout=30)
        sp = {p["id"]: p for p in snap["providers"]}
        check("迁移后采集照常（密文可解）", sp["opencode.seed"]["status"] == "ok",
              json.dumps(sp.get("opencode.seed"))[:200])

        # ③ 导出契约
        code, _, _ = http("GET", base + "/api/config/export")
        check("导出：未登录被拒（401）", code == 401, str(code))
        code, hdr, ex = http("GET", base + "/api/config/export", None, auth)
        check("导出：登录后 200", code == 200)
        check("导出：no-store 且带附件文件名", hdr.get("Cache-Control") == "no-store"
              and "attachment" in (hdr.get("Content-Disposition") or "")
              and "quotaclock-config-" in (hdr.get("Content-Disposition") or ""),
              json.dumps({k: v for k, v in hdr.items() if k.lower().startswith(("cache", "content-disp"))}))
        check("导出：含 _note / version / balance",
              "_note" in ex and ex.get("version") == 5 and ex.get("balance", {}).get("full") == 100)
        tokens = [k.get("token") for p in ex.get("providers", []) for k in p.get("access_keys", [])]
        check("导出：token 为明文", seed_token in tokens, json.dumps(tokens)[:120])
        check("导出：不含密文与密码 hash",
              "token_cipher" not in json.dumps(ex) and "password_hash" not in json.dumps(ex))
        check("导出：enabled 显式给布尔（导入方不靠默认值）",
              all(isinstance(k.get("enabled"), bool) for p in ex["providers"] for k in p["access_keys"]))

        # ④ 导出文件原样回灌（导入的地基：PUT 必须容忍 _note 这类附带字段）
        back = {k: v for k, v in ex.items() if k != "_note"}
        back["balance"] = {"full": 150, "green_pct": 60, "warn_pct": 30}
        code, _, resp = http("PUT", base + "/api/config", back, auth)
        check("导出文件能直接回灌 PUT（_note 被容忍）", code == 200, json.dumps(resp)[:200])

        # ⑤ 余额落盘 + 视图一致（mergeForSave 回归）
        with open(cfg, encoding="utf-8") as f:
            after = json.load(f)
        check("balance 落盘为 150/60/30",
              after.get("balance") == {"full": 150, "green_pct": 60, "warn_pct": 30}, json.dumps(after.get("balance")))
        _, _, view = http("GET", base + "/api/config")
        check("GET /api/config 视图与落盘一致", view.get("balance", {}).get("full") == 150, json.dumps(view.get("balance")))
        # 注意：不能断言密文不变 —— 同一明文重新加密会得到不同密文（AES-GCM 随机 nonce，属正常）。
        # 该验的是「明文没被写坏」：再导一次，token 仍是原值，且密文确实换了一份。
        _, _, ex2 = http("GET", base + "/api/config/export", None, auth)
        toks2 = [k.get("token") for p in ex2.get("providers", []) for k in p.get("access_keys", [])]
        check("回灌后 token 明文未变（重新导出核对）", toks2 == [seed_token], json.dumps(toks2)[:120])
        check("回灌后密文已重新生成（随机 nonce，非逐字节沿用）",
              after["providers"][0]["access_keys"][0]["token_cipher"] != cipher)

        # ⑥ 非法余额被拒
        bad = json.loads(json.dumps(back))
        bad["balance"] = {"full": 100, "green_pct": 20, "warn_pct": 80}
        code, _, _ = http("PUT", base + "/api/config", bad, auth)
        check("非法余额（黄 > 绿）被拒 400", code == 400, str(code))
        bad["balance"] = {"full": 100, "green_pct": 50, "warn_pct": 20}
        code, _, _ = http("PUT", base + "/api/config", bad, auth)
        check("合法余额（非默认值）放行", code == 200, str(code))
    finally:
        inst.kill()


def scenario_trends(binpath, mock):
    """v0.4.0 趋势：采集入档 / 接口契约 / 删除 AK 连带清理（数据层兜底）。"""
    print("\n== 场景：趋势（采集入档 / 接口契约 / 删除 AK 连带清理） ==")
    d = tempfile.mkdtemp(prefix="qc-e2e-trend-")
    port = free_port()
    cfg = os.path.join(d, "config.json")
    seed_path = os.path.join(d, "trends.json")
    # 预置一份 trends.json：含一个稍后会被删掉的 Key。
    # 目的：清理必须落在**数据层**（文件里真的没了），而不是只从内存视图里消失。
    with open(seed_path, "w", encoding="utf-8") as f:
        json.dump({
            "version": 1,
            "written_at": "2026-09-21T08:00:00+08:00",
            "keys": {
                "opencode.gone": {
                    "id": "opencode.gone", "name": "Gone", "platform": "opencode", "kind": "percent",
                    "days": [{"date": "2026-09-21", "samples": 10, "gaps": 0,
                              "pct": {"peak": 30, "last": 30, "gain": 30, "samples": 10}}],
                    "hours": [],
                }
            },
        }, f)
    inst = Instance(binpath, cfg, port, upstream=mock.url())
    base = f"{BASE}:{port}"
    try:
        inst.wait_ready()
        check("启动加载既有趋势数据", "已加载 1 个 API Key 的趋势数据" in inst.log())

        code, _, tv = http("GET", base + "/api/trends")
        check("趋势接口公开可读（与 /api/quotas 同级）", code == 200, str(code))
        check("预置趋势数据可见", any(k["id"] == "opencode.gone" for k in tv["keys"]), json.dumps(tv["keys"])[:160])
        check("daily 定长连续（默认 400 天）", len(tv["keys"][0]["daily"]) == 400, len(tv["keys"][0]["daily"]))
        check("hours 定长连续（48 槽）", len(tv["keys"][0]["hours"]) == 48, len(tv["keys"][0]["hours"]))
        check("缺数据的槽位是 null（不画成 0）",
              sum(1 for x in tv["keys"][0]["daily"] if x is None) == 399,
              json.dumps(tv["keys"][0]["daily"])[:120])
        code, _, tv7 = http("GET", base + "/api/trends?days=7")
        check("days=7 生效", code == 200 and len(tv7["keys"][0]["daily"]) == 7)
        code, _, _ = http("GET", base + "/api/trends?days=abc")
        check("days 非法 → 400", code == 400, str(code))

        # 登录 + 配两个 OpenCode Key；mock 换成 rolling.percent（= 5 小时额度口径）
        mock.payload = {"usage": {"rolling": {"percent": 42}}}
        code, hdr, _ = http("POST", base + "/api/login", {"password": "Quota@2026090S"})
        auth = {"Cookie": "qc_session=" + re.search(r"qc_session=([^;]+)", hdr.get("Set-Cookie", "")).group(1)}
        put = {
            "version": CUR_VERSION,
            "listen": {"host": "127.0.0.1", "port": port},
            "collector": {"interval_base_s": 30, "jitter_min_s": 1, "jitter_max_s": 2,
                          "stagger_min_s": 1, "stagger_max_s": 2, "backoff_multiplier": 2, "backoff_max_s": 1800},
            "auth": {"mode": "admin"},
            "providers": [{"platform": "opencode", "access_keys": [
                {"id": "keep", "name": "保留", "token": "sk-e2e-keep-1234"},
                {"id": "gone", "name": "待删", "token": "sk-e2e-gone-5678"},
            ]}],
        }
        code, _, body = http("PUT", base + "/api/config", put, auth)
        check("配两个 Key 的 PUT 200", code == 200 and body.get("ok") is True, json.dumps(body)[:160])
        snap = wait_snapshot(base, lambda s: len([p for p in s["providers"] if p.get("status") == "ok"]) == 2, timeout=40)
        check("两个 Key 都采集成功", len([p for p in snap["providers"] if p.get("status") == "ok"]) == 2,
              json.dumps(snap)[:200])

        _, _, tv = http("GET", base + "/api/trends")
        kmap = {k["id"]: k for k in tv["keys"]}
        check("两个 Key 都进了趋势", {"opencode.keep", "opencode.gone"} <= set(kmap), json.dumps(list(kmap)))
        gk = kmap["opencode.gone"]
        cell = gk["daily"][-1]
        check("daily 末位 = 今天且有数据", cell is not None and cell["date"] == tv["today"],
              json.dumps(cell)[:160])
        check("柱高口径 = 5 小时额度峰值 42", bool(cell) and cell["pct"]["peak"] == 42, json.dumps(cell)[:160])
        check("采样点数已累计", bool(cell) and cell["pct"]["samples"] >= 1, json.dumps(cell)[:120])
        check("数值按源精度落盘（整数 42，不放大成小数）", bool(cell) and cell["pct"]["peak"] == 42 and cell["pct"]["last"] == 42)
        check("预置历史 + 今日并存（total_days=2）", gk["total_days"] == 2, gk["total_days"])

        # 删除 gone → 趋势数据连带清理（确认框在前端，这里是数据层兜底）
        put["providers"][0]["access_keys"] = [{"id": "keep", "name": "保留"}]
        code, _, _ = http("PUT", base + "/api/config", put, auth)
        check("删除 Key 的 PUT 200", code == 200, str(code))
        _, _, tv2 = http("GET", base + "/api/trends")
        ids = [k["id"] for k in tv2["keys"]]
        check("删除 AK 后趋势不再可见", "opencode.gone" not in ids, json.dumps(ids))
        check("未删的 Key 趋势保留", "opencode.keep" in ids, json.dumps(ids))
        with open(seed_path, encoding="utf-8") as f:
            saved = json.load(f)
        check("趋势文件里也已清理（不只在内存）", "opencode.gone" not in (saved.get("keys") or {}),
              json.dumps(list((saved.get("keys") or {}).keys())))
        check("清理动作有 INFO 日志", "清理了" in inst.log())
        blob = json.dumps(saved)
        check("趋势文件不含 token 明文/密文（只有指标）",
              "sk-e2e-keep-1234" not in blob and "sk-e2e-gone-5678" not in blob and "token_cipher" not in blob)

        # ---- 清空趋势数据（POST /api/trends/reset）：admin 才能调，且要真清到文件 ----
        code, _, _ = http("POST", base + "/api/trends/reset", {"key": ""})
        check("清空前未登录调用被拒（401）", code == 401, str(code))
        code, _, body = http("POST", base + "/api/trends/reset", {"key": ""}, auth)   # http() 返回 (status, headers, body)
        check("清空全部：200 且返回计数", code == 200 and body.get("keys", 0) >= 1, str(body))
        _, _, tv3 = http("GET", base + "/api/trends")
        check("清空后视图里没有 Key", not tv3["keys"], json.dumps([k["id"] for k in tv3["keys"]]))
        with open(seed_path, encoding="utf-8") as f:
            saved2 = json.load(f)
        check("清空后文件里没有 Key", not (saved2.get("keys") or {}),
              json.dumps(list((saved2.get("keys") or {}).keys())))
        check("清空动作有 INFO 日志", "清空趋势数据" in inst.log())
        # 重新采集一次：清理后的第一条样本必须从「没有上一次」开始（用量 0、峰值 = 本次值）
        time.sleep(2)
        _, _, tv4 = http("GET", base + "/api/trends")
        for k0 in tv4["keys"]:
            today = [x for x in k0["daily"] if x]
            if today:
                pct = (today[-1] or {}).get("pct") or {}
                check("清空后重新采集：用量从 0 起算（不沿用清理前的值）", pct.get("gain", 1) == 0,
                      json.dumps(pct))

    finally:
        inst.kill()


def main():
    if not BIN or not os.path.exists(BIN):
        print("用法: python scripts/e2e.py <quotaclock 可执行文件>")
        sys.exit(2)
    binpath = os.path.abspath(BIN)
    mock = MockUpstream()
    try:
        d, port, cfg, inst = scenario_first_start(binpath, mock)
        try:
            scenario_api_chain(d, port, cfg, inst, mock)
            scenario_log_redaction(inst, cfg)
            # 双开/stale 场景要求第一实例存活，故在 kill 之前执行（结尾以 kill -9 收尾）
            scenario_lock(binpath, d, port, cfg, inst)
        finally:
            inst.kill()
        scenario_graceful(binpath, d, port, cfg)
        scenario_migration(binpath, mock)
        scenario_migrate_v3(binpath, mock)
        scenario_v030(binpath, mock)
        scenario_trends(binpath, mock)
        scenario_enabled_and_cache(binpath, d, port, cfg, mock)
        scenario_startup_time(binpath)
    finally:
        mock.close()

    print("\n===== E2E 结果 =====")
    fails = [r for r in results if not r[1]]
    for name, ok, detail in results:
        print(("PASS  " if ok else "FAIL  ") + name + ("" if ok else "  -> " + detail))
    print(f"\n{len(results) - len(fails)}/{len(results)} 通过")
    sys.exit(1 if fails else 0)


if __name__ == "__main__":
    main()
