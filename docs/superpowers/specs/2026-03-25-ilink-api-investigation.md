# iLink API 调研报告

基于 `@tencent-weixin/openclaw-weixin@2.0.1` 源码分析。

## 核心发现

### 1. 消息接收：Long-Polling 模式（非 Webhook）

iLink **不支持 Webhook 推送**。消息接收使用 **long-polling** 模式。

**API 端点：** `POST {baseUrl}/ilink/bot/getupdates`

```typescript
// 请求
{
  get_updates_buf: string,  // 上次返回的 cursor（首次为空字符串）
  base_info: { channel_version: string }
}

// 响应
{
  ret: number,              // 0=成功
  errcode?: number,         // 错误码（-14=session过期）
  errmsg?: string,
  msgs: WeixinMessage[],    // 新消息列表
  get_updates_buf: string,  // 新的 cursor，下次请求带上
  longpolling_timeout_ms?: number  // 服务端建议的下次 poll 超时
}
```

**工作方式（`monitor.ts`）：**
- 客户端持续循环调用 `getUpdates`，携带上次返回的 `get_updates_buf`
- 服务端 hold 住请求直到有新消息或超时（默认 35 秒）
- 客户端超时时返回空响应 `{ret:0, msgs:[]}`，然后重试
- `get_updates_buf` 持久化到磁盘，重启后恢复

### 2. 消息发送

**API 端点：** `POST {baseUrl}/ilink/bot/sendmessage`

```typescript
// 请求
{
  msg: {
    from_user_id: "",           // 留空
    to_user_id: string,         // 目标用户 ID（xxx@im.wechat 格式）
    client_id: string,          // 客户端生成的消息 ID
    message_type: 2,            // BOT=2
    message_state: 2,           // FINISH=2
    context_token?: string,     // 从 getUpdates 收到的 context_token（必须回传）
    item_list: [{
      type: 1,                  // TEXT=1, IMAGE=2, VOICE=3, FILE=4, VIDEO=5
      text_item?: { text: string },
      image_item?: { media: CDNMedia, mid_size: number },
      // ... 其他媒体类型
    }]
  },
  base_info: { channel_version: string }
}
```

**认证方式：**
```
POST /ilink/bot/sendmessage
Content-Type: application/json
Authorization: Bearer {bot_token}
AuthorizationType: ilink_bot_token
X-WECHAT-UIN: {random_base64}
```

### 3. 认证流程（QR 扫码登录）

与 agentserver 现有的 `ilink.go` 实现一致：

1. `GET {baseUrl}/ilink/bot/get_bot_qrcode?bot_type=3` → 返回 `{qrcode, qrcode_img_content}`
2. `GET {baseUrl}/ilink/bot/get_qrcode_status?qrcode={qrcode}` → long-poll 返回状态
3. 确认后返回 `{status:"confirmed", bot_token, ilink_bot_id, baseurl, ilink_user_id}`

**bot_token** 是后续所有 API 调用的认证凭证。

### 4. Context Token 机制

每条 inbound 消息携带 `context_token`，outbound 回复**必须**回传对应的 `context_token`。这是 iLink 用于维持会话上下文的机制。

- 收到消息时：从 `WeixinMessage.context_token` 中提取
- 存储：按 `accountId:userId` 对缓存（内存 + 磁盘持久化）
- 发送时：必须附带目标用户最新的 `context_token`
- 无 context_token 时仍可发送，但可能丢失上下文

### 5. 其他 API 端点

| 端点 | 用途 |
|------|------|
| `POST /ilink/bot/getconfig` | 获取 bot 配置（含 typing_ticket） |
| `POST /ilink/bot/sendtyping` | 发送"正在输入"状态 |
| `POST /ilink/bot/getuploadurl` | 获取 CDN 上传 URL（用于发送图片/视频/文件） |

### 6. 用户 ID 格式

- 微信用户 ID：`xxx@im.wechat`
- Bot ID（登录后获得）：`hex@im.bot`（如 `abc123@im.bot`）

