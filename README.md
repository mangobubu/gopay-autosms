# AutoSMS

AutoSMS 将 SMSBower 号码购买、GoPay 已有号码登录、余额判断、PIN 设置与后续验证码轮询整合到一个网页工作台。后端使用 Go + Gin，前端使用 Vue 3 + Element Plus，数据保存在 PostgreSQL 16。

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

打开工作台后，先在“SMSBower 配置”中填写 API Key 并保存。API Key 仍会由服务加密后保存在 PostgreSQL，同时也会以明文持久化到当前浏览器的本地存储；创建购买任务时填写的服务、国家、价格、购买数量、PIN 和代理池等表单内容也会作为明文草稿保存在浏览器本地，刷新或重新打开页面后自动恢复。浏览器本地存储中的这些数据可被同源页面脚本访问，请只在可信设备和可信部署环境中使用，并注意浏览器账户及扩展的安全。API Key 不会写入 Docker Compose 文件。

随后按页面顺序选择服务、国家和价格，填写购买数量以及 6 位数字 PIN，再启动任务。“购买数量”表示供应商成功分配且已落库的号码总数上限，而不是最终成功处理的号码数量；每个已买到并落库的号码都会占用一个名额，无论后续被判定为重复、未注册、需要 PIN 登录、0 RP、过期、处理失败，还是被手动删除，均计入购买数量且不会补购。只有供应商尚未分配号码的临时错误可以重试，此类失败不计入购买数量。服务同一时间只允许运行一个任务，当前任务停止前会拒绝创建新任务；服务每次启动都会先停止数据库中遗留的运行中任务，并取消其未完成号码。SMSBower 官方接口地址由服务内置，不需要在页面填写 Base URL。页面会展示号码状态、登录验证码、改 PIN 验证码和持续收到的后续验证码。任务持有号码期间每 2 秒轮询一次；点击“停止任务”会停止后续购买，并为未完成号码排队执行取消（号码会从当前列表隐藏）；点击“成功”会结算号码，点击“删除”会取消并隐藏记录，同时永久保留号码指纹用于以后去重。

余额符合条件后，服务会优先通过 GoPay profile 判断当前账号应执行 PIN setup 还是 reset；profile 状态未知或返回 404 时先尝试 setup，setup 返回 `GoPay-111`（账号已有 PIN）则自动切换到 reset。收到改 PIN 验证码后，服务会先验证 OTP 并持久化验证结果，再提交最终 PIN，重试时不会重复使用已消费的验证码。页面状态依次显示“正在设置 PIN”→“改 PIN 成功”→“等待后续验证码”，随后保持轮询并按接收顺序追加后续验证码。

GoPay 代理池在独立整行文本框中输入，每行一个地址；重复行会保留。支持以下格式：

```text
hostname:port:username:password
socks5://username:password@host:port
username:password@hostname:port
hostname:port@username:password
```

带协议头时支持 `http://`、`https://`、`socks5://`、`socks5h://`；不带协议头默认按 HTTP 处理。系统随机分配代理，单个号码占用期间不会分配给另一个号码；预检失败的地址会移出池，任务面板显示可用数/总数。

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
| `AUTOSMS_POLL_INTERVAL` | `2s` | SMSBower 验证码轮询间隔 |
| `AUTOSMS_GOPAY_LOGIN_STATUS_TTL` | `4s` | GoPay 登录状态缓存窗口；略短于页面的 5 秒轮询，确保每轮可触发远端检查 |
| `AUTOSMS_ACTIVATION_TTL` | `20m` | 号码激活有效期 |
| `AUTOSMS_SMSBOWER_BASE_URL` | SMSBower 官方 handler 地址 | 仅服务端/测试环境覆盖 SMSBower endpoint，页面不会暴露此字段 |
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
