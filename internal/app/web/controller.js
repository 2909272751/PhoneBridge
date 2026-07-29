const $ = (selector) => document.querySelector(selector);
const token = window.location.pathname.split("/").filter(Boolean).pop();
let session = null;
let joined = false;
let viewerToken = "";
let countdownTimer = null;
let pollTimer = null;
let pointerDown = false;
let frameTimer = null;
let fallbackRunning = false;
let peerConnection = null;
let dataChannel = null;
let webRTCFrame = null;
let currentFrameURL = null;
let fallbackPauseTimer = null;
let fallbackRetryTimer = null;
let selectedProfile = "hd";
let transportHint = "direct";
let directProbe = false;
let transportMonitorTimer = null;
let transportLastBytes = null;
let transportLastAt = null;
let pendingPointerMove = null;
let pointerMoveTimer = null;
let profileSwitchTimer = null;
let profileSwitching = false;
let reconnectTimer = null;
let reconnectAttempts = 0;
let diagnosticLast = null;
let webRTCStarting = false;
// Start every capable browser on the real-time H.264 track. The screenshot
// renderer remains visible until the first decoded video frame and is only a
// recovery path; making it the default adds several seconds of visible control
// latency on a public FRP route.
let renderMode = "native";

async function api(url, options = {}) {
  const viewerHeaders = viewerToken ? { "X-PhoneBridge-Viewer": viewerToken } : {};
  const response = await fetch(url, {
    headers: { "Content-Type": "application/json", ...viewerHeaders, ...(options.headers || {}) },
    ...options,
  });
  const payload = await response.json().catch(() => ({}));
  if (!response.ok) {
    const error = new Error(payload.error || `请求失败（${response.status}）`);
    error.status = response.status;
    throw error;
  }
  return payload;
}

async function loadSession() {
  try {
    session = await api(`/api/public/sessions/${encodeURIComponent(token)}`);
    $("#join-loading").classList.add("hidden");
    $("#join-device-name").textContent = session.deviceName;
    $("#join-description").textContent = session.isDemo
      ? "这是产品演示会话。你只能访问演示手机界面，电脑桌面始终不可见。"
      : "你只能访问这台手机，无法查看或控制分享者的电脑。";
    $("#join-form").classList.remove("hidden");
    if (!session.requireCode) {
      $("#access-code").closest("label")?.classList.add("hidden");
      $("#access-code").classList.add("hidden");
    } else {
      $("#access-code").focus();
    }
  } catch (error) {
    showJoinError(error.message, true);
  }
}

async function joinSession(event) {
  event.preventDefault();
  const button = event.submitter;
  button.disabled = true;
  button.textContent = "正在建立安全连接……";
  $("#join-error").classList.add("hidden");
  try {
    const code = $("#access-code").value.replace(/\D/g, "");
    const result = await api(`/api/public/sessions/${encodeURIComponent(token)}/join`, {
      method: "POST",
      body: JSON.stringify({ code }),
    });
    session = result;
    viewerToken = result.viewerToken || "";
    joined = true;
    showController();
  } catch (error) {
    $("#join-error").textContent = error.message;
    $("#join-error").classList.remove("hidden");
  } finally {
    button.disabled = false;
    button.textContent = "连接设备";
  }
}

function showController() {
  selectedProfile = session.streamProfile && session.streamProfile !== "auto" ? session.streamProfile : "hd";
  if ([...$("#stream-profile").options].some((option) => option.value === selectedProfile)) {
    $("#stream-profile").value = selectedProfile;
  } else {
    selectedProfile = "hd";
    $("#stream-profile").value = "hd";
  }
  $("#join-page").classList.add("hidden");
  $("#controller-shell").classList.remove("hidden");
  $("#controller-device-name").textContent = session.deviceName;
  $("#connection-chip").innerHTML = session.isDemo
    ? "<i></i>演示连接"
    : "<i class=\"warning\"></i>兼容预览";
  $("#latency-chip").textContent = session.isDemo ? "UI纵切" : "真机画面";
  $("#demo-screen").classList.toggle("hidden", !session.isDemo);
  $("#device-video").classList.remove("visible");
  $("#fallback-frame").classList.toggle("visible", !session.isDemo);
  updateRenderModeUI();
  $("#video-placeholder").classList.toggle("hidden", session.isDemo);
  $("#control-status").textContent = session.mode === "control"
    ? "当前由你控制 · 电脑桌面始终不可见"
    : "当前为仅查看模式 · 控制操作已禁用";
  if (session.mode !== "control") {
    document.body.classList.add("view-only");
  }
  updateCountdown();
  countdownTimer = setInterval(updateCountdown, 1000);
  pollTimer = setInterval(pollSession, 1500);
  if (!session.isDemo) {
    startFallbackFrames();
    void startWebRTC();
  }
  $("#phone-screen").focus();
}

