# 原生已发送邮件 Portal API

Status: in-progress

## Problem Statement

v154 登录器包含原生已发送邮件恢复功能，会向 `POST /portal/api/native-sent-mail` 请求已发送邮件记录。当前 Portal 未注册该路由，统一返回 404，导致登录器无法从服务端恢复已发送邮件列表。

## Solution

在现有 Portal 会话认证下，按当前设备绑定的角色返回其发件箱记录。支持按收件人分页、按邮件序列向前翻页，并保留邮件正文、附件标识和收件人已读状态。响应字段与 v154 登录器内嵌的 mailcache 数据结构一致。

## User Stories

1. 作为玩家，我希望按收件人查看自己发出的邮件，以便恢复和浏览发送记录。
2. 作为玩家，我希望在同一收件人的邮件很多时继续向前翻页，并在翻页期间固定数据上界。
3. 边界：请求不能指定或伪造 owner；只能读取当前 Portal 会话角色作为 sender 的邮件。
4. 边界：无收件人、无邮件、无附件时返回空数组，不返回 null；无效分页参数返回 400；过期或跨设备会话返回 401。

## Implementation Decisions

- 入口：`internal/portal` 注册 `POST /portal/api/native-sent-mail`，沿用现有 Bearer 会话和来源主机绑定。
- 请求字段：`through`、`afterPeer`、`peer`、`before`、`read`。
- 响应字段：`schema`、`owner`、`through`、`afterPeer`、`peers`、`morePeers`、`peer`、`before`、`read`、`hasMore`、`messages`；消息记录包含 `sequence`、`recipient`、`item`，并保留 mailcache 的 `meta`、`status` 与附件字段。
- 数据：复用玩家库现有 `mail` 表的 `sender`、`uid`、`id`、`read`、`items_json`、`created_ts`；不新增表、不修改静态只读库。
- 分页：`through=0` 时固定当前 sender 的最大邮件 ID；`afterPeer` 对收件人列表分页；`before` 对指定收件人的邮件倒序分页。页大小固定，响应返回下一页游标。
- 已读过滤：`read=-1` 返回全部，`read=0` 返回未读，`read=1` 返回已读；消息 `status` 对应现有 mail.read。
- 兼容：不改游戏 TCP 命令和旧 Portal 路由；回滚只需移除新路由及新增文件。
- 证据：[事实] v154 登录器二进制包含 `/portal/api/native-sent-mail`，并包含 `rf4local/internal/mailcache.Request`、`Page`、`Record`、`Item`、`Meta`、`Attachment` 类型及其字段；`Request` 为 POST JSON 使用，响应使用严格 JSON 解码。[事实] 现有 `mailbox.go` 将邮件 ID 表示为 32 位十六进制 GUID，邮件协议 type 固定为 0。[推断] `through/afterPeer/peer/before/read` 的分页和筛选语义按字段职责实现，尚待登录器实机联调确认。

## Testing Decisions

- 存储层：使用真实 SQLite 测试 sender、recipient、through、before、read 筛选及分页边界。
- HTTP 行为层：测试 Bearer 会话、跨设备拒绝、owner 不可由请求指定、严格请求字段、响应字段与空数组。
- 期望值来自：现有 mail 表字段、现有 `mailbox.go` 邮件 GUID/type 口径，以及 v154 登录器二进制中的 mailcache 类型定义。
- 实机联调：部署后使用 v154 登录器实际调用该路由，确认其严格解码与恢复流程通过；未完成联调前不宣称实机兼容已验证。

## Out of Scope

- `native-purchase/gold` 与 `native-purchase/premium` 真实支付/充值流程。
- 新增游戏 TCP 协议命令、聊天样式、横幅、客户端外观资源及本地 Launcher UI。
- 修改现有邮件发送、领取、删除协议行为。

## Further Notes

- v154 登录器基线：`rf4-4.0.25034-lan-v154-required-launcher`。
- 本改动只实现已从登录器二进制确认的原生已发送邮件读取接口；不把二进制字符串中出现的其他功能标识当作已经验证的服务端协议。
