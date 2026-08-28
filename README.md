# AutoSMS

AutoSMS 将 SMSBower 或 HeroSMS 号码购买、GoPay 已有号码登录、余额判断、PIN 设置与后续验证码轮询整合到一个网页工作台。后端使用 Go + Gin，前端使用 Vue 3 + Element Plus，数据保存在 PostgreSQL 16。

发布镜像是一个完整部署单元：Vue 页面会被编译并嵌入 Go 二进制，PostgreSQL 16 与 Go 服务运行在同一容器中。正常启动不需要预先安装或配置 PostgreSQL。

## 一条命令启动

需要 Docker Desktop 或支持 Compose v2 的 Docker Engine。在项目根目录执行：

```bash
docker compose up --build
```

首次构建完成后访问 [http://localhost:8080](http://localhost:8080)。容器会自动完成以下工作：

1. 在 `/data/runtime/db.env` 生成数据库密码和服务加密密钥；
2. 在 `/data/postgres` 初始化 PostgreSQL 16；
3. 创建数据库和用户、运行服务内置迁移；
4. 启动 Gin API 与内嵌的 Vue 页面。

数据库和凭据保存在 Compose 命名卷 `autosms-data` 中。执行 `docker compose down` 后再次启动，账号记录、号码历史和凭据都会保留。

启动日志会明确显示 Web 地址及内置数据库连接信息：

```text
DB host:     127.0.0.1
DB port:     5432 (container internal only)
DB name:     autosms
DB user:     autosms
DB password: <首次启动自动生成>
Web address: http://localhost:8080
```

可随时用下面的命令重新查看：

```bash
docker compose logs autosms
```

数据库端口只在容器内部监听，不会发布到宿主机。日志包含数据库明文密码，请限制 Docker 日志的读取权限。

## 页面配置与使用

打开工作台后，先在设置卡中选择 SMSBower 或 HeroSMS，再填写当前平台的 API Key 并保存。两个平台的 API Key 分开加密保存在 PostgreSQL，不会相互覆盖；同时，它们也会分别以明文持久化到当前浏览器的本地存储。创建购买任务时选择的平台以及填写的服务、国家、价格、购买数量、PIN 和代理池等表单内容也会作为明文草稿保存在浏览器本地，刷新或重新打开页面后自动恢复。浏览器本地存储中的这些数据可被同源页面脚本访问，请只在可信设备和可信部署环境中使用，并注意浏览器账户及扩展的安全。API Key 不会写入 Docker Compose 文件。

随后按页面顺序选择服务、国家和价格，填写购买数量以及 6 位数字 PIN，再启动任务。计划购买数量的服务端允许范围为 1–100，超出范围的任务创建请求会被驳回。服务、国家、价格目录和购号请求均使用当前选中的平台。选择 HeroSMS 时，报价字段显示为“价格”，只展示其 `getPrices` 返回的数值和库存；该接口不返回账户币种，因此页面不会猜测币种，也不会显示、派生或提交 SMSBower 的供应商 ID 及 Bronze、Silver、Gold 档位。实际购号后，币种会按 HeroSMS `getNumberV2` 返回值规范化并保存。SMSBower 报价仍以 ₽ 展示。任务创建时会固化所选平台；后续在页面切换平台不会改变已有任务和号码的购买、轮询、结算或取消去向。升级前创建且没有来源记录的旧任务继续按 SMSBower 处理。SMSBower 和 HeroSMS 的官方接口地址均由服务内置，页面不需要填写 Base URL。

“计划购买数量”表示任务最终需要完成的成功数（`fulfilled_count`），不是累计买到的号码上限。例如输入 `3`，任务会持续补购，直到有 3 个号码完成 GoPay 处理并进入 `fulfilled`。每个成功分配且落库的号码都会计入 `purchased_count` 并占用一个处理槽位；在首次进入 `fulfilled` 前，未注册、需要 PIN 登录、0 RP、`phone_in_use`、登录或改 PIN 验证码超时、其他处理失败、供应商取消或过期以及手动删除等终态结果都会释放该槽位，只要 `fulfilled_count` 尚未达标就会自动补购。因此 `purchased_count` 可能高于计划数量；`fulfilled_count` 达标后，任务自动进入 `completed` 并停止购买。已进入 `fulfilled` 的号码不再占用 `inflight` 槽位；后续收码阶段发生取消、过期或手动删除时，只结束或隐藏该已成功记录，保留 `fulfilled_count`，不减少 `inflight` 也不因此补购。供应商明确未分配号码的临时错误会退避重试，不计入 `purchased_count`；如果购号回执或号码落库结果未知，服务会冻结自动购买并保留待核实状态，避免重复请求造成重复扣费。

号码不再使用历史黑名单或指纹去重：服务不再维护 `phone_history`，也不会因号码曾经购买过就将新激活标记为 `duplicate`。为避免同一号码的两个激活共享或改写同一份 GoPay 会话，新号码落库时仍会检查当前未结束的激活：如果同一号码已有 `finished_at` 为空的记录，新分配会标记为 `phone_in_use`，自动调用 `setStatus=8` 取消，释放槽位并在成功数未达标时补购；原激活结束后，该号码即可再次使用。点击“删除”会取消号码并隐藏当前记录，不保留用于阻止以后处理的永久去重指纹。服务同一时间只允许运行一个任务，当前任务停止前会拒绝创建新任务；服务每次启动都会先停止数据库中遗留的运行中任务，并取消其未完成号码。页面会展示号码状态、登录验证码、改 PIN 验证码和持续收到的后续验证码。每次触发对应的 GoPay OTP 后，登录验证码等待超过 60 秒、改 PIN 验证码等待超过 80 秒且供应商仍未返回验证码时，服务会先调用该任务所属平台的 `setStatus=3` 请求下一条短信，再重新触发相应的 GoPay OTP；每个验证码阶段最多重发 3 次（初次发送不计为重发）。第三次重发后再次等待超过对应时长仍无码，服务会调用同一平台的 `setStatus=8` 取消号码，并按阶段标记为“登录验证码失败”或“改 PIN 验证码失败”；该号码会释放处理槽位并触发补购。每次重发会在外部发码前持久化并计入预算；若发码结果与最新 OTP token 之间发生进程或数据库中断，该次仍会完整等待对应的 60 秒或 80 秒窗口且不会无计数重复发送，随后继续下一次或按三次上限取消。任务持有号码期间每 2 秒轮询一次；点击“停止任务”会停止后续购买，并为未完成号码排队执行取消（号码会从当前列表隐藏）；点击“成功”会结算号码。

余额符合条件后，服务会优先通过 GoPay profile 判断当前账号应执行 PIN setup 还是 reset；profile 状态未知或返回 404 时先尝试 setup，setup 返回 `GoPay-111`（账号已有 PIN）则自动切换到 reset。收到改 PIN 验证码后，服务会先验证 OTP 并持久化验证结果，再提交最终 PIN，重试时不会重复使用已消费的验证码。页面状态依次显示“正在设置 PIN”→“改 PIN 成功”→“等待后续验证码”，随后保持轮询并按接收顺序追加后续验证码。

仅当号码处于提交新 PIN（`setting_pin`）阶段，并收到结构化的 HTTP 403 / `GoPay-112` 错误时，服务会保留 GoPay 失败详情并立即调用号码所属平台的 `setStatus=6` 结算号码，而不是取消号码。若结算暂时失败，后续重试会继续执行结算动作；该分支不计为 PIN 修改成功，也不会进入 `fulfilled`。结算完成后会释放处理槽位，并在成功数未达标时自动补购。其他阶段收到 `GoPay-112` 时，仍按既有失败流程调用同一平台的 `setStatus=8` 取消号码，释放槽位后继续补购。

GoPay 代理池在独立整行文本框中输入，每行一个地址；重复行会保留。支持以下格式：

```text
hostname:port:username:password
socks5://username:password@host:port
username:password@hostname:port
hostname:port@username:password
```

带协议头时支持 `http://`、`https://`、`socks5://`、`socks5h://`；不带协议头默认按 HTTP 处理。代理池是一次性的：系统随机分配未使用地址，某个代理端点一经被号码领取就会标记为已使用，重复输入的同一端点也会一并退出可用池；预检失败的地址同样不再使用。任务面板显示可用数/总数。如果用户提供了代理池且可用地址已耗尽，服务会先等待尚在处理的号码；当没有剩余处理中号码且没有未决购号预占，且成功数仍未达到计划数量时，任务会明确进入 `failed` 并停止继续购号，避免无代理可用时仍继续扣费。未配置代理池的直连模式不适用该耗尽规则。

## 数据与生命周期

- `docker compose down`：停止并删除容器，保留 `autosms-data` 数据卷。
- `docker compose up --build`：重新构建并启动，继续使用原有数据和凭据。
- `docker compose down -v`：同时删除数据库、账号、号码记录、API Key 和自动生成的凭据。此操作不可撤销。

容器收到停止信号后，会先通知 Go 服务和 PostgreSQL 正常退出。任一进程异常结束时，入口脚本也会关闭另一个进程，Compose 的重启策略随后负责恢复服务。

升级包含购号协议变更的版本时，请先停止所有旧版容器或进程，再启动新版；新旧版本不应同时连接同一个数据库运行。新版首次启动会冻结升级前遗留的活动任务，以免旧购号流程继续补购。

## 可选环境变量

默认配置可以直接运行。确有需要时，可在 `compose.yaml` 的 `environment` 下覆盖：

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `AUTOSMS_DB_USER` | `autosms` | 内置数据库用户 |
| `AUTOSMS_DB_NAME` | `autosms` | 内置数据库名 |
| `AUTOSMS_DB_PASSWORD` | 自动生成 | 内置数据库密码 |
| `AUTOSMS_SECRET_KEY` | 自动生成 | API Key、Token 等敏感数据的服务端加密密钥 |
| `AUTOSMS_DB_PORT` | `5432` | 容器内部 PostgreSQL 端口，不会自动发布到宿主机 |
| `AUTOSMS_HTTP_ADDR` | `:8080` | Go 服务监听地址 |
| `AUTOSMS_HEALTH_URL` | `http://127.0.0.1:8080/readyz` | Docker 健康检查地址 |
| `AUTOSMS_PUBLIC_URL` | `http://localhost:8080` | 启动日志中显示的 Web 地址 |
| `AUTOSMS_POLL_INTERVAL` | `2s` | 短信供应平台验证码轮询间隔 |
| `AUTOSMS_GOPAY_LOGIN_STATUS_TTL` | `4s` | GoPay 登录状态缓存窗口；略短于页面的 5 秒轮询，确保每轮可触发远端检查 |
| `AUTOSMS_ACTIVATION_TTL` | `20m` | 号码激活有效期 |
| `AUTOSMS_SMSBOWER_BASE_URL` | SMSBower 官方 handler 地址 | 仅服务端/测试环境覆盖 SMSBower endpoint，页面不会暴露此字段 |
| `AUTOSMS_HEROSMS_BASE_URL` | HeroSMS 官方 handler 地址 | 仅服务端/测试环境覆盖 HeroSMS endpoint，页面不会暴露此字段；兼容读取 `AUTOSMS_HERO_SMS_BASE_URL` |
| `AUTOSMS_GOPAY_SSO_BASE_URL` | GoPay 官方地址 | GoPay 登录 API 根地址，主要用于测试 |
| `AUTOSMS_GOPAY_BASE_URL` | GoPay 官方地址 | GoPay 业务 API 根地址，主要用于测试 |

数据库账号、密码、名称或加密密钥的显式覆盖会写回 `/data/runtime/db.env` 并在后续启动继续使用。修改已有数据卷的 `AUTOSMS_SECRET_KEY` 会使旧的加密字段失去可读性，因此生产环境初始化后应保持该值不变。

宿主机 Web 端口可直接通过 Compose 变量调整：

```bash
AUTOSMS_PORT=18080 docker compose up --build
```

此时访问 `http://localhost:18080`。Compose 默认只绑定宿主机回环地址，避免未加认证的本地工作台直接暴露到局域网；如需反向代理，请在代理层增加访问控制。

## 开发与测试

本地构建需要 Go 1.25、Node.js 22 与 npm。常用命令：

```bash
make frontend       # 安装前端依赖并构建 Vue 到 internal/webui/dist
make build          # 构建前端，然后生成 bin/autosms
make test           # 构建前端并运行全部 Go 测试
make frontend-dev   # 启动 Vite 开发服务器
make docker-build   # 验证完整容器镜像构建
make docker-up      # 构建并启动完整服务
make docker-logs    # 持续查看服务日志
make docker-down    # 停止服务，保留数据卷
```

Vite 开发服务器会把 `/api` 请求代理到 `http://127.0.0.1:8080`。调试前端时，先用 `make docker-up` 启动后端，再在另一个终端运行 `make frontend-dev`；Vite 默认地址为 `http://localhost:5173`。

健康检查接口：

- `GET /healthz`：Go 进程存活状态；
- `GET /readyz`：服务及数据库可用状态。

## 技术栈

- Go 1.25、Gin、pgx
- Vue 3、TypeScript、Element Plus、Vite
- PostgreSQL 16
- Docker Compose（单应用容器、单持久化数据卷）