function updateRenderModeUI() {
  const button = $("#render-mode-toggle");
  if (!button) return;
  const compatibility = renderMode === "compatibility";
  button.textContent = compatibility ? "低延迟视频" : "兼容画面";
  button.title = compatibility
    ? "当前使用兼容画面，点此尝试低延迟 WebRTC 视频"
    : "当前使用低延迟 WebRTC 视频，点此切回兼容画面";
}

function useCompatibilityPreview() {
  renderMode = "compatibility";
  const video = $("#device-video");
  video.classList.remove("visible");
  $("#fallback-frame").classList.add("visible");
  $("#video-placeholder").classList.remove("hidden");
  $("#video-placeholder strong").textContent = "正在获取兼容实时画面";
  $("#video-placeholder small").textContent = "此设备的浏览器将使用兼容画面，控制仍保持实时。";
  startFallbackFrames();
  updateRenderModeUI();
}

function tryNativeVideo() {
  renderMode = "native";
  updateRenderModeUI();
  const video = $("#device-video");
  $("#video-placeholder").classList.remove("hidden");
  $("#video-placeholder strong").textContent = "正在尝试低延迟视频";
  $("#video-placeholder small").textContent = "若画面仍未出现，可点“兼容画面”立即恢复。";
  video.play().catch(() => {});
  if (video.readyState >= HTMLMediaElement.HAVE_CURRENT_DATA) {
    showNativeVideo(video);
  }
}

function showNativeVideo(video) {
  if (renderMode !== "native" || video.readyState < HTMLMediaElement.HAVE_CURRENT_DATA) return;
  clearTimeout(fallbackPauseTimer);
  clearTimeout(fallbackRetryTimer);
  video.classList.add("visible");
  $("#fallback-frame").classList.remove("visible");
  $("#video-placeholder").classList.add("hidden");
  stopFallbackFrames();
  finishProfileSwitch(true);
}

async function pollSession() {
  try {
    const current = await api(`/api/public/sessions/${encodeURIComponent(token)}`);
    session = { ...session, ...current };
    hideReconnect();
  } catch (error) {
    if (error.status === 409) {
      endSession("此连接已被新的设备接管");
      return;
    }
    if (error.status === 404 || error.status === 410) {
      endSession("分享已经结束或到期");
      return;
    }
    showReconnect();
  }
}

async function sendControl(type, detail = {}) {
  if (!joined || session.mode !== "control") return;
  if (dataChannel?.readyState === "open") {
    try {
      dataChannel.send(JSON.stringify({ kind: "control", type, ...detail }));
      return;
    } catch {
      // The HTTP control endpoint remains available while a data channel heals.
    }
  }
  try {
    await api(`/api/public/sessions/${encodeURIComponent(token)}/events`, {
      method: "POST",
      body: JSON.stringify({ type, ...detail }),
    });
    if (session.isDemo) animateDemoControl(type, detail);
  } catch (error) {
    if (error.status === 409) {
      endSession("此连接已被新的设备接管");
    } else if (error.status === 410) {
      endSession("分享已经结束或到期");
    } else {
      showReconnect();
    }
  }
}

