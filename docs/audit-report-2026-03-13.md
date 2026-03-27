# Agentserver 全面审计报告

**日期:** 2026-03-13
**版本:** v0.20.0
**审计范围:** 代码安全 + 代码规范
**审计方法:** 静态代码分析（7个并行审计维度）

---

## 目录

1. [执行摘要](#1-执行摘要)
2. [严重级别定义](#2-严重级别定义)
3. [发现汇总](#3-发现汇总)
4. [Critical 级别发现](#4-critical-级别发现)
5. [High 级别发现](#5-high-级别发现)
6. [Medium 级别发现](#6-medium-级别发现)
7. [Low 级别发现](#7-low-级别发现)
8. [正面发现](#8-正面发现)
9. [修复优先级建议](#9-修复优先级建议)

---

## 1. 执行摘要

Agentserver 是一个多租户 AI 编码代理平台，包含三个核心服务（主API服务器、LLM代理、沙箱代理），支持 Docker 和 Kubernetes 部署。本次审计覆盖了认证授权、API安全、LLM代理、沙箱隔离、代码质量、基础设施部署、前端安全共7个维度。

### 关键统计

| 严重级别 | 数量 |
|---------|------|
| Critical | 7 |
| High | 21 |
| Medium | 35 |
| Low | 25 |

### 高风险领域

- **认证与会话管理**: 令牌明文存储、会话令牌通过URL泄露、登出未失效令牌
- **沙箱隔离**: Docker Socket挂载导致宿主机逃逸风险、K8s NetworkPolicy默认关闭
- **输入验证**: 无请求体大小限制、无速率限制
- **基础设施**: 容器以root运行、数据库连接未加密、默认弱密码

---

## 2. 严重级别定义

| 级别 | 定义 |
|------|------|
| **Critical** | 可被直接利用导致数据泄露、系统完全接管或大规模服务中断 |
| **High** | 可被利用但需要特定条件，或可与其他漏洞组合造成严重影响 |
| **Medium** | 存在安全风险但利用条件较高，或影响有限 |
| **Low** | 最佳实践偏差，潜在风险较低 |

---

## 3. 发现汇总

### 按维度分布

| 审计维度 | Critical | High | Medium | Low |
|---------|----------|------|--------|-----|
| 认证与授权 | 4 | 7 | 5 | 4 |
| API安全 | 0 | 4 | 8 | 2 |
| LLM代理 | 0 | 2 | 4 | 3 |
| 沙箱隔离 | 1 | 5 | 7 | 3 |
| 代码质量 | 2 | 3 | 6 | 8 |
| 基础设施 | 0 | 7 | 9 | 4 |
| 前端安全 | 0 | 1 | 4 | 4 |

> 注：部分发现在多个维度中被独立发现，已去重合并。

---

## 4. Critical 级别发现

### C-01: 会话令牌明文存储于数据库

- **文件:** `internal/db/tokens.go:9-17`, `internal/db/migrations/001_initial.sql:26`
- **描述:** 会话令牌（auth_tokens表）以明文形式作为PRIMARY KEY存储。一旦数据库被泄露（SQL注入、备份泄露、从节点被攻破），攻击者可直接冒充任意用户。
- **影响:** 数据库泄露 = 全部活跃会话被劫持
- **修复建议:** 存储令牌的SHA-256哈希值。验证时先哈希再查找。

### C-02: 所有沙箱令牌明文存储

- **文件:** `internal/db/migrations/001_initial.sql:83-87`, `internal/db/sandboxes.go:207-218`
- **描述:** proxy_token、opencode_token、openclaw_token、tunnel_token均以明文存储在sandboxes表中。
- **影响:** 数据库泄露导致所有沙箱凭证暴露
- **修复建议:** 对所有令牌进行SHA-256哈希后存储。

### C-03: 用户会话令牌通过URL参数泄露

- **文件:** `internal/server/server.go:425-427`, `internal/sandboxproxy/opencode_proxy.go:49`
- **描述:** 用户的完整会话cookie值被嵌入沙箱URL中（`/auth?token=<session_token>`）。该URL出现在浏览器历史、服务器访问日志、HTTP Referer头中。泄露的令牌可用于劫持用户的整个会话（不仅是沙箱会话）。
- **影响:** 令牌通过多个渠道泄露，攻击者可完全接管用户账户
- **修复建议:** 实现短期、一次性的授权码(authorization code)专门用于沙箱重定向流程。主会话令牌永远不应出现在URL中。

### C-04: 登出未在服务端失效令牌

- **文件:** `internal/server/server.go:310-322`
- **描述:** handleLogout仅清除客户端cookie，不从auth_tokens表中删除令牌。通过C-03捕获的令牌在用户登出后仍可使用长达7天。
- **影响:** 会话劫持无法通过登出缓解
- **修复建议:** 在logout handler中添加 `DELETE FROM auth_tokens WHERE token = $1`。

### C-05: Docker Socket 挂载导致宿主机完全沦陷风险

- **文件:** `docker-compose.yml:38`
- **描述:** agentserver容器挂载 `/var/run/docker.sock`。任何有权访问Docker socket的进程等效于宿主机root权限。若agentserver应用本身被攻破（依赖漏洞、SSRF等），攻击者可创建特权容器挂载宿主文件系统，完全逃逸到宿主机。
- **影响:** 服务端任意漏洞 = 宿主机完全沦陷
- **修复建议:** 使用Docker Socket代理（如docker-socket-proxy）限制可用API调用；或使用rootless Docker-in-Docker。

### C-06: crypto/rand.Read 错误未检查（认证令牌生成）

- **文件:** `internal/auth/auth.go:66`
- **描述:** `rand.Read(b)` 的错误返回值被忽略。若系统CSPRNG故障，令牌将被填充为零字节，产生可预测的认证令牌。
- **影响:** 在极端情况下（CSPRNG降级），所有新生成的令牌可被预测
- **修复建议:** 检查错误返回值：`if _, err := rand.Read(b); err != nil { return "", err }`

### C-07: crypto/rand.Read 错误未检查（OIDC state参数）

- **文件:** `internal/auth/oidc.go:84`
- **描述:** OIDC登录流程中 `rand.Read(stateBytes)` 的错误未检查。CSPRNG故障会产生可预测的OAuth state参数，使CSRF攻击成为可能。
- **影响:** OIDC认证流程可被CSRF攻击劫持
- **修复建议:** 检查错误并在失败时返回HTTP 500。

---

## 5. High 级别发现

### H-01: 无速率限制 -- 登录端点

- **文件:** `internal/server/server.go:121,235-252`
- **描述:** `/api/auth/login` 无速率限制，允许无限次密码暴力破解。虽然bcrypt提供计算成本，但对弱密码仍可在合理时间内破解，且高并发请求会导致CPU耗尽。
- **修复建议:** 添加per-IP和per-username速率限制（如10次/分钟），多次失败后临时锁定。

### H-02: 无速率限制 -- 注册端点

- **文件:** `internal/server/server.go:254-298`
- **描述:** `/api/auth/register` 无速率限制，攻击者可创建无限账户。首个用户自动成为admin（竞态条件，见H-04）。
- **修复建议:** 按IP限制注册频率，考虑添加CAPTCHA或邀请制注册。

### H-03: 无密码复杂度要求

- **文件:** `internal/server/server.go:264-267`, `internal/auth/auth.go:32-41`
- **描述:** 注册仅检查密码非空，无最小长度要求。用户可使用"1"或"a"作为密码。
- **修复建议:** 强制最少8字符，考虑检查常见密码列表。

### H-04: 首个用户admin提升存在竞态条件

- **文件:** `internal/server/server.go:288-292`, `internal/auth/oidc.go:243-245`
- **描述:** 检查 `CountUsers() == 1` 以提升首个用户为admin，该操作非原子性。两个用户同时注册时均可能看到 `count == 1`，导致两个admin。
- **修复建议:** 使用原子数据库操作：`UPDATE users SET role = 'admin' WHERE id = $1 AND (SELECT COUNT(*) FROM users) = 1`

### H-05: 子域名Cookie缺少Secure标志

- **文件:** `internal/sandboxproxy/opencode_proxy.go:71-78`, `internal/sandboxproxy/openclaw_proxy.go:48-55`
- **描述:** `oc-token` 和 `claw-token` cookie未设置 `Secure: true`，可通过HTTP明文传输被截获。主认证cookie正确设置了此标志。
- **修复建议:** 添加 `Secure: true`。

### H-06: 角色未经验证的权限提升

- **文件:** `internal/server/server.go:754-792,794-816`
- **描述:** handleAddMember和handleUpdateMemberRole不验证角色值是否为合法白名单值。maintainer可以将成员设置为"owner"角色，或注入任意角色字符串。
- **修复建议:** 验证角色属于 `["owner", "maintainer", "developer"]`。禁止maintainer分配owner角色。

### H-07: 无CSRF保护

- **文件:** `internal/server/server.go:103-233`
- **描述:** 所有状态变更操作仅依赖 `SameSite=Lax` cookie，无CSRF令牌。沙箱子域名可能发起跨子域CSRF攻击。
- **修复建议:** 要求所有变更请求包含 `Content-Type: application/json` header检查（HTML表单无法设置此Content-Type）。

### H-08: 无HTTP请求体大小限制

- **文件:** `cmd/serve.go:215`, `internal/server/server.go` (所有JSON decode调用)
- **描述:** HTTP服务器未配置 `MaxHeaderBytes`，handler中未使用 `http.MaxBytesReader`。攻击者可发送数GB的请求体导致OOM。LLM代理正确使用了限制（10MB）。
- **修复建议:** 全局添加请求体大小限制中间件，或在每个handler中使用 `http.MaxBytesReader`。

### H-09: 无HTTP服务器超时配置

- **文件:** `cmd/serve.go:215`, `cmd/llmproxy/main.go:44-47`
- **描述:** `http.Server` 未配置 `ReadTimeout`、`WriteTimeout`、`ReadHeaderTimeout`、`IdleTimeout`。易受slowloris攻击。
- **修复建议:** 设置 `ReadHeaderTimeout: 10s`, `ReadTimeout: 30s`, `WriteTimeout: 120s`, `IdleTimeout: 120s`。

### H-10: K8s NetworkPolicy默认关闭

- **文件:** `deploy/helm/agentserver/values.yaml:94-97`
- **描述:** `networkPolicy.enabled: false` 是默认值。默认部署的沙箱Pod可访问集群内任何服务，包括Kubernetes API、PostgreSQL、其他命名空间Pod。
- **修复建议:** 默认值改为 `enabled: true`，并提供合理的 `denyCIDRs` 默认列表。

### H-11: 无云元数据端点阻断

- **文件:** `internal/namespace/manager.go:158-176`
- **描述:** 即使启用NetworkPolicy，默认 `denyCIDRs` 为空，沙箱Pod可访问 `169.254.169.254` 云元数据服务，暴露IAM凭证。
- **修复建议:** 默认deny列表中添加 `169.254.169.254/32`。

### H-12: K8s沙箱容器无SecurityContext

- **文件:** `internal/sandbox/manager.go:247-261,434-461`
- **描述:** K8s沙箱Pod的主容器未设置SecurityContext：无 `runAsNonRoot`、无 `allowPrivilegeEscalation: false`、无 `capabilities.drop: ["ALL"]`。Docker后端正确做了这些设置。
- **修复建议:** 添加SecurityContext，与Docker后端保持一致的安全配置。

### H-13: K8s NetworkPolicy缺少Ingress规则

- **文件:** `internal/namespace/manager.go:186-196`
- **描述:** NetworkPolicy仅指定Egress规则，无Ingress规则。任何集群Pod可直接访问沙箱Pod，无需通过sandboxproxy。
- **修复建议:** 添加Ingress规则，仅允许来自sandboxproxy的流量。

### H-14: gVisor运行时默认未启用

- **文件:** `deploy/helm/agentserver/values.yaml:83`
- **描述:** `runtimeClassName` 默认为空，沙箱使用标准runc运行。不受信任的AI代理代码在无额外隔离的容器中运行。
- **修复建议:** 默认启用gVisor，或在未配置时强制显示安全警告。

### H-15: 生产容器以root运行

- **文件:** `Dockerfile:28-41`, `Dockerfile.llmproxy:10-15`, `Dockerfile.sandboxproxy:17-24`
- **描述:** agentserver、llmproxy、sandboxproxy的Dockerfile均未设置非root用户。仅Dockerfile.opencode正确创建了非root用户。
- **修复建议:** 在每个生产Dockerfile中添加非root用户并使用 `USER` 指令。

### H-16: 默认PostgreSQL弱密码

- **文件:** `deploy/helm/agentserver/values.yaml:14-16`, `docker-compose.yml:4-6`
- **描述:** 默认数据库凭证为 `agentserver/agentserver`，硬编码于Helm chart和docker-compose中。
- **修复建议:** 要求操作员显式设置密码，或使用Helm random函数生成随机密码。

### H-17: 所有数据库连接使用sslmode=disable

- **文件:** `docker-compose.yml:32,50`, `deploy/helm/agentserver/templates/_helpers.tpl:9`
- **描述:** 所有数据库连接字符串使用 `sslmode=disable`。集群内数据库凭证和查询数据以明文传输。
- **修复建议:** 配置PostgreSQL TLS，连接字符串改用 `sslmode=require`。

### H-18: /internal/ 命名空间无应用层认证（LLM代理）

- **文件:** `internal/llmproxy/server.go:48-57`
- **描述:** LLM代理的 `/internal/` 路由（使用量、追踪、配额管理）完全依赖网络隔离，无任何认证。若暴露到更广网络，任何客户端可读取使用数据、修改配额（设置max_rpd为0等于无限配额）。
- **修复建议:** 添加共享密钥或mTLS认证。

### H-19: /internal/validate-proxy-token 无认证

- **文件:** `internal/server/server.go:114`
- **描述:** 主服务器的 `/internal/validate-proxy-token` 端点注册在认证中间件之外，无任何认证。可被用于探测有效的proxy token和枚举沙箱元数据。
- **修复建议:** 添加共享密钥认证，或限制仅内部网络访问。

### H-20: CI/CD无容器镜像漏洞扫描

- **文件:** `.github/workflows/build.yml`
- **描述:** GitHub Actions工作流构建并推送Docker镜像，无任何漏洞扫描步骤。
- **修复建议:** 在push前添加Trivy/Grype扫描步骤，Critical/High CVE时阻断构建。

### H-21: 测试覆盖率几乎为零

- **文件:** 项目全局
- **描述:** 整个项目仅有1个测试文件（`internal/agent/config_test.go`，244行）。API处理器、认证流程、数据库层、LLM代理、隧道协议、沙箱管理、配额执行等关键路径均无测试。
- **修复建议:** 优先为以下路径添加测试：(1)认证中间件 (2)配额执行 (3)隧道协议编解码 (4)资源解析函数 (5)LLM代理流式拦截器。

---

## 6. Medium 级别发现

### M-01: RPD配额检查存在TOCTOU竞态条件

- **文件:** `internal/llmproxy/anthropic.go:44-57,225-252`
- **描述:** RPD检查（SELECT COUNT）和使用记录（INSERT）不是原子操作。高并发请求可全部通过配额检查后才记录，显著超出限制。
- **修复建议:** 使用原子check-and-increment操作。

### M-02: SSRF风险 -- 可配置的上游URL

- **文件:** `internal/llmproxy/config.go:26`, `internal/llmproxy/anthropic.go:93-119`
- **描述:** `ANTHROPIC_BASE_URL` 无验证。若被配置为攻击者控制的URL，真实API Key（通过x-api-key header注入）将泄露给攻击者。
- **修复建议:** 启动时验证上游URL属于允许的主机列表。

### M-03: 响应体无大小限制

- **文件:** `internal/llmproxy/anthropic.go:146`, `internal/llmproxy/auth.go:38`
- **描述:** 多处使用 `io.ReadAll` 读取响应体无大小限制，恶意上游可导致OOM。
- **修复建议:** 使用 `io.LimitReader`。

### M-04: OIDC账户通过email自动关联（无验证）

- **文件:** `internal/auth/oidc.go:211-228`
- **描述:** 新OIDC身份按email自动关联现有用户，若OIDC提供者不验证email，攻击者可声明相同email接管账户。Generic OIDC提供者未检查 `email_verified` claim。
- **修复建议:** 仅在OIDC提供者确认email已验证时才自动关联。

### M-05: 无OIDC nonce参数

- **文件:** `internal/auth/oidc.go:76-98`
- **描述:** OIDC登录流程未使用nonce参数防止ID Token重放攻击。
- **修复建议:** 生成随机nonce，包含在授权请求中，并在回调时验证。

### M-06: OIDC state参数非常量时间比较

- **文件:** `internal/auth/oidc.go:115`
- **描述:** OAuth state参数使用 `!=` 直接比较，存在时序侧信道风险。
- **修复建议:** 使用 `crypto/subtle.ConstantTimeCompare`。

### M-07: 注册响应泄露用户存在信息

- **文件:** `internal/server/server.go:270-278`
- **描述:** 注册返回"username already taken"(409)，可用于用户名枚举。
- **修复建议:** 返回统一的错误消息。

### M-08: 令牌失效未随角色变更

- **文件:** `internal/server/admin.go:163-185`
- **描述:** admin降级用户角色时，现有会话令牌仍有效7天。被降级用户保持admin权限直到令牌过期。
- **修复建议:** 角色变更时失效该用户的所有活跃令牌。

### M-09: 最后一个admin可被自我降级

- **文件:** `internal/server/admin.go:163-185`
- **描述:** admin可将自己降级为普通用户，若是最后一个admin则导致系统无管理员。
- **修复建议:** 降级前检查是否为最后一个admin。

### M-10: 无输入长度验证（注册字段）

- **文件:** `internal/server/server.go:264-267`
- **描述:** 用户名、email、密码无最大长度限制。超长密码（如1MB）会消耗bcrypt计算资源（虽然bcrypt截断至72字节）。
- **修复建议:** 验证用户名(3-64字符)、email(基本格式)、密码(8-128字符)。

### M-11: WebSocket接受任意Origin

- **文件:** `internal/sandboxproxy/tunnel.go:44-46`
- **描述:** WebSocket使用 `InsecureSkipVerify: true` 禁用Origin检查，可能导致跨站WebSocket劫持。
- **修复建议:** 配置 `OriginPatterns` 仅允许预期的基础域名。

### M-12: 缺少HTTP安全头

- **文件:** `internal/server/server.go:103-233`
- **描述:** 服务器未设置安全头：X-Frame-Options、X-Content-Type-Options、CSP、HSTS、Referrer-Policy。
- **修复建议:** 添加安全头中间件。

### M-13: 无CSP（内容安全策略）

- **文件:** `web/index.html`
- **描述:** 前端无CSP meta标签，也无服务器端CSP头。
- **修复建议:** 添加CSP头限制脚本、样式、连接来源。

### M-14: 管理员列表端点无分页

- **文件:** `internal/server/admin.go:34-161`
- **描述:** admin列表端点（users、workspaces、sandboxes）返回所有记录，无分页。大量数据时可导致OOM。
- **修复建议:** 添加分页参数并限制最大页大小。

### M-15: 错误信息泄露内部细节

- **文件:** `internal/server/admin.go:355,482`
- **描述:** admin端点将数据库错误详情直接返回给客户端，可能泄露表名、约束信息等。
- **修复建议:** 日志记录完整错误，客户端返回通用错误消息。

### M-16: SSRF风险 -- 查询参数注入

- **文件:** `internal/server/server.go:1296-1358`
- **描述:** `limit` 和 `offset` 用户输入直接拼接到LLM代理内部请求URL中，未作整数解析。
- **修复建议:** 先用 `strconv.Atoi` 解析，再用 `url.Values` 构建URL参数。

### M-17: 开放注册无任何验证

- **文件:** `internal/server/server.go:254-298`
- **描述:** 任何人可注册账户，无email验证、CAPTCHA或邀请要求。
- **修复建议:** 添加关闭注册的选项（邀请制/管理员审批）。

### M-18: RPD配额绕过（无数据库时）

- **文件:** `internal/llmproxy/anthropic.go:225-252`
- **描述:** 无数据库连接时，即使设置了DefaultMaxRPD，配额检查也会放行。无警告日志。
- **修复建议:** 无数据库且配额大于0时，拒绝请求（fail closed）或使用内存计数器。

### M-19: 用户控制的img src属性（追踪像素风险）

- **文件:** `web/src/components/TopBar.tsx:27`, `web/src/components/AdminPanel.tsx:184`
- **描述:** OIDC头像URL直接用作img标签的src。攻击者可设置追踪像素检测管理员访问。
- **修复建议:** 通过后端代理头像图片，或限制允许的图片域名。

### M-20: Docker后端exec泄露环境变量

- **文件:** `internal/container/manager.go:304-308`
- **描述:** Docker exec通过fork docker CLI执行，传递了服务器完整环境变量（含DATABASE_URL、ANTHROPIC_API_KEY）。
- **修复建议:** 使用Docker API（ContainerExecCreate/ExecAttach）替代CLI fork。

### M-21: K8s sandbox Pod未禁用ServiceAccount自动挂载

- **文件:** `internal/sandbox/manager.go:245-265`
- **描述:** sandbox Pod未设置 `automountServiceAccountToken: false`。加上sandbox镜像安装了kubectl，用户可能利用SA token与K8s API交互。
- **修复建议:** 在PodSpec中添加 `AutomountServiceAccountToken: boolPtr(false)`。

### M-22: 同一workspace内Pod间通信未限制

- **文件:** `internal/namespace/manager.go:138-142`
- **描述:** NetworkPolicy允许同namespace内所有Pod互访。被攻破的sandbox可攻击同workspace的兄弟sandbox。
- **修复建议:** 使用更精细的PodSelector限制Pod间通信。

### M-23: ClusterRole权限过宽

- **文件:** `deploy/helm/agentserver/templates/rbac.yaml:14-35`
- **描述:** agentserver使用ClusterRole（非namespaced Role），可在任何namespace中exec Pod、管理NetworkPolicy。
- **修复建议:** 考虑按workspace命名空间动态创建Role。

### M-24: Helm中数据库密码明文暴露在Pod Spec

- **文件:** `deploy/helm/agentserver/templates/llmproxy.yaml:34-36,52`
- **描述:** PostgreSQL密码通过Helm模板插值直接出现在Pod spec命令中和环境变量value中。
- **修复建议:** 使用Kubernetes Secret引用（secretKeyRef）。

### M-25: 沙箱无临时存储(ephemeral-storage)限制

- **文件:** `internal/container/manager.go:213-217`, `internal/sandbox/manager.go:255-259`
- **描述:** 容器设置了CPU/内存/PID限制，但无磁盘配额。sandbox可写满节点磁盘。
- **修复建议:** K8s添加 `ephemeral-storage` 资源限制。

### M-26: 前端全局性错误静默吞没

- **文件:** `web/src/App.tsx`, `web/src/components/` (20+处)
- **描述:** 几乎所有catch块为空。关键操作（删除sandbox、暂停、恢复等）失败时用户无反馈。
- **修复建议:** 添加用户可见的错误通知，401响应重定向到登录页。

### M-27: server.go 过大（1414行God File）

- **文件:** `internal/server/server.go`
- **描述:** 单文件包含Server结构体、路由、所有handler、辅助函数，难以维护。
- **修复建议:** 按领域拆分：`auth_handlers.go`, `workspace_handlers.go`, `sandbox_handlers.go`。

### M-28: sandbox/manager.go 大量代码重复

- **文件:** `internal/sandbox/manager.go`
- **描述:** Start方法和StartContainerWithIP方法包含约300行近乎相同的代码。
- **修复建议:** 提取共享的 `buildSandboxSpec` 辅助函数。

### M-29: sbxstore错误静默（无法区分"未找到"和"数据库错误"）

- **文件:** `internal/sbxstore/store.go:70-71,103-104`
- **描述:** Get方法在查询出错时返回 `(nil, false)`，调用者无法区分"不存在"和"数据库故障"。
- **修复建议:** 向调用者传播error。

### M-30: 混合日志方式

- **文件:** 项目全局
- **描述:** 主服务器使用 `log.Printf`（stdlib），LLM代理使用 `slog.Logger`（结构化JSON）。格式不统一影响日志聚合。
- **修复建议:** 全项目统一使用 `slog`。

### M-31: CI/CD无SAST、lint、测试步骤

- **文件:** `.github/workflows/build.yml`
- **描述:** 构建流水线无 `go vet`、`golangci-lint`、`gosec`、`go test` 步骤。
- **修复建议:** 在build-and-push前添加质量和安全检查job。

### M-32: 基础镜像标签未锁定

- **文件:** 所有Dockerfile
- **描述:** 使用 `node:25-slim`、`golang:1.26-trixie`、`debian:trixie-slim` 等可变标签而非SHA256 digest。构建不可复现。
- **修复建议:** 使用SHA256 digest固定基础镜像。

### M-33: 无.dockerignore文件

- **文件:** 项目根目录
- **描述:** 无.dockerignore，`COPY . .` 会将 `.git/`、`.env`、文档等全部复制到构建层。
- **修复建议:** 创建.dockerignore排除非必要文件。

### M-34: Ingress TLS默认关闭

- **文件:** `deploy/helm/agentserver/values.yaml:140`
- **描述:** `ingress.tls: false` 是默认值。启用ingress但未显式设置TLS时，登录凭证以明文传输。
- **修复建议:** 默认启用TLS，或在tls=false时添加验证警告。

### M-35: 隧道令牌被日志记录

- **文件:** `internal/agent/client.go:120`
- **描述:** WebSocket URL（含token查询参数）被log输出，隧道认证令牌出现在日志中。
- **修复建议:** 记录日志时脱敏token值。

---

## 7. Low 级别发现

| ID | 文件 | 描述 |
|----|------|------|
| L-01 | `internal/auth/auth.go:33` | bcrypt使用DefaultCost(10)，建议升至12 |
| L-02 | `internal/auth/auth.go:16-17` | 会话令牌7天TTL无轮换机制 |
| L-03 | `internal/server/server.go:254-298` | 注册无email验证 |
| L-04 | `internal/db/tokens.go:35-41` | DeleteExpiredTokens存在但未被调用，过期令牌累积 |
| L-05 | `internal/db/sandboxes.go:276-306` | 过期注册码未定期清理 |
| L-06 | `internal/db/sandboxes.go:207-209` | Proxy token SQL查找非常量时间比较 |
| L-07 | `internal/llmproxy/server.go:46` | HandleFunc接受所有HTTP方法，非/messages路径不限流 |
| L-08 | `internal/llmproxy/trace.go:18-33` | 客户端可控的Trace ID未经清理直接存储 |
| L-09 | `internal/sandbox/manager.go:673-678` | shortID截断至8字符，约77000个sandbox有50%碰撞概率 |
| L-10 | `internal/container/config.go:22` | Docker默认bridge网络，sandbox间可互访 |
| L-11 | `web/src/App.tsx:239-242` | 前端Admin路由无客户端权限守卫 |
| L-12 | `web/src/components/Login.tsx:147` | OIDC provider名未编码直接拼入URL |
| L-13 | `web/src/main.tsx` | 无React Error Boundary |
| L-14 | `web/vite.config.ts` | 未显式禁用生产环境source maps |
| L-15 | 多处 | `envOrDefault` 函数重复定义4次 |
| L-16 | 多处 | `nullIfEmpty`、`shortID`、`parseK8sMemoryBytes`、`buildRESTConfig` 重复 |
| L-17 | `internal/sandbox/manager.go:680` | 未使用函数 `strPtr` |
| L-18 | `internal/auth/auth.go:44` | Login返回bool而非error，无法区分"凭证无效"和"数据库错误" |
| L-19 | 多处 | 角色、状态使用字符串字面量而非类型常量 |
| L-20 | 多处 | JSON响应使用 `map[string]interface{}` 而非类型化结构体 |
| L-21 | `internal/server/server.go:312,467` | Cookie名硬编码而非使用已定义常量 |
| L-22 | `internal/server/admin.go:203-290` | handleAdminSetQuotaDefaults包含9个重复if块 |
| L-23 | `cmd/serve.go:45-233` | cobra Run闭包188行，难以测试 |
| L-24 | `main.go:1-4` | 未填写的版权模板占位符 |
| L-25 | `Dockerfile.opencode:13-22` | sandbox镜像包含大量开发工具（Go、Rust、C/C++），攻击面较大 |

---

## 8. 正面发现

以下方面表现良好，值得肯定：

1. **SQL注入防护**: 所有SQL查询均使用参数化语句，未发现字符串拼接
2. **密码哈希**: 使用bcrypt哈希密码（非明文存储）
3. **登录响应**: 不区分"用户不存在"和"密码错误"，防止登录枚举
4. **前端XSS防护**: 所有用户数据通过JSX自动转义
5. **前端令牌存储**: 使用HttpOnly cookie，localStorage仅用于主题偏好
6. **前端无敏感数据**: 无API key或令牌硬编码在前端代码中
7. **外部链接安全**: 所有外部链接使用 `target="_blank" rel="noopener noreferrer"`
8. **路径遍历防护**: 静态文件使用 `path.Clean` 和 `embed.FS`，安全
9. **RBAC执行**: workspace成员角色在所有端点正确检查，防止跨租户访问
10. **LLM代理令牌生成**: 使用 `crypto/rand` 生成128位熵的令牌
11. **LLM代理日志卫生**: 不记录proxy token或API key
12. **TypeScript严格模式**: 前端启用strict mode和多项安全编译选项
13. **React严格模式**: 在main.tsx中启用StrictMode
14. **依赖精简**: 前端依赖集小而精（React 19、react-router、Tailwind）

---

## 9. 修复优先级建议

### 第一优先级 -- 立即修复（Critical + 高影响High）

| 编号 | 发现 | 预估工作量 |
|------|------|----------|
| C-03 | 会话令牌从URL参数改为一次性授权码 | 中等 |
| C-04 | 登出时服务端删除令牌 | 极小 |
| C-01/02 | 令牌哈希存储 | 中等 |
| C-06/07 | crypto/rand错误检查 | 极小 |
| H-08 | 添加请求体大小限制 | 小 |
| H-09 | 配置HTTP服务器超时 | 极小 |
| H-01/02 | 认证端点速率限制 | 小 |
| H-05 | 子域名cookie添加Secure标志 | 极小 |
| H-06 | 角色白名单验证 | 小 |

### 第二优先级 -- 近期修复（High + 高影响Medium）

| 编号 | 发现 | 预估工作量 |
|------|------|----------|
| H-10/11 | NetworkPolicy默认启用 + 云元数据阻断 | 小 |
| H-12/13 | K8s sandbox SecurityContext + Ingress规则 | 小 |
| H-15 | 容器非root运行 | 小 |
| H-07 | CSRF保护（Content-Type检查） | 小 |
| M-12 | HTTP安全头中间件 | 小 |
| M-04 | OIDC email验证检查 | 小 |
| H-18/19 | 内部API添加共享密钥认证 | 中等 |

### 第三优先级 -- 计划修复

| 编号 | 发现 | 预估工作量 |
|------|------|----------|
| H-03 | 密码复杂度要求 | 小 |
| H-17 | 数据库TLS连接 | 中等 |
| H-16 | 移除默认数据库密码 | 小 |
| M-01 | RPD配额原子操作 | 中等 |
| H-20 | CI添加镜像漏洞扫描 | 小 |
| H-21 | 关键路径测试覆盖 | 大 |
| M-27/28 | 代码组织重构 | 中等 |

---

*本报告由静态代码分析生成，建议结合动态安全测试（渗透测试）进一步验证发现。*
