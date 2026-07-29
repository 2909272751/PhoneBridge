const state = {
  status: null,
  selectedDevice: null,
  timer: null,
};

const $ = (selector) => document.querySelector(selector);
const $$ = (selector) => Array.from(document.querySelectorAll(selector));

async function api(url, options = {}) {
  const response = await fetch(url, {
    headers: { "Content-Type": "application/json", ...(options.headers || {}) },
    ...options,
  });
  const payload = await response.json().catch(() => ({}));
  if (!response.ok) {
    throw new Error(payload.error || `请求失败（${response.status}）`);
  }
  return payload;
}

async function loadStatus(showBusy = false) {
  const refreshButtons = [$("#refresh-button"), $("#refresh-inline")];
  if (showBusy) refreshButtons.forEach((button) => button?.classList.add("spinning"));
  try {
    const status = await api("/api/status");
    state.status = status;
    renderStatus(status);
  } catch (error) {
    showToast(error.message, true);
    $("#agent-label").textContent = "服务连接失败";
    $("#agent-dot").classList.add("danger");
  } finally {
    refreshButtons.forEach((button) => button?.classList.remove("spinning"));
  }
}

async function refreshDevices() {
  $("#adb-message").textContent = "正在重新检测ADB设备……";
  try {
    await api("/api/devices/refresh", { method: "POST", body: "{}" });
    await loadStatus(true);
  } catch (error) {
    showToast(error.message, true);
  }
}

async function checkForUpdate() {
  const button = $("#check-update");
  const message = $("#update-message");
  if (button.dataset.releaseUrl) {
    window.open(button.dataset.releaseUrl, "_blank", "noopener");
    return;
  }
  button.disabled = true;
  message.textContent = "正在安全检查 GitHub Release…";
  try {
    const update = await api("/api/update");
    if (update.updateAvailable) {
      message.textContent = `发现 v${update.latestVersion}，点击可打开下载页。`;
      button.textContent = "打开下载页";
      button.dataset.releaseUrl = update.releaseURL;
      showToast(`发现新版本 v${update.latestVersion}`);
    } else {
      message.textContent = `当前已是最新版 v${update.currentVersion}。`;
      button.textContent = "已是最新版";
    }
  } catch (error) {
    message.textContent = error.message;
    showToast(error.message, true);
  } finally {
    button.disabled = false;
  }
}

function renderStatus(status) {
  $("#version-label").textContent = `版本 ${status.version}`;
  $("#top-version").textContent = `v${status.version}`;
  $("#agent-label").textContent = "后台服务运行中";
  $("#agent-dot").classList.remove("danger");
  const publicReady = status.publicAccess?.state === "manual-ready" || status.publicAccess?.state === "frp-ready";
  $("#cloud-pill").innerHTML = `<span class="status-dot ${publicReady ? "" : "warning"}"></span><span>${publicReady ? "公网地址已就绪" : "仅本机访问"}</span>`;
  $("#demo-banner").classList.toggle("hidden", !status.demoMode);
  $("#adb-message").textContent = status.adb.message;
  $("#diag-agent").textContent = status.agentState === "running" ? "运行正常" : "异常";
  $("#diag-adb").textContent = status.adb.available ? "已就绪" : "不可用";
  $("#diag-adb").className = status.adb.available ? "success" : "danger";
  $("#diag-adb-detail").textContent = status.adb.message;
  $("#diag-scrcpy").textContent = status.scrcpy.available ? status.scrcpy.version : "不可用";
  $("#diag-scrcpy").className = status.scrcpy.available ? "success" : "danger";
  $("#diag-scrcpy-detail").textContent = status.scrcpy.message;
  renderDevices(status.adb.devices || []);
  renderActiveSession(status.activeSession);
  renderPublicAccess(status.publicAccess);
}

