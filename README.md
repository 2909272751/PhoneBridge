# PhoneBridge

PhoneBridge 是一个面向 Android USB/ADB 设备的浏览器远程控制产品。分享端只暴露所选手机，不提供电脑桌面访问。

当前阶段已包含：

- ADB 设备发现与可读状态；
- 分享时长、访问码和权限设置；
- 临时分享链接；
- 独立的浏览器手机控制界面；
- 会话停止、过期与刷新恢复；
- 演示设备模式，用于没有连接真实手机时验证完整 UI 流程。

## 本地运行

```powershell
go run ./cmd/phonebridge --demo
```

然后访问 `http://127.0.0.1:8787`。

真实设备模式：

```powershell
adb devices -l
go run ./cmd/phonebridge
```

手机需要提前开启 USB 调试并在手机上确认当前电脑的 ADB 授权。

PhoneBridge 随软件内置官方 scrcpy Windows 组件，并将 scrcpy 与其同目录的 ADB 一并使用；用户无需单独安装 scrcpy。组件版本、来源和 SHA-256 校验记录在 `third_party/scrcpy/manifest.json`。

## Windows 安装包

安装包位于 `dist/installer`。双击安装后会安装到 Windows 的程序目录，自动包含 PhoneBridge、scrcpy 与 ADB；安装完成可立即启动并打开本地管理页。以后从开始菜单的“打开 PhoneBridge”启动即可，设置保存在当前 Windows 用户的配置目录，不会写入安装目录。

开发者需要重新构建安装包时，可执行：

```powershell
.\scripts\build-installer.ps1
```

## 当前边界

浏览器控制界面和会话闭环已经可运行。真实手机视频、控制协议、WebRTC P2P、TURN 回退和 Windows 安装包属于下一纵切，不能用演示画面替代验收。