function setupPhoneInput() {
  const screen = $("#phone-screen");
  const normalizedPoint = (event) => {
    const rect = screen.getBoundingClientRect();
    return {
      x: Math.max(0, Math.min(1, (event.clientX - rect.left) / rect.width)),
      y: Math.max(0, Math.min(1, (event.clientY - rect.top) / rect.height)),
    };
  };
  screen.addEventListener("pointerdown", (event) => {
    pointerDown = true;
	  pendingPointerMove = null;
    screen.setPointerCapture(event.pointerId);
    sendControl("pointer-down", normalizedPoint(event));
  });
  screen.addEventListener("pointermove", (event) => {
    if (!pointerDown) return;
    event.preventDefault();
    // scrcpy's native control channel consumes real move events. Coalesce them
    // to 30fps so a lossy ordered data channel cannot build a stale backlog.
    pendingPointerMove = normalizedPoint(event);
    if (!pointerMoveTimer) {
      pointerMoveTimer = setTimeout(() => {
        pointerMoveTimer = null;
        if (pointerDown && pendingPointerMove) {
          sendControl("pointer-move", pendingPointerMove);
          pendingPointerMove = null;
        }
      }, 33);
    }
  });
  screen.addEventListener("pointerup", (event) => {
    if (pointerMoveTimer) {
      clearTimeout(pointerMoveTimer);
      pointerMoveTimer = null;
    }
    if (pendingPointerMove) {
      sendControl("pointer-move", pendingPointerMove);
      pendingPointerMove = null;
    }
    pointerDown = false;
    sendControl("pointer-up", normalizedPoint(event));
  });
  screen.addEventListener("pointercancel", (event) => {
    if (!pointerDown) return;
    if (pointerMoveTimer) clearTimeout(pointerMoveTimer);
    pointerMoveTimer = null;
    pendingPointerMove = null;
    pointerDown = false;
    sendControl("pointer-up", normalizedPoint(event));
  });
  screen.addEventListener("wheel", (event) => {
    event.preventDefault();
    sendControl("scroll", { x: Math.sign(event.deltaX), y: Math.sign(event.deltaY) });
  }, { passive: false });
  screen.addEventListener("keydown", (event) => {
    if (["F5", "F11"].includes(event.key)) return;
    event.preventDefault();
    sendControl("key", { key: event.key });
  });
}

function setupButtons() {
  document.querySelectorAll("[data-control]").forEach((button) => {
    button.addEventListener("click", () => {
      sendControl("system", { key: button.dataset.control });
      showToast(`${button.title || button.querySelector("small")?.textContent || "操作"}已发送`);
    });
  });
  $("#fullscreen-button").addEventListener("click", async () => {
    try {
      if (!document.fullscreenElement) {
        await document.documentElement.requestFullscreen();
      } else {
        await document.exitFullscreen();
      }
    } catch {
      showToast("当前浏览器不允许进入全屏", true);
    }
  });
}

function profileLabel(profile, custom = null) {
  if (profile === "custom" && custom) return `${custom.maxSize}p / ${custom.maxFps}fps`;
  return ({ smooth: "360p / 10fps", low: "480p / 15fps", standard: "540p / 18fps", hd: "720p / 24fps", quality: "960p / 24fps", ultra: "1080p / 30fps", auto: "自动（720p 基线）" })[profile] || "视频设置";
}

function setStageMessage(title, detail, switching = false) {
  $("#stage-message-title").textContent = title;
  $("#stage-message-detail").textContent = detail;
  $("#stage-message").classList.toggle("profile-switching", switching);
  $("#stage-message").classList.remove("hidden");
}

function beginProfileSwitch(profile, automatic, custom) {
  profileSwitching = true;
  clearTimeout(profileSwitchTimer);
  const label = profileLabel(profile, custom);
  setStageMessage(automatic ? `正在自动调整至 ${label}` : `正在切换至 ${label}`, automatic ? "网络状态变化，正在平滑调整，约需 2–5 秒" : "正在重启手机视频编码器，控制连接会保持", true);
  $("#stream-profile").disabled = true;
  $("#apply-custom-profile").disabled = true;
  profileSwitchTimer = setTimeout(() => finishProfileSwitch(false), 8000);
}

function finishProfileSwitch(videoReady = true) {
  if (!profileSwitching) return;
  profileSwitching = false;
  clearTimeout(profileSwitchTimer);
  profileSwitchTimer = null;
  $("#stream-profile").disabled = false;
  $("#apply-custom-profile").disabled = false;
  $("#stage-message").classList.add("hidden");
  $("#stage-message").classList.remove("profile-switching");
  if (!videoReady) showToast("画质切换仍在恢复中，可稍候或重新选择档位", true);
}