function renderPublicAccess(access) {
  if (!access) return;
  state.publicAccess = access;
  const settings = access.settings || {};
  const source = settings.source === "88frp" ? "88frp" : "manual";
  const sourceInput = document.querySelector(`input[name="public-source"][value="${source}"]`);
  if (sourceInput) sourceInput.checked = true;
  $("#manual-public-url").value = settings.manualUrl || "";
  $("#frp-service-url").value = settings.frpServiceUrl || "http://127.0.0.1:8801";
  $("#frp-scheme").value = settings.frpScheme === "https" ? "https" : "http";
  $("#frp-auto-sync").checked = Boolean(settings.autoSync);
  $("#manual-public-fields").classList.toggle("hidden", source !== "manual");
  $("#frp-public-fields").classList.toggle("hidden", source !== "88frp");
  renderFRPOptions("#frp-instance", access.instances || [], settings.frpInstanceId, "先点“同步地址”读取实例", (item) => item.name || item.id);
  const eligibleTunnels = (access.tunnels || []).filter((item) => String(item.localPort) === "8787" && item.type === "tcp");
  renderFRPOptions("#frp-tunnel", eligibleTunnels, settings.frpTunnelName, "自动筛选本地端口 8787", (item) => `${item.displayName || item.name} · 公网端口 ${item.remotePort}${item.enabled ? "" : "（未启用）"}`);
  const stateElement = $("#public-access-state");
  const ready = access.state === "manual-ready" || access.state === "frp-ready";
  stateElement.textContent = ready ? "已就绪" : "待配置";
  stateElement.className = `access-state ${ready ? "ready" : "warning"}`;
  const suffix = access.effectiveUrl ? ` 当前地址：${access.effectiveUrl}` : "";
  $("#public-access-message").textContent = `${access.message || "尚未设置公网地址。"}${suffix}`;
  $("#public-access-message").classList.toggle("error", access.state === "frp-error");
}

function renderFRPOptions(selector, items, selected, placeholder, label) {
  const element = $(selector);
  if (!element) return;
  element.innerHTML = "";
  const empty = document.createElement("option");
  empty.value = "";
  empty.textContent = placeholder;
  element.appendChild(empty);
  items.forEach((item) => {
    const option = document.createElement("option");
    option.value = item.id || item.name;
    option.textContent = label(item);
    option.selected = option.value === selected;
    element.appendChild(option);
  });
}

function collectPublicAccessSettings() {
  const source = document.querySelector('input[name="public-source"]:checked')?.value || "manual";
  return {
    source,
    manualUrl: $("#manual-public-url").value.trim(),
    frpServiceUrl: $("#frp-service-url").value.trim(),
    frpInstanceId: $("#frp-instance").value,
    frpTunnelName: $("#frp-tunnel").value,
    frpScheme: $("#frp-scheme").value,
    autoSync: $("#frp-auto-sync").checked,
  };
}

async function savePublicAccess() {
  const button = $("#save-public-access");
  button.disabled = true;
  try {
    const result = await api("/api/public-access", { method: "PUT", body: JSON.stringify(collectPublicAccessSettings()) });
    renderPublicAccess(result);
    await loadStatus();
    showToast("公网分享设置已保存");
  } catch (error) {
    $("#public-access-message").textContent = error.message;
    $("#public-access-message").classList.add("error");
    showToast(error.message, true);
  } finally {
    button.disabled = false;
  }
}

async function syncPublicAccess() {
  const button = $("#sync-public-access");
  button.disabled = true;
  try {
    const result = await api("/api/public-access", { method: "PUT", body: JSON.stringify(collectPublicAccessSettings()) });
    renderPublicAccess(result);
    await loadStatus();
    showToast(result.effectiveUrl ? "已同步最新地址" : "已读取 88FRP 配置");
  } catch (error) {
    $("#public-access-message").textContent = error.message;
    $("#public-access-message").classList.add("error");
    showToast(error.message, true);
  } finally {
    button.disabled = false;
  }
}

function setupPublicAccess() {
  $$('input[name="public-source"]').forEach((input) => input.addEventListener("change", () => {
    const frp = input.value === "88frp" && input.checked;
    $("#manual-public-fields").classList.toggle("hidden", frp);
    $("#frp-public-fields").classList.toggle("hidden", !frp);
  }));
  $("#save-public-access").addEventListener("click", savePublicAccess);
  $("#sync-public-access").addEventListener("click", syncPublicAccess);
}

