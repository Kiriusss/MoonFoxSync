# macOS 和 Windows 客户端

开发版的操作步骤和已实现范围见[快速开始](../快速开始.md)。开发按[实施规格](../docs/implementation/03-桌面与调度.md)和[HTTP 契约](../docs/implementation/openapi.yaml)执行。下文保留初期调研笔记。

建议用共享同步核心维护文件索引、持久化队列、版本冲突与传输进度。macOS 菜单栏、Windows 托盘显示当前网络模式、待同步大小和单文件推迟原因。用户可以选择文件或目录立即同步，也可以将某些文件固定为离线可用。

## 网络与调度

- **家中**。首次配置时登记 NAS 的局域网地址或本地服务名并绑定服务身份。运行时检查当前物理网络到该地址的路由是否符合登记的家庭路径，并直接连接该地址完成身份验证。符合条件时允许上传待传的大文件，并下载用户选择常驻或已在本机使用的更新。仅 NAS 文件只刷新目录。
- **外出**。通过 Tailscale 访问同一台服务。用户可选“仅手动”或“自动同步小文件”。按完整文件的逻辑大小判定，避免大文件拆块后被误当成小文件。10 MiB 可作为试运行阈值，两种模式均允许用户单独指定大文件立即传输。自动上传、自动下载共用此策略。
- **转网或离线**。网络变化时重新验证家庭路径，停止向旧连接提交新块。离开局域网时，回家才允许的大文件暂停，保留已确认的进度。手动例外只绑定本次选中的文件版本。离线修改保存在本机，联网后继续。

异地 NAT 穿透也可建立 direct 连接。Tailscale 的点对点或中继状态适合显示给用户，单凭该状态无法判断是否在家。家庭传输应使用通过验证的局域网连接。[Tailscale 连接类型](https://tailscale.com/docs/reference/connection-types)

## 释放本地空间

释放前比对本机当前版本和 NAS 的版本回执与内容摘要，并在实际回收时防止并发写入。文件有未上传的改动、正在写入、被固定离线或 NAS 无法核实时，保留本地文件。释放成功后仍记录文件身份、版本和“仅 NAS”状态。这项操作只改变当前设备的状态。其他电脑保持各自的缓存选择。

在系统管理的同步位置，macOS 通过 File Provider 回收本地内容，Windows 通过 Cloud Files API 管理占位文件。原有任意目录可以另做上传来源。首版在这些普通目录中仅扫描和上传，避免把尚未接入系统占位能力的普通文件当作可安全回收的缓存。外出时的索引器、缩略图预览等后台请求，同样需要服从大文件传输策略。[Apple File Provider](https://developer.apple.com/documentation/fileprovider/synchronizing-the-file-provider-extension) · [Windows Cloud Files API](https://learn.microsoft.com/en-us/windows/win32/cfapi/build-a-cloud-file-sync-engine)