async function applyStreamProfile(profile, automatic = true, custom = null) {
  if (!joined || session.isDemo) return;
  const selector = $("#stream-profile");
  beginProfileSwitch(profile, false, custom);
  try {
    await api(`/api/public/sessions/${encodeURIComponent(token)}/stream-profile`, {
      method: "POST",
      body: JSON.stringify({ profile, ...(custom || {}) }),
    });
    if (!automatic) showToast(profile === "smooth" ? "已切换至流畅优先" : profile === "quality" ? "已切换至画质优先" : "已启用自动调节");
    selectedProfile = profile;
    showToast(`已切换至 ${profileLabel(profile, custom)}`);
    tuneReceiverBuffer(profile);
  } catch (error) {
    finishProfileSwitch(false);
    if (!automatic) showToast(error.message, true);
    selector.value = selectedProfile;
  }
}

function updateTransportIndicator(reports, pair, rtt) {
  if (!pair) return;
  const local = pair.localCandidateId ? reports.get(pair.localCandidateId) : null;
  const remote = pair.remoteCandidateId ? reports.get(pair.remoteCandidateId) : null;
  const usesTURN = local?.candidateType === "relay" || remote?.candidateType === "relay";
  let path = "WebRTC 直连";
  if (usesTURN) path = "TURN 中继";
  else if (transportHint === "frp-udp") path = "FRP UDP";
  else if (local?.candidateType === "host" && remote?.candidateType === "host") path = "局域网直连";
  const delay = rtt > 0 ? ` · ${Math.round(rtt * 1000)}ms` : "";
  $("#latency-chip").textContent = path + delay;
  // The browser sees the server candidate as "remote". A server srflx
  // candidate is its real NAT mapping; the FRP UDP advertisement is a host
  // candidate whose public port is rewritten before the answer is sent.
  const diagnosticPath = usesTURN ? "TURN 中继" : remote?.candidateType === "srflx" ? "公网 UDP 直连" : transportHint === "frp-udp" ? "88FRP UDP 中继" : local?.candidateType === "host" && remote?.candidateType === "host" ? "局域网直连" : "WebRTC UDP";
  $("#latency-chip").textContent = diagnosticPath + delay;
  $("#diagnostic-path").textContent = diagnosticPath;
}

function renderTransferSpeed(reports) {
  let inbound = null;
  reports.forEach((report) => {
    if (report.type === "inbound-rtp" && report.kind === "video") inbound = report;
  });
  if (!inbound || inbound.bytesReceived == null) return;
  const now = performance.now();
  const bytes = Number(inbound.bytesReceived);
  let bitsPerSecond = 0;
  if (transportLastBytes != null && transportLastAt != null && now > transportLastAt) {
    bitsPerSecond = Math.max(0, (bytes - transportLastBytes) * 8000 / (now - transportLastAt));
  }
  transportLastBytes = bytes;
  transportLastAt = now;
  const label = bitsPerSecond >= 1_000_000
    ? `${(bitsPerSecond / 1_000_000).toFixed(1)} Mbps`
    : `${Math.round(bitsPerSecond / 1000)} Kbps`;
  $("#mobile-transport").textContent = `传输 ${label}`;
  const chip = $("#latency-chip");
  chip.dataset.speed = label;
  if (!chip.textContent.includes(label)) chip.textContent = `${chip.textContent.replace(/ · [\d.]+ (?:M|K)bps$/, "")} · ${label}`;

  const packets = Number(inbound.packetsReceived || 0);
  const lost = Math.max(0, Number(inbound.packetsLost || 0));
  const loss = packets + lost > 0 ? lost * 100 / (packets + lost) : 0;
  const jitter = Number(inbound.jitter || 0) * 1000;
  const decoded = Number(inbound.framesDecoded || 0);
  let fps = 0;
  if (diagnosticLast && now > diagnosticLast.at) fps = Math.max(0, (decoded - diagnosticLast.decoded) * 1000 / (now - diagnosticLast.at));
  diagnosticLast = { at: now, decoded };
  const rtt = Number(reports.__phoneBridgeRTT || 0) * 1000;
  $("#diagnostic-rtt").textContent = rtt > 0 ? `${Math.round(rtt)}ms` : "--";
  $("#diagnostic-quality").textContent = `${Math.round(jitter)}ms / ${loss.toFixed(1)}%`;
  $("#diagnostic-video").textContent = `${label} / ${fps.toFixed(0)}fps`;
  const warning = loss > 3 || rtt > 260 || jitter > 45 || (decoded > 5 && fps > 0 && fps < 8);
  $("#network-summary").textContent = warning ? "网络波动" : "传输正常";
  $("#network-advice").textContent = warning
    ? "检测到弱网。画质不会自动变化；可点击“流畅”切到 360p / 10fps，或在顶部手动选择档位。"
    : "当前为实时测量结果；显示“公网 UDP 直连”才表示浏览器未经过 88FRP 视频中继。";
}

