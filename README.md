# QuotaClock

多平台 LLM 配额总览 Dashboard——单二进制 Go 服务端，零依赖、开箱即用。

后台常驻采集器定时拉取各平台用量，浏览器访问 `http://127.0.0.1:8787` 即可打开看板，
无需任何本地配置；一份服务端配置全设备共享。

![Version](https://img.shields.io/badge/Version-0.4.0-blue)
![Platform](https://img.shields.io/badge/Platform-智谱%20%7C%20DeepSeek%20%7C%20Kimi%20%7C%20OpenCode-blue)
![License](https://img.shields.io/badge/License-MIT-green)

## 功能

- **多平台额度总览**：智谱 GLM / DeepSeek / Kimi Code / OpenCode 四个内置平台，**只能选平台、只需填 API Key**（接口地址与请求路径由程序内置，避免填错地址却看不出问题）
- **趋势页（用量历史）**：每个在采 API Key 一张卡，区间切换「当日 / 7 日 / 月度 / 季度 / 年度」，**柱高 = 该时段的实际用量**（额度平台为百分比增量之和，DeepSeek 为实际消费）；0 值不画柱、连续没采样画成淡色区段，悬停按 x 吸附出明细。数据每小时落盘 `trends.json`（只存聚合指标，不含 token），删 Key 连带清理，趋势页也有「清空趋势数据」入口
- **深浅色主题**：页头最右侧的 🌗 按钮一键切换（深色显示太阳、浅色显示月亮），选择存 localStorage、刷新不闪色；默认深色
- **同平台多 API Key**：一个平台可挂任意多个 Key，采集按「基准间隔 ÷ Key 数」等分错开、同平台请求串行化——多个 Key 互不冲突
- **后台常驻采集**：默认每个 Key 300s + 5~25s 抖动定时拉取，平台间 1~5s 错峰；页面只读服务端缓存
- **上次成功数据缓存**：每次成功采集后写入 `cache.json`，**重启服务端后页面立刻显示上次数据**（徽标「缓存数据」，首轮采集到了自动转「正常」），不再回到空白等待
- **凭据可停用**：设置里取消勾选某个 Key 的「启用」即停采，并从首页/接口页隐藏（状态行提示「N 个已停用，未显示」），比删掉再重建更省事
- **失败语义区分**：token 失效红字提示并停采该 Key；平台异常（5xx/网络）暂停该平台全部 Key 并指数退避（×2 封顶 30min），单 Key 限流（429）按 Retry-After 顺延且只停该 Key
- **配置加密落盘**：token 以 AES-256-GCM 加密存 `config.json`，密钥为随机生成的 `key.bin`，配置与日志中永不出现明文
- **时间用本地时区**：看板上的时间戳按运行机器的本地时区显示
- **两档鉴权**：`admin`（查看公开、修改须登录）/ `none`（全开放，本机自用）
- **设置分三个标签**：「平台管理」（平台 + API Key 列表）、「采集与服务」（采集间隔、监听 IP / 端口）与「账户安全」（鉴权模式/改密码/登录态）
- **页头按钮固定顺序**：刷新 / 设置 / 登出 / 主题切换（未登录时「登录」顶替「设置 / 登出」的位置）；登出只在这里，设置弹窗里不再重复放一个
- **采集状态一眼分清**：卡片徽标有「等待采集」（新加的 Key 或服务端刚起、还没采到）、「采集异常」（平台限流 429，等等就好，带重试倒计时）、「采集失败」（5xx / 超时 / 网络 / 业务失败，带重试倒计时）、「缓存数据」（显示上次成功采集的数据）、「已停用」、「Key 失效」
- **先测通再保存**：新增的 API Key 不用先保存就能点「测试」；新增的 Key 不允许留空（前后端都会拦），已保存过的 Key 留空 = 保留原值
- **配置导出 / 导入**：在设置里导出可读 JSON（**默认含明文 API Key**），拷到另一台机器导入即可——到新机器只改不一样的那几个 Key，不用重配已有的（token 与本机 `key.bin` 绑定，直接拷 `config.json` 是解不开的）。导入前先看差异预览，确认后全量替换并热生效。**文件含明文凭据，导入后请尽快删除**
- **余额型平台也能红黄绿**：DeepSeek 这类只有账户余额、没有周期额度的平台，可设「满额基准 + 档位阈值」，看板按 `余额 ÷ 满额` 的百分比上色（默认满额 100 元、绿 ≥50%、黄 ≥20%；余额越高越健康，方向与用量相反）
- **看板卡片紧凑**：300px 宽，每张卡固定三行（5 小时 / 周 / 月），没有数据的行也用「—」占位——不管一条还是三条数据，所有卡片对齐得很整齐；点标题栏可折叠（折叠后显示各区间小环，状态本地记住），卡片底部显示该 Key 自己的采集时间与状态灯
- **热生效**：保存配置后采集器立即按新配置重建；仅监听地址/端口需重启

## 快速开始

1. 下载对应平台产物（Release 页）：`quotaclock.exe`（Windows amd64）/ `quotaclock-Linux-x86_64` / `quotaclock-Linux-arm64`
2. 放到任意目录，直接运行：

```bash
./quotaclock              # Linux
quotaclock.exe            # Windows（双击亦可）
```

3. 首次启动自动生成模板 `config.json`，并**自动在默认浏览器中打开看板**（无图形环境会跳过并打印地址）：

```
2026-09-05 19:00:00 [INFO] 已生成模板配置 ...\config.json（默认密码 Quota@2026090S，请尽快修改）
2026-09-05 19:00:00 [WARN] 正在使用默认密码，请立即修改
2026-09-05 19:00:00 [INFO] QuotaClock v0.4.0 已启动 | 监听 http://127.0.0.1:8787 | auth=admin | ...
2026-09-05 19:00:00 [INFO] 已在浏览器打开 http://127.0.0.1:8787
```

4. 浏览器打开 `http://127.0.0.1:8787` → 点页头「登录」（默认密码 `Quota@2026090S`）→ 再点页头「设置」→
   从下拉里选平台、填入 API Key → 保存，采集器即刻开始拉取。设置面板分「平台管理」/「采集与服务」/「账户安全」三个标签。新加的 Key 可以先点「测试」确认连通再保存（未保存也能测），但**不能留空保存**。

> ⚠️ 首次登录后请立即在设置中修改管理密码。

### Token 获取指南

| 平台 | 获取方式 |
|---|---|
| 智谱 GLM | 登录 open.bigmodel.cn → F12 → Network → 点任意 `/api/biz/...` 请求 → 复制请求头 `Authorization` 里 `Bearer` 后面的值 |
| DeepSeek | platform.deepseek.com → API Keys |
| Kimi Code | platform.kimi.com → API Keys（周期额度查询走 `/usages`，官方 CLI 同款接口） |
| OpenCode | opencode.ai 控制台 → API Keys（鉴权方式用缺省 bearer；用量接口 `GET /zen/go/v1/usage`） |

> v0.2.5 起**只能从这 4 个内置平台里选**，界面上只填 API Key：接口地址、请求路径与鉴权方式（四家统一 Bearer）由程序内置。同一平台可以加多个 Key——采集会按「基准间隔 ÷ Key 数」把它们的请求等分错开，避免互相冲突。

## 从旧版本迁移

v0.1 是**单页 HTML（`index.html`）+ 一个 Python 反向代理（`llm-proxy.py`，本机 8787，绕开各家网关的 CORS 限制）**：解析与渲染全在浏览器里做，配置只存在浏览器 `localStorage`（可「导出 JSON」备份），服务端不存任何东西。迁移走**旁路文件**（localStorage 跨源不可读，因此没有导入 UI）：

1. 从 Release 页下载 **v0.1.0** 中的 `index.html`（或 `git checkout v0.1.0 -- index.html`），浏览器打开（file://）→ 设置 → 「导出 JSON」下载配置
2. 将导出文件**重命名为 `config.json`**，放到 v0.2 可执行文件同目录
3. 启动 v0.2 → 自动迁移：schema 升至当前版本、token 立即加密落盘、
   原 Python 代理平台自动改写为直连地址（启动日志打印改写对照）
4. 核对日志后按需在设置页修正各平台的 API Key

### 迁移改写对照表（自动执行）

| v0.1 base_url | 迁移结果 | 附带动作 |
|---|---|---|
| `http://127.0.0.1:8787/kimi` | 平台 `kimi-code` | 截余路径并入；请求路径收敛为预设 `/usages` |
| `http://127.0.0.1:8787/opencode` | 平台 `opencode` | 官方用量接口；v0.1 的 Cookie 登录态无法转换为 API Key，**token 未迁移**——到控制台生成 API Key 后在设置页录入（设置页里不再出现 Cookie 字样；这条只是说明旧配置为什么没带过来） |
| 直连地址（如 open.bigmodel.cn） | 对应平台（按地址识别） | 其余字段按平台预设收敛 |
| 指向其他本机代理端口 / 未知地址 | **跳过 + WARN** | v0.2.5 起不再支持自建平台；请到设置页从 4 个内置平台里重新添加 |

v0.1 的 `enabled=false` 平台**导入并保持停用**（停用凭据不采集、卡片灰显「已停用」，到设置里勾选「启用」即可开始采集）；endpoint 的 JSON 参数不迁移（如需 query 请并入 path）。

### 从 v0.2.x（version 3）升级

**无需任何操作**：首次启动自动升级到 version 4 —— 按地址识别平台、同一平台的多个旧条目自动合并成「一个平台 + 多个 API Key」、token 密文原样保留、监听/间隔/密码都不动。启动日志会打印归并与收敛明细（例如 Kimi 的请求路径收敛为 `/usages`，旧版本会额外请求的 `/me`、`/models` 从此不再发送）。

**升级前会自动备份**：原 `config.json` 会被整份复制成 `config.json.v3.bak`（启动日志第一行会打印路径），内容与升级前的文件**逐字节一致**（含旧的 `base_url`/`paths`）。核对新配置无误后可删除备份；想回退就把 `.bak` 改名回 `config.json`。备份文件同样属运行时文件（已 gitignore，内含 token 密文，切勿外传）。

> 备份只发生在**版本迁移**时；日常保存配置不会留备份文件。同一个旧版本再次迁移时，备份会追加序号（`config.json.v3-2.bak`），不会覆盖已有的那份。

> **端口提示**：v0.1 的 kimi/opencode 依赖 Python 代理 `llm-proxy.py`（127.0.0.1:8787），
> v0.2 服务端直连后**不再需要代理**。v0.2 默认也监听 8787——迁移前请先关闭代理，
> 或用 `-port` 换端口。

## 配置说明（config.json）

```jsonc
{
  "version": 5,
  "listen":   { "host": "127.0.0.1", "port": 8787 },
  "collector": {
    "interval_base_s": 300,    // 每个 API Key 的采集周期（≥30）
    "jitter_min_s": 5, "jitter_max_s": 25,
    "stagger_min_s": 1, "stagger_max_s": 5,
    "backoff_multiplier": 2, "backoff_max_s": 1800
  },
  "balance": { "full": 100, "green_pct": 50, "warn_pct": 20 },  // 余额型平台（DeepSeek）：满额 100 元 → 100%，绿 ≥50% / 黄 ≥20% / 其余红
  "auth": { "mode": "admin" },                 // password_hash 服务端自管，勿手写
  "providers": [{
    "platform": "deepseek",                    // 四选一：zhipu-glm | deepseek | kimi-code | opencode
    "access_keys": [
      { "id": "k1", "name": "主号", "token_cipher": "…" },              // token_cipher 服务端自管，勿手写
      { "id": "k2", "name": "备用", "token_cipher": "…", "enabled": false }  // 缺省 = 启用；false = 停用
    ]
  }]
}
```

- 推荐在设置页修改（保存即原子写盘 + 热生效）；token 留空 = 保留原值
- `enabled` 缺省即启用；停用的**凭据**不参与采集、不在首页/接口页出现
- **同一平台多个 Key**：每轮采集里它们的启动时刻按 `interval_base_s ÷ Key 数` 等分错开（例：300s + 2 个 Key → 相隔 150s），各自仍是 300s 一轮
- `balance` 只影响看板渲染（余额型平台的红黄绿档位），不参与采集；留空或写 0 = 用默认值
- 手工编辑 `config.json` 亦受支持，但请先停止程序（运行中保存会覆盖手改）
- **换机器 / 换平台**：不要直接拷 `config.json`（里面的 token 用本机 `key.bin` 加密，到别的机器解不开）——用设置里的「导出配置」拿到可读 JSON，到新机器「导入配置」即可；导入后请删除导出文件（含明文 Key）
- `config.json` / `config.json.v*.bak`（迁移前自动备份）/ `key.bin` / `quotaclock-*.lock` / `cache.json` / `trends.json` 为运行时文件，已列入 .gitignore，切勿外传
- **回退旧版本**：升级时已自动留了 `config.json.v<N>.bak`——把它改名回 `config.json` 再换回旧版二进制即可；若已在旧版本上手改过配置，删除 version 4 的 `config.json` 重新录入亦可

## 启动参数

| 参数 | 默认 | 说明 |
|---|---|---|
| `-addr` | `127.0.0.1` | 监听 IP（`0.0.0.0` 时日志与页面双重暴露警示） |
| `-port` | `8787` | 监听端口（改动需重启生效） |
| `-interval` | config 值 | 采集基准间隔（秒），覆盖 config |
| `-config` | exe 同目录/config.json | 配置文件路径（亦可用环境变量 `QUOTACLOCK_CONFIG`） |
| `-admin-password` | 无 | 初始化/重置管理密码（写盘后继续启动） |
| `-version` | - | 打印版本后退出 |

优先级：启动参数 > config.json > 内置默认值。环境变量：

| 环境变量 | 说明 |
|---|---|
| `QUOTACLOCK_CONFIG` | 配置文件路径（等价 `-config`） |
| `QUOTACLOCK_LOG=debug` | 日志提升至 DEBUG |
| `QUOTACLOCK_NO_BROWSER=1` | 禁用启动时自动打开浏览器 |

自动打开浏览器的行为：Windows（交互会话）用默认浏览器打开；Linux 检测到图形会话
（`DISPLAY` / `WAYLAND_DISPLAY`）时经 `xdg-open` 打开，SSH / systemd / 容器等无图形环境
自动跳过、只打印访问地址。

## 部署指南

### 常驻运行

```ini
# Linux systemd 示例
[Service]
ExecStart=/opt/quotaclock/quotaclock -config /opt/quotaclock/config.json
Restart=on-failure
# 日志走 stdout，journald 自动收集；建议绝对路径启动
```

```powershell
# Windows：计划任务 / 快捷方式
D:\quotaclock\quotaclock.exe -config D:\quotaclock\config.json -port 8787
```

- **单实例锁**：同一配置文件同时只允许一个进程（重复启动提示 PID 后退出）；
  崩溃/断电残留的锁会被下次启动自动接管
- **多实例**：同目录放多份配置（如 `config-a.json` / `config-b.json`）即可并行多套账号
- **局域网开放**：`-addr 0.0.0.0`（建议置于反向代理之后；HTTPS 建议由反代终结）

### 恢复路径 ①：key.bin 丢失 / 损毁

`key.bin` 是全部 token 的加密密钥，**丢失后密文不可恢复**（设计使然，无后门）：

1. 停止程序；`key.bin` 已损坏或被删则跳过此步
2. 启动程序——自动生成新 `key.bin`，所有平台显示「token 无法解密，请在设置中重新录入」
3. 到设置页逐平台重新粘贴 token 保存即可；其余配置不受影响

### 恢复路径 ②：忘记管理密码

```bash
quotaclock -admin-password 新密码
# 日志提示「管理员密码已重置」，随后正常启动，用新密码登录
```

### 恢复路径 ③：想清掉看板里的「上次成功数据」

`cache.json` 只存上次成功采集的展示数据（无 token），**随时可删**：
停止程序 → 删除 `cache.json` → 重启，看板回到「等待采集」状态（首轮采集结束后照常出数）。

## 隐私与安全

- token 明文只存在于内存；落盘为 AES-256-GCM 密文；日志全链路脱敏（明文/密文/掩码/会话 cookie 永不入日志）
- 后端只读硬约束：唯一写路径是配置保存；文件系统白名单仅 `config.json` / `key.bin` / 锁文件 / `cache.json`（仅存展示数据，不含 token）/ `trends.json`（仅存趋势聚合指标，不含 token）/ `*.tmp`
- 会话 cookie：HttpOnly + SameSite=Lax，7 天有效；登出立即失效（重启后旧登出 cookie 至多存活至自然过期，单管理员场景已接受的取舍）
- CORS 红线：服务端不发送任何 CORS 头；`/api/login` 仅接受 JSON（CSRF 三道防线）
- `auth.mode=none` 表示全开放（任何可访问者可查看/修改配置），页面顶部常驻警示

## 常见问题

**平台接口返回失败？**
4 个平台的接口地址与请求路径由程序内置（各家的用量接口都是官方文档里的地址），一般无需跟进平台部署变化；
万一某家改了接口，升级到包含新地址的版本即可（配置里没有地址可改）。
OpenCode 用的是官方用量接口（`GET /zen/go/v1/usage`，Bearer API Key）。

**怎么看原始返回？**
「接口」页可查看每个 API Key 的状态、上次成功采集时间与原始 JSON（原「详情」页，v0.4.0 改名）；日常看用量用「总览」和「趋势」。

**重启后卡片显示「缓存数据」？**
那是在读上次成功采集的结果（`cache.json`），首轮采集一到就会转「正常」；想强制立即刷新，点页面右上「刷新」。

**首页 / 接口页少了某个 Key？**
停用的凭据不在首页和接口页显示。到「设置 → 平台管理」勾选该 Key 的「启用」即可恢复（平台上会显示「停用」标记）。

**能加新的平台吗？**
不能——只支持智谱 GLM / DeepSeek / Kimi Code / OpenCode 四个内置平台。这四家的返回结构各有各的字段，
解析规则与地址是配套写死的，允许自填地址只会得到「接口通了但认不出字段」。
但每个平台可以挂**任意多个 API Key**（同一账号多个 Key 或不同账号都行），采集会自动按
「基准间隔 ÷ Key 数」把它们的请求等分错开，互不冲突。

**同一个平台加多个 Key 会打架吗？**
不会。例：基准间隔 300s、2 个 Key → 两个 Key 的采集相隔 150s，各自仍是 300s 一轮；
同一时刻同一个平台只会有一个请求在跑（上一个没回，下一个会主动避让）。

**升级会不会把我的配置改坏？**
不会。程序改配置前先把原文件整份备份（升级时会生成 `config.json.v3.bak` 这类文件，内容与改前**逐字节一致**），
万一有问题，把它改名回 `config.json` 即可回退；备份失败时程序会**中止升级**而不是硬写。

**时间怎么是本地时间？**
`v0.2.5` 起页面上的时间戳按**运行程序那台机器的本地时区**显示（如 `2026-09-12T19:35:52+08:00`）。

**换机器怎么搬？**
`-config` 指向任意路径；`key.bin` + `config.json` 一起拷走即可在别的机器解密（同配置请用绝对路径启动）。

## 构建

```bash
go build -ldflags "-s -w -X main.version=v0.4.0" -o quotaclock ./cmd/quotaclock
go test ./...                        # 单元 + 集成（-race 建议）
python scripts/e2e.py ./quotaclock   # E2E 演练（需要 python3）
```

本地联调（把采集指向 mock 上游，不改配置、不碰真实平台）：

```bash
python scripts/mock_upstream.py 8899                       # 终端 1
QUOTACLOCK_UPSTREAM_OVERRIDE=http://127.0.0.1:8899 ./quotaclock -port 8797   # 终端 2
```

## 发布流程

发布由 GoReleaser + GitHub Actions 完成（push tag 触发，windows/amd64 + linux/amd64 + linux/arm64 产物 + sha256）。Release 正文不是模板文案，而是自动拼出来的：

```bash
# 1. 先写 CHANGELOG.md：把当前版本的小节从「未发布」改成发布日期，写清新增/变更/修复
# 2. 本地预览这次会生成的 Release 正文（不推任何东西）
python scripts/release_notes.py v0.4.0
# 3. 打标签并推送（CI 自动：go vet → go test -race → 三平台产物 → 建 Release）
git tag -a v0.4.0 -m "v0.4.0 …"   # 标签注释也会留在 git 里，方便 git tag -n99 查看
git push origin v0.4.0
```

Release 正文 = 按版本给出的下载说明 + **CHANGELOG.md 里该版本的小节（本版重点）** + 两版之间的提交明细（按新增/修复/性能/重构分组，去掉 40 位 SHA，`docs:`/`chore:`/`test:` 不进正文）。CI 里由 `scripts/release_notes.py` 生成后交给 `goreleaser --release-notes`，见 `.github/workflows/ci.yml`；CI 带 `--strict`——**CHANGELOG 里没写这个版本，发布直接失败**（避免又发一条「看起来都一样」的说明）。

## License

MIT，见 [LICENSE](LICENSE)。
