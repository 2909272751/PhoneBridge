const state = {
  status: null,
  selectedDevice: null,
  timer: null,
};

// isLocalPage is true only when this page is served from the machine running
// PhoneBridge (loopback host). The public-tunnel page is never local, so it
// can never save credentials and only derives proofs in the browser. It is
// guarded so the Node test harness (which has no window) can require app.js.
const isLocalPage = typeof window !== "undefined" && ["localhost", "127.0.0.1", "::1"].includes(window.location.hostname);

const $ = (selector) => document.querySelector(selector);
const $$ = (selector) => Array.from(document.querySelectorAll(selector));

async function api(url, options = {}) {
  const headers = { "Content-Type": "application/json", ...(options.headers || {}) };
  const response = await fetch(url, {
    headers,
    ...options,
  });
  const payload = await response.json().catch(() => ({}));
  if (!response.ok) {
    const error = new Error(payload.error || payload.message || `请求失败（${response.status}）`);
    error.payload = payload;
    throw error;
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

async function repairADB() {
  const button = $("#repair-adb");
  const refreshButton = $("#refresh-inline");
  const hasConnectedDevice = state.status?.adb?.available && (state.status.adb.devices || []).length > 0;
  if (hasConnectedDevice && !window.confirm("修复 ADB 会短暂中断当前手机画面并重新连接，是否继续？")) {
    return;
  }
  button.disabled = true;
  refreshButton.disabled = true;
  button.textContent = "正在接管 ADB…";
  $("#adb-message").textContent = "正在停止冲突服务并启动 PhoneBridge 内置 ADB…";
  try {
    const result = await api("/api/devices/repair-adb", { method: "POST", body: "{}" });
    showToast(result.message);
  } catch (error) {
    showToast(error.message, true);
  } finally {
    button.disabled = false;
    refreshButton.disabled = false;
    button.textContent = "修复 ADB";
    await loadStatus(true);
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
  const publicState = status.publicAccess?.state;
  const publicReady = ["manual-ready", "frp-ready", "frp-stale-ready"].includes(publicState);
  const publicDotClass = publicState === "frp-stale-ready" || publicState === "frp-unreachable" || !publicReady ? "warning" : "";
  $("#cloud-pill").innerHTML = `<span class="status-dot ${publicDotClass}"></span><span>${publicReady ? "公网地址已就绪" : "仅本机访问"}</span>`;
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
  renderLocalAdmin(status.localAdmin);
  renderWebRTCTransport(status.webrtcTransport);
}

function renderWebRTCTransport(transport) {
  const element = $("#diag-webrtc");
  const detail = $("#diag-webrtc-detail");
  if (!element) return;
  if (!transport) {
    element.textContent = "不可用";
    element.className = "danger";
    detail.textContent = "服务尚未报告 WebRTC 传输状态。";
    return;
  }
  if (transport.udpForwardActive && transport.udpForwardFresh) {
    element.textContent = "实时链路";
    element.className = "success";
    detail.textContent = "88FRP 公网 UDP 映射已激活，浏览器可建立低延迟视频。";
  } else if (transport.udpForwardActive) {
    element.textContent = "已恢复映射";
    element.className = "warning";
    detail.textContent = "使用上次成功保存的 88FRP UDP 映射；88FRP 重新登录后会自动更新。";
  } else if (transport.turnConfigured) {
    element.textContent = "TURN 中继";
    element.className = "warning";
    detail.textContent = "当前无公网 UDP 映射，将依赖 TURN 中继建立实时视频。";
  } else {
    element.textContent = "不可用";
    element.className = "danger";
    detail.textContent = transport.reason || "当前没有公网 UDP 映射且未配置 TURN，将使用兼容画面。";
  }
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
  const stateInfo = {
    "manual-ready": { text: "已就绪", cls: "ready" },
    "frp-ready": { text: "已就绪", cls: "ready" },
    "frp-stale-ready": { text: "公网可用 · 待重登", cls: "warning" },
    "frp-unreachable": { text: "公网不可用", cls: "danger" },
    "frp-error": { text: "同步错误", cls: "danger" },
    "frp-pending": { text: "同步中", cls: "warning" },
    "frp-needs-instance": { text: "待选实例", cls: "warning" },
    "frp-needs-tunnel": { text: "待选隧道", cls: "warning" },
    "local": { text: "待配置", cls: "warning" },
  };
  const info = stateInfo[access.state] || { text: "待配置", cls: "warning" };
  stateElement.textContent = info.text;
  stateElement.className = `access-state ${info.cls}`;
  const suffix = access.effectiveUrl ? ` 当前地址：${access.effectiveUrl}` : "";
  $("#public-access-message").textContent = `${access.message || "尚未设置公网地址。"}${suffix}`;
  $("#public-access-message").classList.toggle("error", access.state === "frp-error" || access.state === "frp-unreachable");
}

// base64urlFromArray encodes bytes as RFC 4648 base64url without padding.
function base64urlFromArray(bytes) {
  let binary = "";
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

// base64SaltBytes decodes an 88FRP v3 challenge salt that the server sends as
// base64 or base64url (with or without padding) into the raw bytes PBKDF2
// consumes. The real 88FRP console-login.js calls bytes(challenge.salt), i.e.
// it base64/base64url-decodes the salt before deriving the key. It never
// falls back to UTF-8: an undecodable salt throws so a protocol mismatch is
// surfaced instead of silently computing a wrong proof (SPEC point 4).
function base64SaltBytes(salt) {
  const table = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
  const lookup = new Int8Array(128).fill(-1);
  for (let i = 0; i < table.length; i++) lookup[table.charCodeAt(i)] = i;
  let cleaned = String(salt).replace(/\s+/g, "").replace(/-/g, "+").replace(/_/g, "/");
  cleaned += "=".repeat((4 - (cleaned.length % 4)) % 4);
  const out = [];
  let buffer = 0;
  let bits = 0;
  for (let i = 0; i < cleaned.length; i++) {
    const ch = cleaned[i];
    if (ch === "=") break;
    const value = lookup[ch.charCodeAt(0)];
    if (value < 0) throw new Error("88FRP salt 不是有效的 base64/base64url");
    buffer = (buffer << 6) | value;
    bits += 6;
    if (bits >= 8) {
      bits -= 8;
      out.push((buffer >> bits) & 0xff);
    }
  }
  return new Uint8Array(out);
}

// ---------------------------------------------------------------------------
// Pure-JavaScript cryptographic primitives (PBKDF2-HMAC-SHA256 fallback)
//
// 88FRP public pages served over plain HTTP are not a secure context, so most
// mobile browsers disable WebCrypto (crypto.subtle is undefined or throws). To
// keep those pages able to log in, these primitives implement UTF-8, SHA-256,
// HMAC-SHA256 and PBKDF2-HMAC-SHA256 (dkLen=32) entirely in JS with no network
// dependencies. The output matches Go's crypto/pbkdf2 and standard WebCrypto
// exactly (SPEC point 2). The password only ever exists as a local variable
// inside a call and is cleared by the caller right after.

// webCryptoAvailable reports whether the native WebCrypto API is reachable.
function webCryptoAvailable() {
  return typeof crypto !== "undefined" && crypto && typeof crypto.subtle !== "undefined";
}

// isWebCryptoUnavailableError reports whether an error came from WebCrypto
// being unavailable (e.g. not a secure context) so we can fall back.
function isWebCryptoUnavailableError(error) {
  const message = String((error && error.message) || "");
  return /insecure|secure context|not available|is undefined|not supported|not an object/i.test(message);
}

// utf8Encode encodes a JS string as UTF-8 bytes without relying on TextEncoder,
// so the fallback works identically in any environment.
function utf8Encode(str) {
  const bytes = [];
  for (let i = 0; i < str.length; i++) {
    let code = str.charCodeAt(i);
    if (code >= 0xd800 && code <= 0xdbff && i + 1 < str.length) {
      const low = str.charCodeAt(i + 1);
      if (low >= 0xdc00 && low <= 0xdfff) {
        code = 0x10000 + ((code - 0xd800) << 10) + (low - 0xdc00);
        i++;
      }
    }
    if (code <= 0x7f) {
      bytes.push(code);
    } else if (code <= 0x7ff) {
      bytes.push(0xc0 | (code >> 6), 0x80 | (code & 0x3f));
    } else if (code <= 0xffff) {
      bytes.push(0xe0 | (code >> 12), 0x80 | ((code >> 6) & 0x3f), 0x80 | (code & 0x3f));
    } else {
      bytes.push(0xf0 | (code >> 18), 0x80 | ((code >> 12) & 0x3f), 0x80 | ((code >> 6) & 0x3f), 0x80 | (code & 0x3f));
    }
  }
  return new Uint8Array(bytes);
}

function rotr32(value, count) {
  return (value >>> count) | (value << (32 - count));
}

const SHA256_K = [
  0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
  0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
  0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
  0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
  0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
  0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
  0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
  0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
];

// sha256 hashes a Uint8Array and returns a 32-byte Uint8Array (RFC 6234).
function sha256(input) {
  const length = input.length;
  const remainder = (length + 1) % 64;
  const zeroLen = remainder <= 56 ? 56 - remainder : 120 - remainder;
  const total = length + 1 + zeroLen + 8;
  const message = new Uint8Array(total);
  message.set(input);
  message[length] = 0x80;
  const bitLength = length * 8;
  const lengthView = new DataView(message.buffer);
  lengthView.setUint32(total - 8, Math.floor(bitLength / 0x100000000), false);
  lengthView.setUint32(total - 4, bitLength >>> 0, false);

  let h0 = 0x6a09e667, h1 = 0xbb67ae85, h2 = 0x3c6ef372, h3 = 0xa54ff53a;
  let h4 = 0x510e527f, h5 = 0x9b05688c, h6 = 0x1f83d9ab, h7 = 0x5be0cd19;

  for (let offset = 0; offset < total; offset += 64) {
    const blockView = new DataView(message.buffer, offset, 64);
    const w = new Uint32Array(64);
    for (let i = 0; i < 16; i++) w[i] = blockView.getUint32(i * 4, false);
    for (let i = 16; i < 64; i++) {
      const s0 = rotr32(w[i - 15], 7) ^ rotr32(w[i - 15], 18) ^ (w[i - 15] >>> 3);
      const s1 = rotr32(w[i - 2], 17) ^ rotr32(w[i - 2], 19) ^ (w[i - 2] >>> 10);
      w[i] = (w[i - 16] + s0 + w[i - 7] + s1) >>> 0;
    }
    let a = h0, b = h1, c = h2, d = h3, e = h4, f = h5, g = h6, hh = h7;
    for (let i = 0; i < 64; i++) {
      const sum1 = rotr32(e, 6) ^ rotr32(e, 11) ^ rotr32(e, 25);
      const ch = (e & f) ^ (~e & g);
      const temp1 = (hh + sum1 + ch + SHA256_K[i] + w[i]) >>> 0;
      const sum0 = rotr32(a, 2) ^ rotr32(a, 13) ^ rotr32(a, 22);
      const maj = (a & b) ^ (a & c) ^ (b & c);
      const temp2 = (sum0 + maj) >>> 0;
      hh = g; g = f; f = e; e = (d + temp1) >>> 0;
      d = c; c = b; b = a; a = (temp1 + temp2) >>> 0;
    }
    h0 = (h0 + a) >>> 0; h1 = (h1 + b) >>> 0; h2 = (h2 + c) >>> 0; h3 = (h3 + d) >>> 0;
    h4 = (h4 + e) >>> 0; h5 = (h5 + f) >>> 0; h6 = (h6 + g) >>> 0; h7 = (h7 + hh) >>> 0;
  }

  const out = new Uint8Array(32);
  const outView = new DataView(out.buffer);
  outView.setUint32(0, h0, false); outView.setUint32(4, h1, false);
  outView.setUint32(8, h2, false); outView.setUint32(12, h3, false);
  outView.setUint32(16, h4, false); outView.setUint32(20, h5, false);
  outView.setUint32(24, h6, false); outView.setUint32(28, h7, false);
  return out;
}

// hmacSHA256 computes HMAC-SHA256 (RFC 2104) of message with key. Both inputs
// are Uint8Array; returns a 32-byte Uint8Array.
function hmacSHA256(key, message) {
  const blockSize = 64;
  let k = new Uint8Array(blockSize);
  if (key.length > blockSize) key = sha256(key);
  k.set(key);
  const ipad = new Uint8Array(blockSize);
  const opad = new Uint8Array(blockSize);
  for (let i = 0; i < blockSize; i++) {
    ipad[i] = k[i] ^ 0x36;
    opad[i] = k[i] ^ 0x5c;
  }
  const inner = new Uint8Array(blockSize + message.length);
  inner.set(ipad);
  inner.set(message, blockSize);
  const innerHash = sha256(inner);
  const outer = new Uint8Array(blockSize + innerHash.length);
  outer.set(opad);
  outer.set(innerHash, blockSize);
  return sha256(outer);
}

function yieldToEventLoop() {
  return new Promise((resolve) => setTimeout(resolve, 0));
}

// pbkdf2SHA256 derives dkLen bytes via PBKDF2-HMAC-SHA256 (RFC 2898 / RFC 8018).
// Iterations are processed in batches of `batchSize`, yielding to the event
// loop between batches so a mobile page is never frozen for the whole loop
// (SPEC point 5). Returns a Uint8Array.
async function pbkdf2SHA256(password, salt, iterations, dkLen, batchSize = 1000) {
  const hLen = 32;
  const blockCount = Math.ceil(dkLen / hLen);
  const out = new Uint8Array(dkLen);
  const saltBlock = new Uint8Array(salt.length + 4);
  saltBlock.set(salt);
  for (let block = 1; block <= blockCount; block++) {
    const blockView = new DataView(saltBlock.buffer);
    blockView.setUint32(salt.length, block, false);
    let u = hmacSHA256(password, saltBlock);
    let t = new Uint8Array(u);
    for (let i = 1; i < iterations; i++) {
      u = hmacSHA256(password, u);
      for (let j = 0; j < hLen; j++) t[j] ^= u[j];
      if (batchSize > 0 && i % batchSize === 0) await yieldToEventLoop();
    }
    const targetStart = (block - 1) * hLen;
    const chunkLen = Math.min(hLen, dkLen - targetStart);
    out.set(t.subarray(0, chunkLen), targetStart);
  }
  return out;
}

// deriveFRPProofWebCrypto reproduces the 88FRP v3 challenge with native
// WebCrypto: PBKDF2-SHA256 derives a key from the password and the challenge
// salt, then HMAC-SHA256 signs `${nonce}\n${username.trim()}` and the result is
// base64url-encoded (SPEC point 3).
async function deriveFRPProofWebCrypto(password, username, challenge, iterations, nonce) {
  const encoder = new TextEncoder();
  const baseKey = await crypto.subtle.importKey("raw", encoder.encode(password), "PBKDF2", false, ["deriveKey"]);
  // length:256 pins the derived HMAC-SHA256 key to 32 bytes (the SHA-256 output
  // size). Without it WebCrypto defaults to the hash block size (64 bytes),
  // which would diverge from the Go/standard pbkdf2 32-byte key and break the
  // proof the backend verifies.
  const key = await crypto.subtle.deriveKey(
    { name: "PBKDF2", salt: base64SaltBytes(challenge), iterations, hash: "SHA-256" },
    baseKey,
    { name: "HMAC", hash: "SHA-256", length: 256 },
    false,
    ["sign"],
  );
  const signature = await crypto.subtle.sign("HMAC", key, encoder.encode(`${nonce}\n${username.trim()}`));
  return base64urlFromArray(new Uint8Array(signature));
}

// deriveFRPProofFallback reproduces the same proof purely in JS. `batchSize`
// controls how many PBKDF2 iterations run before yielding to the event loop;
// it is only a scheduling hint and does not change the output.
async function deriveFRPProofFallback(password, username, challenge, iterations, nonce, batchSize = 1000) {
  const passwordBytes = utf8Encode(password);
  const saltBytes = base64SaltBytes(challenge);
  const key = await pbkdf2SHA256(passwordBytes, saltBytes, iterations, 32, batchSize);
  const message = utf8Encode(`${nonce}\n${username.trim()}`);
  return base64urlFromArray(hmacSHA256(key, message));
}

// deriveFRPProof prefers native WebCrypto and transparently falls back to the
// pure-JS implementation when WebCrypto is missing or blocked (SPEC point 1).
// The password exists only as a local variable inside this call and is cleared
// by the caller right after.
async function deriveFRPProof(password, username, challenge, iterations, nonce, batchSize = 1000) {
  if (!webCryptoAvailable()) {
    return deriveFRPProofFallback(password, username, challenge, iterations, nonce, batchSize);
  }
  try {
    return await deriveFRPProofWebCrypto(password, username, challenge, iterations, nonce);
  } catch (error) {
    if (isWebCryptoUnavailableError(error)) {
      return deriveFRPProofFallback(password, username, challenge, iterations, nonce, batchSize);
    }
    throw error;
  }
}

function hideLoginSections() {
  ["#frp-login-form", "#frp-session-state"].forEach((selector) => {
    $(selector)?.classList.add("hidden");
  });
}

function renderLocalAdmin(localAdmin) {
  const stateElement = $("#frp-login-state");
  const message = $("#frp-login-message");
  if (!stateElement) return;
  message.classList.remove("error");
  hideLoginSections();
  stateElement.className = "frp-login-state";
  const remember = $("#frp-remember");
  const rememberHint = $("#frp-remember-hint");
  if (remember) remember.disabled = !isLocalPage;
  if (rememberHint) {
    rememberHint.textContent = isLocalPage ? "" : "公网页面无法保存凭据；请在连接电脑的本机页面登录一次。";
    rememberHint.classList.toggle("hidden", isLocalPage);
  }
  if (localAdmin?.authenticated) {
    stateElement.textContent = localAdmin.saved ? "已登录（凭据已保存）" : "已登录（本次运行）";
    stateElement.classList.add("ready");
    $("#frp-session-state").classList.remove("hidden");
    message.textContent = localAdmin.saved
      ? "88FRP 会话已恢复；本机退出登录会同时清除保存的凭据。"
      : "88FRP 会话保留在本次运行进程内；勾选“记住登录”后重启可自动恢复。";
    return;
  }
  stateElement.textContent = "未登录";
  $("#frp-login-form").classList.remove("hidden");
  if (localAdmin?.saved && localAdmin.autoLoginState === "failed") {
    stateElement.classList.add("danger");
    stateElement.textContent = "自动登录失败";
    message.textContent = "已保存的凭据自动登录失败，请重新登录。";
  } else if (localAdmin?.saved && localAdmin.autoLoginState === "running") {
    stateElement.classList.add("warning");
    stateElement.textContent = "自动登录中";
    message.textContent = "正在使用已保存的凭据恢复 88FRP 登录……";
  } else {
    message.textContent = isLocalPage
      ? "输入 88FRP 用户名和密码登录并同步；本机可勾选“记住登录”，凭据经加密保存，重启后自动恢复。"
      : "这是公网页面：密码只在当前浏览器用 WebCrypto 生成一次性证明，不会发送或保存。如需记住登录，请在连接电脑的本机页面设置一次。";
  }
}

async function login88FRP() {
  const button = $("#frp-login-button");
  const username = $("#frp-username").value.trim();
  const password = $("#frp-password").value;
  const message = $("#frp-login-message");
  if (!username || !password) {
    showToast("请输入 88FRP 用户名和密码", true);
    return;
  }
  button.disabled = true;
  button.textContent = "正在安全计算登录证明…";
  message.textContent = "";
  message.classList.remove("error");
  try {
    // 1. Ask the backend to proxy the 88FRP v3 challenge.
    const challenge = await api("/api/remote-admin/challenge", {
      method: "POST",
      body: JSON.stringify({ username, serviceUrl: $("#frp-service-url").value.trim() }),
    });
    // 2. Derive the one-time proof locally with WebCrypto. Before dropping
    //    the password, persist it on the local machine when "remember login"
    //    is checked; an unchecked box clears any previously saved credentials
    //    (SPEC point 3). The public page never saves.
    const proof = await deriveFRPProof(password, username, challenge.challenge, challenge.iterations, challenge.nonce);
    if (isLocalPage) {
      try {
        await api("/api/remote-admin/credentials", {
          method: "POST",
          body: JSON.stringify(
            rememberLogin()
              ? { serviceUrl: $("#frp-service-url").value.trim(), username, password, remember: true }
              : { remember: false },
          ),
        });
      } catch (saveError) {
        if (rememberLogin()) showToast("登录成功，但保存凭据失败：" + saveError.message, true);
      }
    }
    button.textContent = "正在登录并同步…";
    $("#frp-password").value = "";
    // 3. Submit username/proof/challengeId only; there is no password field.
    const result = await api("/api/remote-admin/login", {
      method: "POST",
      body: JSON.stringify({ username, proof, challengeId: challenge.challengeId }),
    });
    if (result.publicAccess) renderPublicAccess(result.publicAccess);
    await loadStatus();
    message.textContent = result.message || "已登录 88FRP 并同步。";
    showToast("88FRP 登录成功");
  } catch (error) {
    $("#frp-password").value = "";
    if (error.payload?.publicAccess) renderPublicAccess(error.payload.publicAccess);
    message.textContent = error.message;
    message.classList.add("error");
    showToast(error.message, true);
  } finally {
    button.disabled = false;
    button.textContent = "登录并同步";
  }
}

// rememberLogin reports whether the user asked to persist the login on the
// local machine. The checkbox is disabled on the public page.
function rememberLogin() {
  const checkbox = $("#frp-remember");
  return isLocalPage && checkbox?.checked === true;
}

async function logout88FRP() {
  const button = $("#frp-logout-button");
  button.disabled = true;
  try {
    // Same-origin main-service call (8787); no separate 8788 listener and no
    // bearer are involved anymore.
    await api("/api/remote-admin/logout", { method: "POST", body: "{}" });
    await loadStatus();
    showToast("已退出 88FRP 登录");
  } catch (error) {
    showToast(error.message, true);
  } finally {
    button.disabled = false;
  }
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
    if (error.payload?.publicAccess) renderPublicAccess(error.payload.publicAccess);
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
    if (error.payload?.publicAccess) renderPublicAccess(error.payload.publicAccess);
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
  $("#frp-login-button").addEventListener("click", login88FRP);
  $("#frp-logout-button").addEventListener("click", logout88FRP);
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

if (typeof document !== "undefined" && typeof document.addEventListener === "function") {
  document.addEventListener("DOMContentLoaded", () => {
    setupNavigation();
    setupCopyButtons();
    setupPublicAccess();
    $("#refresh-button").addEventListener("click", refreshDevices);
    $("#refresh-inline").addEventListener("click", refreshDevices);
    $("#repair-adb").addEventListener("click", repairADB);
    $("#check-update").addEventListener("click", checkForUpdate);
    $("#share-form").addEventListener("submit", createShare);
    loadStatus();
  });
}

// Export the pure functions for the Node deterministic-vector test. In a
// browser this block is skipped, so the UI stays untouched.
if (typeof module !== "undefined" && module.exports) {
  module.exports = {
    deriveFRPProof,
    deriveFRPProofWebCrypto,
    deriveFRPProofFallback,
    pbkdf2SHA256,
    hmacSHA256,
    sha256,
    utf8Encode,
    base64urlFromArray,
    base64SaltBytes,
    webCryptoAvailable,
    isWebCryptoUnavailableError,
  };
}