## 对 agentserver 桥接架构的影响

### 原设计假设 vs 实际情况

| 原设计假设 | 实际情况 |
|-----------|---------|
| iLink 支持 Webhook 推送 | **不支持**，只有 Long-Polling |
| agentserver 被动接收消息 | agentserver 需要**主动轮询** |
| 简单的 HTTP 转发 | 需要维护 polling 循环 + cursor + context_token |

### 修订后的桥接架构

```
微信用户发消息
    ↓
iLink 服务器（hold 住直到有消息）
    ↑↓ long-poll
agentserver 轮询循环（per sandbox）
    ↓ HTTP POST
NanoClaw Pod weixin channel
    ↓
NanoClaw 消息循环 → Claude Agent SDK → 生成回复
    ↓ HTTP callback
agentserver → iLink sendmessage API → 微信用户
```

### 需要在 agentserver 实现的新功能

1. **Per-sandbox polling 循环**
   - 每个有 WeChat 绑定的 nanoclaw sandbox 启动一个 goroutine
   - 循环调用 `POST /ilink/bot/getupdates`
   - 维护 `get_updates_buf` cursor（存 DB 或内存）
   - 处理 session expired（errcode -14）、重试、backoff

2. **Context Token 管理**
   - 从 getUpdates 响应中提取 `context_token`
   - 按 sandbox+user 对存储
   - 发送回复时附带

3. **消息发送 API**
   - `POST /ilink/bot/sendmessage`
   - 构造 `SendMessageReq` with `WeixinMessage`
   - 附带认证 headers（`Authorization: Bearer {bot_token}`）
   - 附带 `context_token`

4. **Typing 状态**（可选）
   - 需要先调 `getConfig` 获取 `typing_ticket`
   - 再调 `sendTyping` 发送状态

### agentserver ilink.go 需要新增的函数

```go
// GetUpdates 执行一次 long-poll 获取新消息
func (c *Client) GetUpdates(botToken, getUpdatesBuf string) (*GetUpdatesResponse, error)

// SendMessage 发送文本消息
func (c *Client) SendMessage(botToken, toUserID, text, contextToken string) error

// SendTyping 发送"正在输入"状态（可选）
func (c *Client) SendTyping(botToken, ilinkUserID, typingTicket string) error

// GetConfig 获取 bot 配置（含 typing_ticket）
func (c *Client) GetConfig(botToken, ilinkUserID, contextToken string) (*ConfigResponse, error)
```

### DB 新增字段

```sql
-- sandbox_weixin_bindings 表新增（已在 migration 009 中）
bot_token TEXT          -- iLink bot 认证 token
ilink_base_url TEXT     -- iLink API base URL

-- 新增：per-binding 的 polling 状态
ALTER TABLE sandbox_weixin_bindings ADD COLUMN get_updates_buf TEXT;
ALTER TABLE sandbox_weixin_bindings ADD COLUMN last_poll_at TIMESTAMPTZ;

-- 新增：context token 存储
CREATE TABLE weixin_context_tokens (
    sandbox_id TEXT NOT NULL REFERENCES sandboxes(id) ON DELETE CASCADE,
    bot_id TEXT NOT NULL,
    user_id TEXT NOT NULL,
    context_token TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (sandbox_id, bot_id, user_id)
);
```

## 工作量评估

| 模块 | 复杂度 | 说明 |
|------|--------|------|
| iLink API 客户端（getUpdates, sendMessage） | 中 | 参考 openclaw-weixin 源码，直接翻译为 Go |
| Per-sandbox polling 循环 | 高 | 需要 goroutine 生命周期管理、cursor 持久化、错误恢复 |
| Context Token 管理 | 中 | 新建 DB 表 + 内存缓存 |
| NanoClaw weixin channel | 低 | 简单 HTTP server，接收/发送消息 |
| Typing 状态 | 低 | 可选，后续实现 |