function startTransportMonitor() {
  clearInterval(transportMonitorTimer);
  transportLastBytes = null;
  transportLastAt = null;
  transportMonitorTimer = setInterval(async () => {
    if (!joined || !peerConnection) return;
    const reports = await peerConnection.getStats().catch(() => null);
    if (!reports) return;
    let pair = null;
    reports.forEach((report) => {
      if (report.type === "candidate-pair" && report.state === "succeeded" && (report.nominated || !pair)) pair = report;
    });
    reports.__phoneBridgeRTT = pair?.currentRoundTripTime || 0;
    updateTransportIndicator(reports, pair, reports.__phoneBridgeRTT);
    renderTransferSpeed(reports);
  }, 1000);
}

function stopTransportMonitor() {
  clearInterval(transportMonitorTimer);
  transportMonitorTimer = null;
  transportLastBytes = null;
  transportLastAt = null;
  diagnosticLast = null;
  $("#mobile-transport").textContent = "传输 --";
}

function scheduleWebRTCRecovery() {
  if (!joined || session?.isDemo || reconnectTimer) return;
  if (reconnectAttempts >= 3) {
    $("#stage-message-title").textContent = "视频通道暂不可用";
    $("#stage-message-detail").textContent = "已保留兼容预览；请稍后点击刷新或重新打开分享链接。";
    return;
  }
  reconnectAttempts += 1;
  const delay = Math.min(8000, 700 * (2 ** Math.min(reconnectAttempts - 1, 3)));
  $("#stage-message-detail").textContent = `正在重新协商视频通道（第 ${reconnectAttempts} 次，约 ${Math.ceil(delay / 1000)} 秒）`;
  reconnectTimer = setTimeout(() => {
    reconnectTimer = null;
    if (joined) void startWebRTC(true);
  }, delay);
}

function tuneReceiverBuffer(profile) {
  if (!peerConnection) return;
  const targetMilliseconds = profile === "smooth" ? 100 : profile === "low" ? 85 : profile === "standard" ? 70 : profile === "hd" ? 60 : profile === "quality" ? 50 : 45;
  peerConnection.getReceivers().forEach((receiver) => {
    try {
      if ("jitterBufferTarget" in receiver) {
        receiver.jitterBufferTarget = targetMilliseconds;
      } else if ("playoutDelayHint" in receiver) {
        receiver.playoutDelayHint = targetMilliseconds / 1000;
      }
    } catch {
      // Browsers clamp unsupported targets; WebRTC's default remains safe.
    }
  });
}

function animateDemoControl(type, detail) {
  if (type === "pointer-up") {
    const hint = $("#demo-hint");
    hint.textContent = `触控已接收 · ${Math.round(detail.x * 100)}%, ${Math.round(detail.y * 100)}%`;
    hint.classList.add("pulse");
    setTimeout(() => hint.classList.remove("pulse"), 350);
  }
  if (type === "system") {
    $("#demo-hint").textContent = `系统操作：${detail.key}`;
  }
  if (type === "key") {
    $("#demo-hint").textContent = `键盘输入：${detail.key}`;
  }
}

