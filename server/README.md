# 梦枢天罗

梦枢天罗是心狐同步的 NAS 服务端，提供 HTTPS 文件读取和项目快照保存。兼容命令名为 `kdrive-server`。当前版本实现受管资料库、管理员显式登记的外部只读目录、按设备授权、对象查缺、可重试分批上传、完整快照发布、分叉保留、归档 hold、两快照路径差异与可识别的移动、分页历史及增量游标。完整目标契约见 [OpenAPI](../docs/implementation/openapi.yaml)。

## 启动与配对

首次使用独立空数据目录，准备证书及配置。证书须覆盖客户端实际验证的服务名称或地址。配置含 `formatVersion: 1`、`dataDir`、`listen` 及 `tls.certFile`、`tls.keyFile`，完整示例在[工程与命令行](../docs/implementation/01-工程与命令行.md)。私钥与配置应由服务账户持有。管理员命令在 NAS 本机运行。

```sh
kdrive-server init --config /etc/kiriusdrive/config.json
kdrive-server unit add --config /etc/kiriusdrive/config.json --kind managed --name 项目资料
kdrive-server unit add --config /etc/kiriusdrive/config.json --kind external --root /vol2/共享资料 --name 只读资料
kdrive-server serve --config /etc/kiriusdrive/config.json
```

在另一终端签发一次性配对码：`kdrive-server pair issue --config <配置> --unit <资料库ID> --role sync`。手机使用 `reader`。新增授权使用 `kdrive-server unit grant --config <配置> --unit <资料库ID> --device <设备ID> --role reader`；撤销使用 `kdrive-server device revoke --config <配置> --id <设备ID>`。配对码最多十分钟有效，设备在可信 TLS 连接上提交自己的凭据，NAS 只保存校验值。

## 手机浏览器

在已信任 NAS 证书的鸿蒙或安卓浏览器中打开 `https://服务地址/reader/`，输入管理员签发的 `reader` 配对码即可查看已授权资料库。受管资料库可切换历史版本，外部目录显示 NAS 上的现有文件。每页最多 40 项，点击「加载更多」翻页。文本预览最多读取开头 64 KiB 并按 UTF-8 显示。下载由浏览器保存原文件。

浏览器使用本域的 HTTPS、HttpOnly 会话 Cookie，刷新后可继续浏览。点击「退出配对」会在服务端撤销本次设备授权。页面、头像和脚本均由 NAS 提供。

## 数据与行为

- 受管数据存放于 `dataDir/objects`，元数据与授权关系位于 `dataDir/state/meta.db`。初始化生成卷身份标志；服务启动同时核对 SQLite 记录和卷标志。运行中每个请求检查标志仍在。
- 发布时服务端遍历引用闭包、检查权限和整文件摘要，再用单个数据库事务推进主线或留下分叉。另一台机器能在发布后读取完整版本。重复 finalize 返回同一持久回执。
- `/changes` 和历史、资料库分页游标绑定已授权范围，心跳式清单对账不触发文件正文读取。受管文件支持单区间 Range 与 If-Match。Git bundle 附件可通过同步角色读取。
- 领取长期 hold 前按版本和路径复验对象。版本可按项目策略或手动预览删除。有效 hold 保护对应时间点，底层对象仍需预留存储空间。备份可在正常关闭后复制整个数据目录，另存证书与配置。
- 外部只读目录按登记时的文件系统身份固定，逐级通过目录描述符打开以拒绝跟随符号链接。仅支持普通文件读取。每次 GET 会在 NAS 上形成临时副本并再次回读校验，保证响应有强 ETag。最多同时处理两次外部文件固化，临时数据随响应关闭释放。频繁随机读取超大文件宜优先采用受管快照。
- 本版本需管理员本机建立单元。跨单元链接发布、自动清理、后台跨快照哈希缓存属于后续开发项。


## 网页控制台与项目时间线

服务在 HTTPS 根路径提供梦枢天罗控制台，静态资源随 Go 二进制分发。浏览器用一次性配对码获得当前资料库授权，使用同源、HttpOnly 会话；登录最多持续 24 小时。原有手机只读阅读器位于 `/reader/`。控制台支持版本恢复、保存策略与带预览的删除重建。时间点的独立对象引用和可见父链在同一事务更新，校验失败会回滚。可见历史清理与物理存储回收是独立操作，当前版本保留底层去重对象。
