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

当前版本已包含真实手机 H.264 视频、浏览器受限控制、WebRTC 媒体通道、88FRP UDP 映射以及 Windows 安装包。视频默认固定为 720p/24fps，并提供 360p/10fps 至 1080p/30fps 的预设档，以及分辨率与帧率独立组合的自定义档；网络波动时不会自动切换视频档位，用户可按实际体验手动降低画质或帧率。控制优先使用内置 scrcpy 的原生二进制控制通道，支持连续拖动轨迹；通道尚未就绪或异常时会自动回退到常驻 ADB shell 控制。

TURN 服务器参数已经支持，但仍需用真实 TURN 服务完成强制中继验收。当前 88FRP UDP 路径属于 UDP 转发，不应在界面或文档中误称为公网 P2P。
