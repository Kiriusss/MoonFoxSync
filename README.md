# 心狐同步 · Moon Fox Sync

[下载客户端与 NAS 服务端](https://github.com/Kiriusss/MoonFoxSync/releases/latest) · [安装与配对](快速开始.md)

心狐同步面向一个家庭 NAS 和多台电脑。NAS 保存文件和历史版本。各台电脑可以独立决定哪些内容常驻本机、哪些只在需要时下载，以及什么网络条件下允许后台传输。Android 与原生鸿蒙客户端可按计划通过只读接口浏览 NAS 目录、项目历史并取回选定文件。

「梦枢天罗」是运行在 NAS 上的 Go 服务。桌面端把单个文件或整棵代码项目保存为一个版本，传输途中继续修改文件也能取回已保存的版本。已有 NAS 目录可以登记为只读资料库。在服务的 HTTPS 根地址可打开网页控制台，浏览目录和管理项目时间线。每个项目都可以单独设置历史策略、恢复以前的内容为当前版本，以及预览并删除时间点、重建保留版本。

梦枢天罗提供项目文件列表、单文件下载与整项目 ZIP 下载。桌面客户端可从菜单栏、托盘和传输队列点击「立即同步」上传当前队列，也可以查看各文件的进度和传输速度，默认备份 Git 忽略文件，也能核对后快进拉取或推送 Git 分支。在客户端可以分别移除本机记录、删除 NAS 资料库。桌面支持按来源设置检查间隔与网络策略、按需启用 Codex 对话历史备份、手动关联多机共享项目，以及预览并合并 NAS 历史分叉。同步概览和菜单栏或托盘展示最近活动项目、传输队列和驻留状态。安装和操作见[快速开始](快速开始.md)。完整目标见[开发计划](开发计划.md)，协议及验收样本见[实施规格](docs/implementation/README.md)。研发目录和兼容命令分别沿用 KiriusDrive、`kdrive` 与 `kdrive-server`。

## 项目内并行传输

在桌面端「设置」→「项目内并行传输」，可以分别设置一个项目同时上传和下载的文件数，范围为 1–50，默认各 4。新设置用于之后开始的上传与恢复任务。传输页的上传主进度显示整个快照已处理的逻辑字节和文件数量，明细列出当前文件与速率。NAS 已有的内容也计入项目进度，最后由 NAS 确认发布。

## 方案调研

- **飞牛同步** 是本机 fnOS 自带生态中最先值得核对的成品。其按需同步支持保留占位、释放空间和固定本地，官方文档明确标注 Mac 端暂不支持按需同步。[飞牛按需同步](https://help.fnnas.com/articles/v1/sync/on-demand-sync)
- **Seafile / SeaDrive** 最接近所需的桌面体验。Mac 用户可以在 Finder 移除下载，Windows 用户可以在资源管理器释放空间或固定离线文件。这个能力作用于 SeaDrive 管理的资料库。可以用它作为成品基线，比较释放后的行为、打开耗时和缓存占用。[Mac 客户端](https://help.seafile.com/drive_client/drive_client_for_macos/) · [Windows 客户端](https://help.seafile.com/drive_client/drive_client_for_win10/)
- **Nextcloud** 值得参考服务端接口和 Finder 集成。客户端通过 macOS File Provider 展示云端目录，支持离线保留和回收本地内容。服务端对外提供分块上传和文件版本恢复接口。如果决定复用成熟后端，可以先验证这两个接口。桌面客户端的“大目录确认”针对目录大小，无法直接表达外出时每个文件的传输规则。[Finder 集成](https://docs.nextcloud.com/server/stable/user_manual/en/desktop/macosfileprovider.html) · [客户端配置](https://docs.nextcloud.com/server/stable/user_manual/en/desktop/configfile.html) · [分块上传](https://docs.nextcloud.com/server/stable/developer_manual/client_apis/WebDAV/chunking.html) · [版本恢复](https://docs.nextcloud.com/server/stable/developer_manual/client_apis/WebDAV/versions.html)
- **git-annex** 把受版本管理的文件记录和可单独取回的文件内容分开。它支持按大小表达副本偏好，并能在释放前核实是否还有足够副本。KiriusDrive 的目标更具体，释放时必须核实指定 NAS 已有本机这次修改的完整版本。[副本检查](https://git-annex.branchable.com/copies/) · [内容策略](https://git-annex.branchable.com/git-annex-preferred-content/)
- **Syncthing** 值得借鉴分块哈希、重用已有块和并发修改时保留冲突文件的设计。普通双向同步会传播删除，因此移除本机副本需要单独设计。[同步机制](https://docs.syncthing.net/users/syncing.html) · [常见问题](https://docs.syncthing.net/users/faq.html)
- **群晖 Drive** 给出了一个具体的功能边界。它有按需同步，也有大小过滤，但官方明确说明 macOS 按需同步任务不支持按文件大小过滤。[群晖同步任务说明](https://kb.synology.com/en-global/DSM/tutorial/How_to_sync_files_between_Synology_NAS_and_your_computer_using_Drive_desktop)

上述文档中没有找到同时按家庭局域网身份、单文件大小和用户手动选择来调度双向传输的现成配置。建议优先自研网络调度和本地副本管理，两端通过版本化接口通信。文件系统集成采用操作系统提供的接口。

如果希望直接修改桌面端实现，可评估 Nextcloud 的开源桌面客户端。Seafile 维护方曾明确说明 SeaDrive 的开源版本面向 Linux。Mac 和 Windows 的 SeaDrive 在本项目中作为体验参考。[Nextcloud 桌面端源码](https://github.com/nextcloud/desktop) · [Seafile 维护方说明](https://forum.seafile.com/t/open-source-seadrive/21804)