function renderDevices(devices) {
  const grid = $("#device-grid");
  grid.innerHTML = "";
  if (!devices.length) {
    grid.innerHTML = `
      <div class="device-empty">
        <div class="empty-phone">＋</div>
        <h3>尚未发现已授权的手机</h3>
        <p>使用USB连接Android手机，开启USB调试，并在手机上允许这台电脑进行调试。</p>
        <button class="button primary" id="empty-refresh">重新检测设备</button>
        <ol>
          <li>打开手机“开发者选项”</li>
          <li>开启“USB调试”</li>
          <li>连接数据线并确认授权弹窗</li>
        </ol>
      </div>`;
    $("#empty-refresh").addEventListener("click", refreshDevices);
    return;
  }

  devices.forEach((device) => {
    const available = device.state === "device";
    const card = document.createElement("article");
    card.className = `device-card${available ? "" : " unavailable"}`;
    const badge = device.isDemo ? "演示设备" : device.connection === "usb" ? "USB连接" : "网络设备";
    const stateLabel = available ? "设备可用" : device.state === "unauthorized" ? "等待手机授权" : `设备${device.state}`;
    card.innerHTML = `
      <div class="device-visual">
        <div class="device-notch"></div>
        <div class="device-glow"></div>
        <span>${device.isDemo ? "DEMO" : "ADB"}</span>
      </div>
      <div class="device-info">
        <div class="device-title-row"><div><span class="device-badge">${badge}</span><h3>${escapeHTML(device.model)}</h3></div><span class="availability ${available ? "" : "warning"}"><i></i>${stateLabel}</span></div>
        <p>${escapeHTML(device.androidLabel)} · 序列号 ${escapeHTML(maskSerial(device.id))}</p>
        <div class="device-card-footer">
          <span>${device.isDemo ? "用于验证分享与控制界面" : "只会分享这台手机"}</span>
          <button class="button primary share-device" ${available ? "" : "disabled"}>分享这台设备</button>
        </div>
      </div>`;
    card.querySelector(".share-device").addEventListener("click", () => openShareDialog(device));
    grid.appendChild(card);
  });
}

function renderActiveSession(session) {
  const panel = $("#active-session");
  if (!session || session.state === "stopped" || session.state === "expired") {
    panel.classList.add("hidden");
    if (state.timer) clearInterval(state.timer);
    return;
  }
  panel.classList.remove("hidden");
  $("#active-device-name").textContent = session.deviceName;
  $("#share-url").value = session.shareUrl;
  $("#share-code").textContent = session.requireCode ? formatCode(session.accessCode) : "无需访问码";
  $("#viewer-state").textContent = session.viewerState === "connected" ? "已连接" : "等待加入";
  $("#connection-mode").textContent = connectionLabel(session.connectionMode);
  $("#open-controller").onclick = () => window.open(session.shareUrl, "_blank", "noopener");
  $("#stop-session").onclick = () => stopSession(session.id);
  updateCountdown(session.expiresAt, $("#session-countdown"));
  if (state.timer) clearInterval(state.timer);
  state.timer = setInterval(() => {
    updateCountdown(session.expiresAt, $("#session-countdown"));
    loadStatus(false);
  }, 5000);
}

function openShareDialog(device) {
  state.selectedDevice = device;
  $("#dialog-device-name").textContent = `分享 ${device.model}`;
  $("#share-error").classList.add("hidden");
  $("#share-dialog").showModal();
}

async function createShare(event) {
  event.preventDefault();
  if (!state.selectedDevice) return;
  const button = $("#create-share");
  button.disabled = true;
  button.textContent = "正在创建安全会话……";
  $("#share-error").classList.add("hidden");
  try {
    const payload = {
      deviceId: state.selectedDevice.id,
      durationMinutes: Number(document.querySelector('input[name="duration"]:checked').value),
      mode: document.querySelector('input[name="mode"]:checked').value,
      streamProfile: "hd",
      requireCode: $("#require-code").checked,
      allowClipboard: $("#allow-clipboard").checked,
      allowAudio: $("#allow-audio").checked,
    };
    await api("/api/sessions", { method: "POST", body: JSON.stringify(payload) });
    $("#share-dialog").close();
    await loadStatus();
    showToast("分享已创建，可以复制链接发送给访问者");
  } catch (error) {
    $("#share-error").textContent = error.message;
    $("#share-error").classList.remove("hidden");
  } finally {
    button.disabled = false;
    button.textContent = "开始分享";
  }
}