function updateCountdown() {
  const element = $("#controller-countdown");
  if (!session?.expiresAt) {
    element.textContent = "不限时";
    return;
  }
  const remaining = Math.max(0, new Date(session.expiresAt).getTime() - Date.now());
  if (remaining <= 0) {
    endSession("分享时间已结束");
    return;
  }
  const seconds = Math.floor(remaining / 1000);
  const hours = Math.floor(seconds / 3600);
  const minutes = Math.floor((seconds % 3600) / 60);
  const rest = seconds % 60;
  element.textContent = hours > 0
    ? `${String(hours).padStart(2, "0")}:${String(minutes).padStart(2, "0")}:${String(rest).padStart(2, "0")}`
    : `${String(minutes).padStart(2, "0")}:${String(rest).padStart(2, "0")}`;
}

function showReconnect() {
  profileSwitching = false;
  clearTimeout(profileSwitchTimer);
  $("#stream-profile").disabled = false;
  $("#apply-custom-profile").disabled = false;
  $("#stage-message").classList.remove("profile-switching");
  $("#stage-message-title").textContent = "正在恢复连接";
  $("#stage-message-detail").textContent = "请保持页面打开";
  $("#stage-message").classList.remove("hidden");
  $("#connection-chip").innerHTML = "<i class=\"warning\"></i>正在重连";
}

function hideReconnect() {
  if (profileSwitching) return;
  $("#stage-message").classList.add("hidden");
  if (session?.isDemo) $("#connection-chip").innerHTML = "<i></i>演示连接";
}

function startFallbackFrames() {
  if (fallbackRunning) return;
  fallbackRunning = true;
  const image = $("#fallback-frame");
  const placeholder = $("#video-placeholder");
  let stopped = false;
  const schedule = (delay) => {
    if (stopped || !joined || frameTimer !== null) return;
    frameTimer = setTimeout(() => {
      frameTimer = null;
      refresh();
    }, delay);
  };
  const refresh = () => {
    if (stopped || !joined) return;
    image.src = `/api/public/sessions/${encodeURIComponent(token)}/frame?viewer=${encodeURIComponent(viewerToken)}&r=${Date.now()}`;
  };
  image.onload = () => {
    placeholder.classList.add("hidden");
    if (!$("#device-video").classList.contains("visible")) {
      $("#connection-chip").innerHTML = "<i></i>兼容实时画面";
      $("#latency-chip").textContent = "ADB 低带宽预览";
    }
    schedule(120);
  };
  image.onerror = () => {
    placeholder.classList.remove("hidden");
    placeholder.querySelector("strong").textContent = "正在重试获取手机画面";
    schedule(800);
  };
  image.dataset.fallbackActive = "true";
  image._stopFallbackFrames = () => {
    stopped = true;
  };
  refresh();
}

function stopFallbackFrames() {
  const image = $("#fallback-frame");
  if (typeof image._stopFallbackFrames === "function") image._stopFallbackFrames();
  delete image._stopFallbackFrames;
  delete image.dataset.fallbackActive;
  if (frameTimer !== null) clearTimeout(frameTimer);
  frameTimer = null;
  fallbackRunning = false;
}

function prioritizeNativeVideo() {
  if (renderMode !== "native") return;
  clearTimeout(fallbackPauseTimer);
  clearTimeout(fallbackRetryTimer);
  // Keep a first visual fallback while scrcpy starts, then stop repeated ADB
  // screenshots so they cannot contend with the low-latency encoder.
  fallbackPauseTimer = setTimeout(() => {
    if (!$("#device-video").classList.contains("visible")) stopFallbackFrames();
  }, 1200);
  // A failed native track still recovers to a usable preview instead of a
  // blank controller.
  fallbackRetryTimer = setTimeout(() => {
    if (joined && !$("#device-video").classList.contains("visible")) startFallbackFrames();
  }, 3000);
}

