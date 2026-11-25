# 心狐同步 · Moon Fox Sync Android 只读阅读端

Android 0.1.1 随系统语言显示中文或英文应用名。客户端使用 Kotlin 和 Android 原生 View，支持 Android 8.0（API 26）及以上，可在手机上查看梦枢天罗服务端已授权的资料库。托管资料库支持逐版本浏览，外部只读目录直接浏览 NAS 当前内容。

## 构建与安装

安装 JDK 17、Android SDK Platform 36、Build Tools 35.0.0 及 Platform Tools。设置 `ANDROID_HOME`，或在本目录的 `local.properties` 写入 `sdk.dir`。在本目录执行以下命令。Windows 可使用 `gradlew.bat`。

```sh
./gradlew :app:assembleDebug
adb install -r app/build/outputs/apk/debug/app-debug.apk
```

Gradle Wrapper 版本为 8.14.3。首次构建需从 Google Maven、Maven Central 和 Gradle 分发站下载依赖。该命令生成使用本地调试证书签名的 APK。

## 连接资料库

1. 在 NAS 上用管理员 CLI 为目标资料库签发一次性只读配对码。
   ```sh
   kdrive-server pair issue --config <配置文件> --unit <资料库ID> --role reader
   ```
2. 手机连接可访问 NAS 的家庭网络或 Tailscale。输入与证书 SAN 匹配的 HTTPS 服务地址。如果 NAS 使用私有证书颁发机构，点击“选择 NAS CA 证书”，从手机文件中选取管理员提供的 PEM 公共证书。
3. 输入配对码并打开资料库。点选项目、快照和文件夹可查看历史内容。文件夹和快照列表可分页。设备令牌使用 Android Keystore 创建的 AES-GCM 密钥加密保存；应用禁止系统备份。
4. 常用代码和 UTF-8 文本按每次 64 KiB 加载，预览上限为 1 MiB。图片和 PDF 可下载后在应用内阅读。其他格式可用系统阅读器打开。下载大于 10 MiB 的文件需确认。完整下载会核对 SHA-256、字节数和 HTTP ETag，再向阅读器授予单个缓存文件的临时只读权限。
5. 顶部“设备”可以清理本地预览缓存或解除本机配对。管理员可执行 `kdrive-server device revoke --config <配置文件> --id <设备ID>` 撤销该设备在 NAS 上的授权。

本目录交付 Android APK。HarmonyOS NEXT 需要独立的原生应用与 HAP 包。

## 文件格式与边界

- 应用内文本预览按 UTF-8 解码，支持 txt、md、json、go、rs、js、ts、kt、c、cpp、h、py、log、xml、yaml、yml、csv、tex、ini、toml、sh、swift、cs。每次续读会携带上一段的版本标识，文件变化时需要重新打开。
- 应用内图片预览支持 JPEG、PNG、WebP、BMP，解码时缩小大图。PDF 逐页显示。这两类格式都需要先完整下载。
- 音视频、Office 文档、压缩包等可交由设备中已安装的对应阅读器打开。当前手机端提供浏览与下载，本机文件夹同步、上传和后台更新由桌面客户端承担。各厂商 Android 系统、鸿蒙及实体手机仍需独立验证。

## 用 Android 模拟器复现阅读流程

1. 在 Android Studio Device Manager 创建 Android 15（API 35）模拟器并启动。安装上面构建的调试 APK。以独立测试资料库运行 Go 服务，使用桌面 CLI 为该库同步一个带有 sample.txt 的命名快照。记录测试文件内容以便比对。
2. 在 Mac 本机监听 127.0.0.1:29876，测试证书的 IP SAN 包含 127.0.0.1。通过 adb 将模拟器相同端口转发给 Mac，并把公开 CA 证书放到模拟器的“下载”目录。
   ```sh
   adb reverse tcp:29876 tcp:29876
   adb push <公共CA证书.pem> /sdcard/Download/xinhu-ca.pem
   adb shell am start -n local.kirius.xinhusync/.ReaderActivity
   ```
3. 在手机界面输入 `https://127.0.0.1:29876` 和测试资料库的只读配对码，选择下载目录中的 PEM。依次打开资料库、命名快照、sample.txt，核对应用内文本与源文件一致。
4. 点“下载完整文件并打开”。Android 应显示文件处理应用的选择器。在调试 APK 上，可通过 `adb shell run-as local.kirius.xinhusync find cache/viewer -name sample.txt` 得到缓存文件路径，再用 `adb shell run-as local.kirius.xinhusync cat <缓存文件路径>` 比对所有字节。
5. 重启应用验证设备配对保留。选择“设备 → 解除配对 → 移除”，检查退回配对页并移除缓存。结束测试后可执行 `adb reverse --remove tcp:29876`。

本次在 API 35 ARM64 模拟器上使用真实 Go 客户端写入快照，并通过原生界面选择 PEM、只读配对、按范围读回原文、完整下载并逐字节核对。覆盖安装后配对可继续使用，系统阅读器选择器可以打开。在本机解除配对后，应用退回配对页且预览缓存已经清空。实际手机与家庭网络的路由、厂商阅读器兼容性需要在对应设备上验收。