async function stopSession(id) {
  if (!confirm("确定停止当前分享吗？链接将立即失效，访问者会被断开。")) return;
  try {
    await api(`/api/sessions/${encodeURIComponent(id)}/stop`, { method: "POST", body: "{}" });
    await loadStatus();
    showToast("分享已安全停止");
  } catch (error) {
    showToast(error.message, true);
  }
}

function setupNavigation() {
  $$(".nav-item").forEach((button) => {
    button.addEventListener("click", () => {
      $$(".nav-item").forEach((item) => item.classList.remove("active"));
      $$(".view").forEach((view) => view.classList.remove("active"));
      button.classList.add("active");
      $(`#view-${button.dataset.view}`).classList.add("active");
      const titles = { devices: "已连接设备", sessions: "分享记录", diagnostics: "连接诊断", settings: "设置" };
      $("#page-title").textContent = titles[button.dataset.view];
      $("#refresh-button").classList.toggle("hidden", button.dataset.view !== "devices");
    });
  });
}

function setupCopyButtons() {
  document.addEventListener("click", async (event) => {
    const id = event.target.dataset.copy;
    if (!id) return;
    const value = document.getElementById(id)?.value;
    if (!value) return;
    try {
      await navigator.clipboard.writeText(value);
      showToast("链接已复制");
    } catch {
      document.getElementById(id).select();
      document.execCommand("copy");
      showToast("链接已复制");
    }
  });
}

function updateCountdown(expiresAt, element) {
  if (!expiresAt) {
    element.textContent = "不限时";
    return;
  }
  const remaining = Math.max(0, new Date(expiresAt).getTime() - Date.now());
  const totalSeconds = Math.floor(remaining / 1000);
  const hours = Math.floor(totalSeconds / 3600);
  const minutes = Math.floor((totalSeconds % 3600) / 60);
  const seconds = totalSeconds % 60;
  element.textContent = hours > 0
    ? `${String(hours).padStart(2, "0")}:${String(minutes).padStart(2, "0")}:${String(seconds).padStart(2, "0")}`
    : `${String(minutes).padStart(2, "0")}:${String(seconds).padStart(2, "0")}`;
}

function connectionLabel(value) {
  return {
    "not-connected": "尚未连接",
    "demo": "演示通道",
    "awaiting-webrtc": "等待WebRTC",
    "negotiating-webrtc": "正在协商 WebRTC",
    "webrtc": "WebRTC 已连接",
    "reconnecting": "正在重连 WebRTC",
    "fallback-screen": "兼容画面通道",
    "p2p": "点对点连接",
    "turn": "安全中继",
  }[value] || value;
}

function maskSerial(value) {
  if (value.length <= 8) return value;
  return `${value.slice(0, 4)}••••${value.slice(-2)}`;
}

function formatCode(value) {
  return value ? `${value.slice(0, 3)} ${value.slice(3)}` : "";
}

function escapeHTML(value = "") {
  return value.replace(/[&<>"']/g, (character) => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
  })[character]);
}

let toastTimer;
function showToast(message, isError = false) {
  const toast = $("#toast");
  toast.textContent = message;
  toast.classList.toggle("error", isError);
  toast.classList.add("show");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => toast.classList.remove("show"), 3000);
}

document.addEventListener("DOMContentLoaded", () => {
  setupNavigation();
  setupCopyButtons();
  setupPublicAccess();
  $("#refresh-button").addEventListener("click", refreshDevices);
  $("#refresh-inline").addEventListener("click", refreshDevices);
  $("#check-update").addEventListener("click", checkForUpdate);
  $("#share-form").addEventListener("submit", createShare);
  loadStatus();
});