async function startWebRTC(recovery = false) {
  if (webRTCStarting) return;
  webRTCStarting = true;
  try {
    const config = await api(`/api/public/sessions/${encodeURIComponent(token)}/webrtc/config`);
    transportHint = config.transportHint || "direct";
    directProbe = Boolean(config.directProbe);
    const previousConnection = peerConnection;
    peerConnection = null;
    previousConnection?.close();
    const connection = new RTCPeerConnection({ iceServers: config.iceServers || [] });
    peerConnection = connection;
    connection.addTransceiver("video", { direction: "recvonly" });
    dataChannel = connection.createDataChannel("phonebridge", { ordered: true });
    dataChannel.binaryType = "arraybuffer";
    dataChannel.onopen = () => {
      if (peerConnection !== connection) return;
      reconnectAttempts = 0;
      $("#connection-chip").innerHTML = "<i></i>WebRTC 已连接";
      $("#latency-chip").textContent = transportHint === "frp-udp" ? "FRP UDP 探测中" : "WebRTC 探测中";
      prioritizeNativeVideo();
      startTransportMonitor();
    };
    dataChannel.onclose = () => {
      if (peerConnection !== connection) return;
      dataChannel = null;
      stopTransportMonitor();
      if (joined) {
        startFallbackFrames();
        showReconnect();
        scheduleWebRTCRecovery();
      }
    };
    dataChannel.onmessage = receiveWebRTCMessage;
    connection.ontrack = (event) => {
      if (peerConnection !== connection) return;
      if (event.track.kind !== "video") return;
      tuneReceiverBuffer(selectedProfile);
      const video = $("#device-video");
      video.srcObject = event.streams[0] || new MediaStream([event.track]);
      // Receiving a track is not the same as decoding a frame. Keep the
      // screenshot fallback visible until the native video element has data.
      const showDecodedVideo = () => showNativeVideo(video);
      video.onloadeddata = showDecodedVideo;
      video.onplaying = showDecodedVideo;
      video.play().then(showDecodedVideo).catch(() => {});
      $("#connection-chip").innerHTML = "<i></i>低延迟视频";
      $("#latency-chip").textContent = "scrcpy H.264 / WebRTC";
    };
    connection.onconnectionstatechange = () => {
      if (peerConnection !== connection) return;
      if (connection.connectionState === "failed" || connection.connectionState === "disconnected") {
        showReconnect();
        startFallbackFrames();
        scheduleWebRTCRecovery();
      }
    };

    const offer = await connection.createOffer();
    await connection.setLocalDescription(offer);
    await waitForIceGathering(connection, directProbe ? 5000 : 10000);
    const answer = await api(`/api/public/sessions/${encodeURIComponent(token)}/webrtc/offer`, {
      method: "POST",
      body: JSON.stringify(connection.localDescription),
    });
    if (peerConnection === connection) await connection.setRemoteDescription(answer);
  } catch (error) {
    if (error.status === 409) {
      endSession("此连接已被新的设备接管");
      return;
    }
    // Fallback frames keep the controller usable when ICE/TURN is unavailable.
    startFallbackFrames();
    if (recovery || joined) scheduleWebRTCRecovery();
  } finally {
    webRTCStarting = false;
  }
}

function waitForIceGathering(connection, timeoutMilliseconds = 1800) {
  if (connection.iceGatheringState === "complete") return Promise.resolve();
  return new Promise((resolve) => {
    const timeout = setTimeout(done, timeoutMilliseconds);
    function done() {
      clearTimeout(timeout);
      connection.removeEventListener("icegatheringstatechange", onChange);
      resolve();
    }
    function onChange() {
      if (connection.iceGatheringState === "complete") done();
    }
    connection.addEventListener("icegatheringstatechange", onChange);
  });
}

function receiveWebRTCMessage(event) {
  if (typeof event.data === "string") {
    try {
      const header = JSON.parse(event.data);
      if (header.kind === "frame" && Number.isInteger(header.id) && Number.isInteger(header.chunks)) {
        webRTCFrame = { id: header.id, chunks: header.chunks, mime: header.mime || "image/png", parts: new Array(header.chunks), received: 0 };
		$("#phone-screen").dataset.webrtcFrame = `receiving:${header.id}`;
		stopFallbackFrames();
      }
    } catch {
      // Ignore unknown peer messages.
    }
    return;
  }
  if (event.data && typeof event.data.arrayBuffer === "function") {
    event.data.arrayBuffer().then(receiveWebRTCFrameChunk).catch(() => {});
    return;
  }
  receiveWebRTCFrameChunk(event.data);
}

