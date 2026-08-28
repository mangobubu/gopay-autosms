# AutoSMS

AutoSMS 将 SMSBower 或 HeroSMS 号码购买、GoPay 已有号码登录、余额判断、PIN 设置与后续验证码处理整合到一个网页工作台，并提供只负责 HeroSMS 购号与接码的独立页面。GoPay 主工作流中的 SMSBower 保留状态轮询，HeroSMS 只通过 webhook 接收验证码；独立 `/hero-sms` 页面则以 webhook 为主，并保留 45 秒低频供应商轮询兜底。后端使用 Go + Gin，前端使用 Vue 3 + Element Plus，数据保存在 PostgreSQL 16。

发布镜像是一个完整部署单元：Vue 页面会被编译并嵌入 Go 二进制，PostgreSQL 16 与 Go 服务运行在同一容器中。正常启动不需要预先安装或配置 PostgreSQL。

## 一条命令启动

需要 Docker Desktop 或支持 Compose v2 的 Docker Engine。在项目根目录执行：

```bash
docker compose up --build
```

首次构建完成后访问 [http://localhost:8080](http://localhost:8080)。本地 Compose 默认用户名为 `admin`，默认密码为 `autosms-local-password-change-me`；浏览器会先显示 HTTP Basic Auth 登录框。这组凭据只便于回环地址本地开发，公网部署必须换成强密码。容器会自动完成以下工作：

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
Web address: http://localhost:8080
```

可随时用下面的命令重新查看：

```bash
docker compose logs autosms
```

数据库端口只在容器内部监听，不会发布到宿主机。自动生成的数据库密码和加密密钥只写入权限为 `0600` 的 `/data/runtime/db.env`，启动日志不会输出它们。

## 公网 HTTPS 部署

公网模式使用 `compose.public.yaml` 叠加基础 Compose，由 Caddy 自动申请 TLS 证书并反向代理到内网中的 AutoSMS；应用的 `8080` 端口和 PostgreSQL 不直接向公网开放。部署前需要：

1. 一个你可控制 DNS 的域名，将其 A 记录（以及使用 IPv6 时的 AAAA 记录）指向部署服务器。
2. 公网防火墙和云安全组允许 TCP `80`/`443`；Compose 还会为 HTTP/3 发布 UDP `443`。
3. 强随机的 Basic Auth 密码，以及至少 32 个 URL-safe 字符的独立 webhook token；两者必须使用不同的随机值。

以仅当前用户可读写的权限创建环境变量文件，再替换所有占位值：

```bash
install -m 600 .env.public.example .env.public
```

`.env.public` 中至少要设置：

```dotenv
AUTOSMS_DOMAIN=sms.example.com
AUTOSMS_AUTH_USERNAME=admin
AUTOSMS_AUTH_PASSWORD=<至少-16-字符的强随机密码>
AUTOSMS_HEROSMS_WEBHOOK_TOKEN=<至少-32-字符的-URL-safe-随机值>
```

启动公网部署：

```bash
docker compose --env-file .env.public \
  -f compose.yaml -f compose.public.yaml \
  up --build -d
```

当 `https://sms.example.com` 可正常访问后，在 HeroSMS 个人信息页配置以下完整 webhook URL，其中最后一段必须与 `AUTOSMS_HEROSMS_WEBHOOK_TOKEN` 完全一致：

```text
https://sms.example.com/api/webhooks/hero-sms/<AUTOSMS_HEROSMS_WEBHOOK_TOKEN>
```

管理页面和普通 API 均由 HTTP Basic Auth 保护。HeroSMS 不会携带这组 Basic Auth 凭据，因此 webhook 使用独立的长随机 URL token 鉴权；请把完整 URL 当作密钥保管，不要写入日志、截图或公开文档。当前 Caddy 还只允许 HeroSMS 公布的源 IP `84.32.223.53` 和 `185.138.88.87` 访问该路由；HeroSMS 变更回调 IP 后，需要同步更新 `Caddyfile` 并重载 Caddy，否则新回调会被拒绝。

[HeroSMS 官方 API 文档](https://hero-sms.com/api)要求 webhook 使用 HTTPS `POST`，并在 3 秒内返回 `200`；失败时 HeroSMS 会至少重试 7 次。AutoSMS 只在回调原文已持久化到 PostgreSQL 后返回 `200`，再由后台 worker 异步处理；这样 HTTP handler 不会在 HeroSMS 的响应时限内执行 GoPay 业务流程。Caddy 会在运行日志中把 webhook URL 的 token 段替换成 `REDACTED`。

`/readyz` 仅供应用容器内部的 Docker 健康检查使用，Caddy 不向公网反向代理该路由。管理页面使用 Basic Auth，所以只应在 Caddy 提供的 HTTPS 地址上输入公网凭据。

## 页面配置与使用

打开工作台后，先在设置卡中选择 SMSBower 或 HeroSMS，再填写当前平台的 API Key 并保存。两个平台的 API Key 分开加密保存在 PostgreSQL，不会相互覆盖。创建任务时选择的平台、服务、国家、价格、购买数量、PIN 和代理池作为一份服务端草稿保存；完整草稿在写入 PostgreSQL 前加密，页面刷新或重新打开后从服务端恢复。浏览器不再使用 `localStorage` 保存 API Key、PIN、代理、任务草稿或活动任务标识；新版页面启动时只会删除旧版留下的相关 `localStorage` 键。API Key 不会写入 Docker Compose 文件。

随后按页面顺序选择服务、国家和价格，填写购买数量以及 6 位数字 PIN，再启动任务。计划购买数量的服务端允许范围为 1–100，超出范围的任务创建请求会被驳回。服务、国家、价格目录和购号请求均使用当前选中的平台。选择 HeroSMS 时，报价字段显示为“价格”，只展示其 `getPrices` 返回的数值和库存；该接口不返回账户币种，因此页面不会猜测币种，也不会显示、派生或提交 SMSBower 的供应商 ID 及 Bronze、Silver、Gold 档位。实际购号后，币种会按 HeroSMS `getNumberV2` 返回值规范化并保存。SMSBower 报价仍以 ₽ 展示。任务创建时会固化所选平台；后续在页面切换平台不会改变已有任务和号码的购买、收码、结算或取消去向。升级前创建且没有来源记录的旧任务继续按 SMSBower 处理。SMSBower 和 HeroSMS 的官方接口地址均由服务内置，页面不需要填写 Base URL。

“计划购买数量”表示任务最终需要完成的成功数（`fulfilled_count`），不是累计买到的号码上限。例如输入 `3`，任务会持续补购，直到有 3 个号码完成 GoPay 处理并进入 `fulfilled`。每个成功分配且落库的号码都会计入 `purchased_count` 并占用一个处理槽位；在首次进入 `fulfilled` 前，未注册、需要 PIN 登录、0 RP、`phone_in_use`、登录或改 PIN 验证码超时、其他处理失败、供应商取消或过期以及手动删除等终态结果都会释放该槽位，只要 `fulfilled_count` 尚未达标就会自动补购。因此 `purchased_count` 可能高于计划数量；`fulfilled_count` 达标后，任务自动进入 `completed` 并停止购买。已进入 `fulfilled` 的号码不再占用 `inflight` 槽位；后续收码阶段发生取消、过期或手动删除时，只结束或隐藏该已成功记录，保留 `fulfilled_count`，不减少 `inflight` 也不因此补购。供应商明确未分配号码的临时错误会退避重试，不计入 `purchased_count`；如果购号回执或号码落库结果未知，服务会冻结自动购买并保留待核实状态，避免重复请求造成重复扣费。

号码不再使用历史黑名单或指纹去重：服务不再维护 `phone_history`，也不会因号码曾经购买过就将新激活标记为 `duplicate`。为避免同一号码的两个激活共享或改写同一份 GoPay 会话，新号码落库时仍会检查当前未结束的激活：如果同一号码已有 `finished_at` 为空的记录，新分配会标记为 `phone_in_use`，自动调用 `setStatus=8` 取消，释放槽位并在成功数未达标时补购；原激活结束后，该号码即可再次使用。点击“删除”会取消号码并隐藏当前记录，不保留用于阻止以后处理的永久去重指纹。服务同一时间只允许运行一个任务，当前任务停止前会拒绝创建新任务。运行中任务、号码和下次执行时间都持久化在 PostgreSQL；服务重启后会恢复未完成流程，而不是停止任务或批量取消号码。若进程在购号请求的结果落库前中断，该购号尝试会保留为结果未知并冻结自动补购，避免重复扣费。

页面会展示号码状态、登录验证码、改 PIN 验证码和持续收到的后续验证码。SMSBower 仍每 2 秒调用 `getStatus` 查询验证码和激活状态。HeroSMS 的登录码、改 PIN 码和后续码则全部由 webhook 事件驱动，不调用 HeroSMS `getStatus`；回调会持久化并唤醒匹配的号码 worker。每次触发对应的 GoPay OTP 后，登录验证码等待超过 60 秒、改 PIN 验证码等待超过 80 秒且仍未收到验证码时，服务会先调用该任务所属平台的 `setStatus=3` 请求下一条短信，再重新触发相应的 GoPay OTP；每个验证码阶段最多重发 3 次（初次发送不计为重发）。第三次重发后再次等待超过对应时长仍无码，服务会调用同一平台的 `setStatus=8` 取消号码，并按阶段标记为“登录验证码失败”或“改 PIN 验证码失败”；该号码会释放处理槽位并触发补购。每次重发会在外部发码前持久化并计入预算；若发码结果与最新 OTP token 之间发生进程或数据库中断，该次仍会完整等待对应的 60 秒或 80 秒窗口且不会无计数重复发送，随后继续下一次或按三次上限取消。点击“停止任务”会停止后续购买，并为未完成号码排队执行取消（号码会从当前列表隐藏）；点击“成功”会结算号码。

余额符合条件后，服务会优先通过 GoPay profile 判断当前账号应执行 PIN setup 还是 reset；profile 状态未知或返回 404 时先尝试 setup，setup 返回 `GoPay-111`（账号已有 PIN）则自动切换到 reset。收到改 PIN 验证码后，服务会先验证 OTP 并持久化验证结果，再提交最终 PIN，重试时不会重复使用已消费的验证码。页面状态依次显示“正在设置 PIN”→“改 PIN 成功”→“等待后续验证码”，随后按接收顺序追加后续验证码。

仅当号码处于提交新 PIN（`setting_pin`）阶段，并收到结构化的 HTTP 403 / `GoPay-112` 错误时，服务会保留 GoPay 失败详情并立即调用号码所属平台的 `setStatus=6` 结算号码，而不是取消号码。若结算暂时失败，后续重试会继续执行结算动作；该分支不计为 PIN 修改成功，也不会进入 `fulfilled`。结算完成后会释放处理槽位，并在成功数未达标时自动补购。其他阶段收到 `GoPay-112` 时，仍按既有失败流程调用同一平台的 `setStatus=8` 取消号码，释放槽位后继续补购。

### HeroSMS 独立购号与接码

访问 `/hero-sms` 可进入不执行 GoPay 登录与 PIN 流程的独立页面，专门用于购买 HeroSMS 号码并接收验证码。页面会根据当前选择的服务、国家和验证方式加载实际可用报价；只有该组合支持租号时才显示可选时长，普通短期号码不要求选择时长。设置数量为 `N` 并创建后，系统会向现有列表追加 `N` 个持久化的独立号码任务，不覆盖旧任务，也不把多个号码合并成任务池；每个任务都可单独开始和停止。

每个等待购号的任务独立工作。HeroSMS 返回“暂无可用号码”时，该任务会继续退避重试，其他新建或已有任务不受影响。购号成功后，页面同时显示号码有效期倒计时和可退款倒计时：退款截止时间取“购买后 20 分钟”和“号码有效期”中的较早者。收到首条验证码后会立即标记为不可退款，但号码任务继续接收后续验证码；已购号码只在有效期届满，或用户手动停止并完成取消/结算后结束。

验证码以 webhook 推送为首选，同时每 45 秒执行一次低频轮询兜底。来自 webhook 与轮询的同一条验证码会跨来源幂等去重，相同内容在不同时间再次到达时仍会作为新消息保留。任务、购号状态、验证码、两个截止时间和下次重试时间都保存在 PostgreSQL 中，服务重启后会恢复等待购号及仍在有效期内的独立任务。HeroSMS 的同一份 webhook 回调会同时写入原有 GoPay 工作流的回调收件箱和独立购号任务的消息记录，两侧均使用幂等去重，重复投递不会重复处理。

GoPay 代理池在独立整行文本框中输入，每行一个地址；重复行会保留。支持以下格式：

```text
hostname:port:username:password
socks5://username:password@host:port
username:password@hostname:port
hostname:port@username:password
```

带协议头时支持 `http://`、`https://`、`socks5://`、`socks5h://`；不带协议头默认按 HTTP 处理。代理池是一次性的：系统随机分配未使用地址，某个代理端点一经被号码领取就会标记为已使用，重复输入的同一端点也会一并退出可用池；预检失败的地址同样不再使用。任务面板显示可用数/总数。如果用户提供了代理池且可用地址已耗尽，服务会先等待尚在处理的号码；当没有剩余处理中号码且没有未决购号预占，且成功数仍未达到计划数量时，任务会明确进入 `failed` 并停止继续购号，避免无代理可用时仍继续扣费。未配置代理池的直连模式不适用该耗尽规则。

## 数据与生命周期

业务数据以 PostgreSQL 为唯一持久化来源，包括 API Key、PIN、代理池、未提交的任务草稿、主工作流和 HeroSMS 独立页的任务/购号/号码状态、验证码历史、账号和 HeroSMS webhook 原始 JSON 及其处理状态。API Key、PIN、代理、任务草稿，以及主 GoPay 工作流的手机号、供应商原始载荷、验证码和 HeroSMS webhook 原文，都使用 `AUTOSMS_SECRET_KEY` 在服务端逐行加密后落库；主工作流中用于手机号精确查询和 webhook 幂等的摘要使用从同一主密钥按用途隔离派生的 HMAC 盲索引，避免从这些加密记录的数据库摘要离线枚举手机号或短验证码。调度所需的 ID、状态与时间字段保留为可索引列。升级时会在同一数据库迁移事务中加密旧明文、替换旧无密钥摘要并清空旧列，失败会整体回滚。HeroSMS 回调仍会完整保留接收到的原文字节，同时记录幂等盲索引、接收/处理/忽略状态、尝试次数和错误，用于可恢复处理与审计。

- `docker compose down`：停止并删除容器，保留 `autosms-data` 数据卷。
- `docker compose up --build`：重新构建并启动，继续使用原有数据和凭据，并恢复数据库中的活动任务与未完成号码。
- `docker compose down -v`：同时删除数据库、任务、webhook 原文/状态、草稿、账号、号码记录、API Key 和自动生成的凭据。此操作不可撤销。

### 备份与恢复

数据库内容必须与 `/data/runtime/db.env` 中的原始 `AUTOSMS_SECRET_KEY` 成对保存；只有 `pg_dump` 而没有这份密钥时，加密字段不可恢复。公网部署还应同时保存权限为 `0600` 的 `.env.public`，以保留 Basic Auth 和 webhook 凭据。最稳妥的方式是在短暂停机期间备份整个 `autosms-data` 卷，它会同时包含 PostgreSQL 数据目录和运行时密钥：

```bash
mkdir -p backups
container_id="$(docker compose --env-file .env.public -f compose.yaml -f compose.public.yaml ps -q autosms)"
data_volume="$(docker inspect "${container_id}" --format '{{range .Mounts}}{{if eq .Destination "/data"}}{{.Name}}{{end}}{{end}}')"
docker compose --env-file .env.public -f compose.yaml -f compose.public.yaml stop autosms
docker run --rm -v "${data_volume}:/source:ro" -v "${PWD}/backups:/backup" alpine:3.22 \
  tar -C /source -czf /backup/autosms-data.tgz .
docker compose --env-file .env.public -f compose.yaml -f compose.public.yaml start autosms
install -m 600 .env.public backups/env.public
```

恢复时先停止服务，将 `autosms-data.tgz` 解压到一个空的 `autosms-data` 卷，再放回配套的 `.env.public` 后启动；不要把归档覆盖到仍含其他数据库内容的卷。恢复完成后，应用会用归档中的原密钥校验并解密数据。

容器收到停止信号后，会先通知 Go 服务和 PostgreSQL 正常退出。任一进程异常结束时，入口脚本也会关闭另一个进程，Compose 的重启策略随后负责恢复服务。

升级包含购号协议变更的版本时，请先停止所有旧版容器或进程，再启动新版；新旧版本不应同时连接同一个数据库运行。新版会从 PostgreSQL 恢复活动任务和未完成激活；只有在停机时处于“购号已发送但结果未落库”的请求会转为结果未知并保留名额，防止重复购买。

## 环境变量

基础 Compose 为回环地址本地开发提供了默认鉴权值。公网 overlay 会强制显式设置密码和 webhook token；直接运行 Go 程序时，`AUTOSMS_PUBLIC_URL`、鉴权用户名/密码和 webhook token 也都是必填项。

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `AUTOSMS_DB_USER` | `autosms` | 内置数据库用户 |
| `AUTOSMS_DB_NAME` | `autosms` | 内置数据库名 |
| `AUTOSMS_DB_PASSWORD` | 自动生成 | 内置数据库密码 |
| `AUTOSMS_SECRET_KEY` | 自动生成 | API Key、Token 等敏感数据的服务端加密密钥 |
| `AUTOSMS_DB_PORT` | `5432` | 容器内部 PostgreSQL 端口，不会自动发布到宿主机 |
| `AUTOSMS_HTTP_ADDR` | `:8080` | Go 服务监听地址 |
| `AUTOSMS_HEALTH_URL` | `http://127.0.0.1:8080/readyz` | Docker 健康检查地址 |
| `AUTOSMS_PUBLIC_URL` | 本地 Compose：`http://localhost:8080` | 对外服务 origin；公网必须为不带路径的 HTTPS URL，只有 loopback 开发地址允许 HTTP |
| `AUTOSMS_AUTH_USERNAME` | 本地 Compose：`admin` | 管理页面和普通 API 的 Basic Auth 用户名 |
| `AUTOSMS_AUTH_PASSWORD` | 本地 Compose 有开发默认值 | Basic Auth 密码，至少 16 个字符；公网必须显式设置强随机值 |
| `AUTOSMS_HEROSMS_WEBHOOK_TOKEN` | 本地 Compose 有开发默认值 | HeroSMS webhook 路径密钥，至少 32 个 `A–Z`/`a–z`/`0–9`/`_`/`-` 字符；公网必须显式设置 |
| `AUTOSMS_POLL_INTERVAL` | `2s` | SMSBower 验证码/激活状态轮询间隔，也作为 HeroSMS webhook 处理暂时失败的初始重试基准；HeroSMS 主工作流不轮询 `getStatus`，独立购号页另以 45 秒低频轮询兜底 |
| `AUTOSMS_GOPAY_LOGIN_STATUS_TTL` | `4s` | GoPay 登录状态缓存窗口；略短于页面的 5 秒轮询，确保每轮可触发远端检查 |
| `AUTOSMS_ACTIVATION_TTL` | `20m` | 号码激活有效期 |
| `AUTOSMS_SMSBOWER_BASE_URL` | SMSBower 官方 handler 地址 | 仅服务端/测试环境覆盖 SMSBower endpoint，页面不会暴露此字段 |
| `AUTOSMS_HEROSMS_BASE_URL` | HeroSMS 官方 handler 地址 | 仅服务端/测试环境覆盖 HeroSMS endpoint，页面不会暴露此字段；兼容读取 `AUTOSMS_HERO_SMS_BASE_URL` |
| `AUTOSMS_GOPAY_SSO_BASE_URL` | GoPay 官方地址 | GoPay 登录 API 根地址，主要用于测试 |
| `AUTOSMS_GOPAY_BASE_URL` | GoPay 官方地址 | GoPay 业务 API 根地址，主要用于测试 |

数据库账号、密码或名称的显式覆盖会写回 `/data/runtime/db.env` 并在后续启动继续使用。`AUTOSMS_SECRET_KEY` 只在首次初始化时写入；已有持久密钥后，传入不同值会在修改文件和启动数据库之前直接退出，相同值或不传值会继续复用原密钥。当前版本不执行在线密钥轮换，生产环境必须连同数据备份并保持这份密钥不变。

宿主机 Web 端口可直接通过 Compose 变量调整：

```bash
AUTOSMS_PORT=18080 docker compose up --build
```

此时访问 `http://localhost:18080`。Compose 默认只绑定宿主机回环地址；公网请使用上述 Caddy HTTPS overlay，不要改为直接发布应用端口。

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

应用内部健康检查接口：

- `GET /healthz`：Go 进程存活状态；
- `GET /readyz`：服务及数据库可用状态。

应用路由中两者不需 Basic Auth，但公网 Caddy 会拦截 `/readyz`，Docker 健康检查仍通过容器内网访问它。

## 技术栈

- Go 1.25、Gin、pgx
- Vue 3、TypeScript、Element Plus、Vite
- PostgreSQL 16
- Docker Compose（单应用容器、单持久化数据卷）
