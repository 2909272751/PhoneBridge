const $ = (selector) => document.querySelector(selector);
const token = window.location.pathname.split("/").filter(Boolean).pop();
let session = null;
let joined = false;
let countdownTimer = null;
let pollTimer = null;
let pointerDown = false;
let frameTimer = null;
let peerConnection = null;
let dataChannel = null;
let webRTCFrame = null;
let currentFrameURL = null;
let fallbackPauseTimer = null;
let fallbackRetryTimer = null;
let autoProfileTimer = null;
let autoAppliedProfile = "auto";
let autoGoodSamples = 0;
let autoLastInbound = null;

async function api(url, options = {}) {
  const response = await fetch(url, {
    headers: { "Content-Type": "application/json", ...(options.headers || {}) },
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
  $("#video-placeholder").classList.toggle("hidden", session.isDemo);
  $("#control-status").textContent = session.mode === "control"
    ? "当前由你控制 · 电脑桌面始终不可见"
    : "当前为仅查看模式 · 控制操作已禁用";
  if (session.mode !== "control") {
    document.body.classList.add("view-only");
  }
  updateCountdown();
  countdownTimer = setInterval(updateCountdown, 1000);
  pollTimer = setInterval(pollSession, 5000);
  if (!session.isDemo) {
    startFallbackFrames();
    void startWebRTC();
  }
  $("#phone-screen").focus();
}

async function pollSession() {
  try {
    const current = await api(`/api/public/sessions/${encodeURIComponent(token)}`);
    session = { ...session, ...current };
    hideReconnect();
  } catch (error) {
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
    if (error.status === 410) {
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
    screen.setPointerCapture(event.pointerId);
    sendControl("pointer-down", normalizedPoint(event));
  });
  screen.addEventListener("pointermove", (event) => {
    if (pointerDown) sendControl("pointer-move", normalizedPoint(event));
  });
  screen.addEventListener("pointerup", (event) => {
    pointerDown = false;
    sendControl("pointer-up", normalizedPoint(event));
  });
  screen.addEventListener("pointercancel", () => { pointerDown = false; });
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

async function applyStreamProfile(profile, automatic = false) {
  if (!joined || session.isDemo) return;
  const selector = $("#stream-profile");
  try {
    await api(`/api/public/sessions/${encodeURIComponent(token)}/stream-profile`, {
      method: "POST",
      body: JSON.stringify({ profile }),
    });
    if (!automatic) showToast(profile === "smooth" ? "已切换至流畅优先" : profile === "quality" ? "已切换至画质优先" : "已启用自动调节");
    autoAppliedProfile = profile;
  } catch (error) {
    if (!automatic) showToast(error.message, true);
    selector.value = "auto";
  }
}

function startAutoProfile() {
  clearInterval(autoProfileTimer);
  autoProfileTimer = setInterval(async () => {
    if (!joined || $("#stream-profile").value !== "auto" || !peerConnection) return;
    const reports = await peerConnection.getStats().catch(() => null);
    if (!reports) return;
    let rtt = 0;
    let inbound = null;
    reports.forEach((report) => {
      if (report.type === "candidate-pair" && (report.nominated || report.state === "succeeded") && report.currentRoundTripTime != null) rtt = Math.max(rtt, report.currentRoundTripTime);
      if (report.type === "inbound-rtp" && report.kind === "video") inbound = report;
    });
    if (!inbound) return;
    let lossRate = 0;
    if (autoLastInbound) {
      const received = Math.max(0, inbound.packetsReceived - autoLastInbound.received);
      const lost = Math.max(0, inbound.packetsLost - autoLastInbound.lost);
      lossRate = lost / Math.max(1, received + lost);
    }
    autoLastInbound = { received: inbound.packetsReceived || 0, lost: inbound.packetsLost || 0 };
    let target = autoAppliedProfile;
    if (rtt > 0.25 || lossRate > 0.02) {
      autoGoodSamples = 0;
      target = "smooth";
    } else if (rtt > 0 && rtt < 0.12 && lossRate < 0.003) {
      autoGoodSamples += 1;
      if (autoGoodSamples >= 3) target = "quality";
    } else {
      autoGoodSamples = 0;
      target = "auto";
    }
    if (target !== autoAppliedProfile) await applyStreamProfile(target, true);
  }, 3000);
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
  $("#stage-message").classList.remove("hidden");
  $("#connection-chip").innerHTML = "<i class=\"warning\"></i>正在重连";
}

function hideReconnect() {
  $("#stage-message").classList.add("hidden");
  if (session?.isDemo) $("#connection-chip").innerHTML = "<i></i>演示连接";
}

function startFallbackFrames() {
  if (frameTimer) return;
  const image = $("#fallback-frame");
  const placeholder = $("#video-placeholder");
  const refresh = () => {
    if (!joined) return;
    image.src = `/api/public/sessions/${encodeURIComponent(token)}/frame?r=${Date.now()}`;
  };
  image.onload = () => {
    placeholder.classList.add("hidden");
    if (!$("#device-video").classList.contains("visible")) {
      $("#connection-chip").innerHTML = "<i></i>兼容实时画面";
      $("#latency-chip").textContent = "ADB 低带宽预览";
    }
  };
  image.onerror = () => {
    placeholder.classList.remove("hidden");
    placeholder.querySelector("strong").textContent = "正在重试获取手机画面";
  };
  refresh();
  frameTimer = setInterval(refresh, 550);
}

function stopFallbackFrames() {
  clearInterval(frameTimer);
  frameTimer = null;
}

function prioritizeNativeVideo() {
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

async function startWebRTC() {
  try {
    const config = await api(`/api/public/sessions/${encodeURIComponent(token)}/webrtc/config`);
    peerConnection?.close();
    peerConnection = new RTCPeerConnection({ iceServers: config.iceServers || [] });
    peerConnection.addTransceiver("video", { direction: "recvonly" });
    dataChannel = peerConnection.createDataChannel("phonebridge", { ordered: true });
    dataChannel.binaryType = "arraybuffer";
    dataChannel.onopen = () => {
      $("#connection-chip").innerHTML = "<i></i>WebRTC 已连接";
      $("#latency-chip").textContent = config.turnConfigured ? "WebRTC / TURN" : "FRP UDP 通道";
      prioritizeNativeVideo();
      startAutoProfile();
    };
    dataChannel.onclose = () => {
      dataChannel = null;
      if (joined) startFallbackFrames();
    };
    dataChannel.onmessage = receiveWebRTCMessage;
    peerConnection.ontrack = (event) => {
      if (event.track.kind !== "video") return;
      const video = $("#device-video");
      video.srcObject = event.streams[0] || new MediaStream([event.track]);
      // Receiving a track is not the same as decoding a frame. Keep the
      // screenshot fallback visible until the native video element has data.
      const showDecodedVideo = () => {
        if (video.readyState < HTMLMediaElement.HAVE_CURRENT_DATA) return;
        clearTimeout(fallbackPauseTimer);
        clearTimeout(fallbackRetryTimer);
        video.classList.add("visible");
        $("#fallback-frame").classList.remove("visible");
        $("#video-placeholder").classList.add("hidden");
        stopFallbackFrames();
      };
      video.onloadeddata = showDecodedVideo;
      video.onplaying = showDecodedVideo;
      video.play().then(showDecodedVideo).catch(() => {});
      $("#connection-chip").innerHTML = "<i></i>低延迟视频";
      $("#latency-chip").textContent = "scrcpy H.264 / WebRTC";
    };
    peerConnection.onconnectionstatechange = () => {
      if (!peerConnection) return;
      if (peerConnection.connectionState === "failed" || peerConnection.connectionState === "disconnected") {
        showReconnect();
        startFallbackFrames();
      }
    };

    const offer = await peerConnection.createOffer();
    await peerConnection.setLocalDescription(offer);
    await waitForIceGathering(peerConnection);
    const answer = await api(`/api/public/sessions/${encodeURIComponent(token)}/webrtc/offer`, {
      method: "POST",
      body: JSON.stringify(peerConnection.localDescription),
    });
    await peerConnection.setRemoteDescription(answer);
  } catch {
    // Fallback frames keep the controller usable when ICE/TURN is unavailable.
    startFallbackFrames();
  }
}

function waitForIceGathering(connection) {
  if (connection.iceGatheringState === "complete") return Promise.resolve();
  return new Promise((resolve) => {
    const timeout = setTimeout(done, 10000);
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
  clearInterval(autoProfileTimer);
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
    autoGoodSamples = 0;
    autoLastInbound = null;
    applyStreamProfile(event.target.value);
  });
  setupPhoneInput();
  setupButtons();
  loadSession();
});