function receiveWebRTCFrameChunk(data) {
  if (!webRTCFrame || !data || typeof data.byteLength !== "number" || data.byteLength < 8) return;
  const view = new DataView(data);
  const id = view.getUint32(0);
  const index = view.getUint16(4);
  const total = view.getUint16(6);
  if (id !== webRTCFrame.id || total !== webRTCFrame.chunks || index >= total || webRTCFrame.parts[index]) return;
  webRTCFrame.parts[index] = data.slice(8);
  webRTCFrame.received += 1;
  if (webRTCFrame.received !== webRTCFrame.chunks) return;
  const completedFrameID = webRTCFrame.id;
  const nextURL = URL.createObjectURL(new Blob(webRTCFrame.parts, { type: webRTCFrame.mime }));
  const image = $("#fallback-frame");
  image.onload = () => {
    if (currentFrameURL) URL.revokeObjectURL(currentFrameURL);
    currentFrameURL = nextURL;
    $("#video-placeholder").classList.add("hidden");
    stopFallbackFrames();
	finishProfileSwitch(true);
	$("#phone-screen").dataset.webrtcFrame = `shown:${completedFrameID}`;
	try {
	  dataChannel?.send(JSON.stringify({ kind: "frame-ack", id: completedFrameID }));
	} catch {
	  startFallbackFrames();
	}
  };
	image.onerror = () => {
	  URL.revokeObjectURL(nextURL);
	  startFallbackFrames();
	};
  image.src = nextURL;
  webRTCFrame = null;
}

function endSession(message) {
  clearInterval(countdownTimer);
  clearInterval(pollTimer);
  clearTimeout(fallbackPauseTimer);
  clearTimeout(fallbackRetryTimer);
  clearTimeout(reconnectTimer);
  reconnectTimer = null;
  stopTransportMonitor();
  stopFallbackFrames();
  peerConnection?.close();
  peerConnection = null;
  dataChannel = null;
  if (currentFrameURL) URL.revokeObjectURL(currentFrameURL);
  currentFrameURL = null;
  joined = false;
  $("#controller-shell").classList.add("hidden");
  $("#join-page").classList.remove("hidden");
  $("#join-form").classList.add("hidden");
  $("#join-loading").classList.remove("hidden");
  $("#join-loading").innerHTML = `<strong>${message}</strong><small>你可以关闭这个页面。</small>`;
}

function showJoinError(message, terminal = false) {
  $("#join-loading").classList.remove("hidden");
  $("#join-loading").innerHTML = `<strong>${message}</strong>${terminal ? "<small>请向分享者获取新的链接。</small>" : ""}`;
}

let toastTimer;
function showToast(message, isError = false) {
  const toast = $("#toast");
  toast.textContent = message;
  toast.classList.toggle("error", isError);
  toast.classList.add("show");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => toast.classList.remove("show"), 2500);
}

document.addEventListener("DOMContentLoaded", () => {
  $("#join-form").addEventListener("submit", joinSession);
  $("#join-submit").addEventListener("click", (event) => {
    if (event.defaultPrevented) return;
    event.preventDefault();
    joinSession({
      preventDefault() {},
      submitter: event.currentTarget,
    });
  });
  $("#access-code").addEventListener("input", (event) => {
    const digits = event.target.value.replace(/\D/g, "").slice(0, 6);
    event.target.value = digits.length > 3 ? `${digits.slice(0, 3)} ${digits.slice(3)}` : digits;
  });
  $("#stream-profile").addEventListener("change", (event) => {
    if (event.target.value === "custom") {
      $("#custom-profile").classList.remove("hidden");
      return;
    }
    $("#custom-profile").classList.add("hidden");
    applyStreamProfile(event.target.value);
  });
  $("#apply-custom-profile").addEventListener("click", () => {
    const maxSize = Number($("#custom-size").value);
    const maxFps = Number($("#custom-fps").value);
    applyStreamProfile("custom", true, { maxSize, maxFps });
    $("#custom-profile").classList.add("hidden");
  });
  $("#quick-smooth").addEventListener("click", () => {
    $("#stream-profile").value = "smooth";
    $("#custom-profile").classList.add("hidden");
    applyStreamProfile("smooth", false);
  });
  $("#render-mode-toggle").addEventListener("click", () => {
    if (renderMode === "compatibility") {
      tryNativeVideo();
    } else {
      useCompatibilityPreview();
    }
  });
  setupPhoneInput();
  setupButtons();
  loadSession();
});
