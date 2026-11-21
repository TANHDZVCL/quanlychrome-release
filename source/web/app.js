"use strict";

const TOKEN = location.hash.slice(1);
const $ = (s) => document.querySelector(s);

let state = { profiles: [], groups: [], settings: {}, chromePath: "", dataDir: "", sync: { locks: {}, cloud: {} } };
const DEFAULT_SERVER = "https://49-236-211-75.sslip.io";
const selected = new Set();
const checks = new Map(); // id -> {wait} | ProxyCheck result
const busy = new Set();   // ids with an open/close in flight
let editingId = null;

// ---------- API ----------
async function api(method, path, body) {
  const res = await fetch(path, {
    method,
    headers: { "X-Token": TOKEN, "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  let data = null;
  try { data = await res.json(); } catch { /* empty */ }
  if (!res.ok) throw new Error((data && data.error) || `Lỗi ${res.status}`);
  return data;
}

let vpnLoadedAt = 0;
async function refresh() {
  try {
    state = await api("GET", "/api/state");
    if (state.sync && state.sync.enabled && Date.now() - vpnLoadedAt > 60000) {
      vpnLoadedAt = Date.now();
      api("GET", "/api/vpn/status").then((v) => { vpn = v; render(); }).catch(() => {});
    }
    render();
  } catch (e) {
    console.error(e);
  }
}

// ---------- helpers ----------
function esc(s) {
  return String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}
function toast(msg, kind = "") {
  const t = document.createElement("div");
  t.className = "toast " + kind;
  t.textContent = msg;
  // Open <dialog>s sit in the top layer: put the toast inside the dialog so it is visible.
  const dlgs = document.querySelectorAll("dialog[open]");
  let box = $("#toasts");
  if (dlgs.length) {
    const d = dlgs[dlgs.length - 1];
    box = d.querySelector(".toasts") || d.appendChild(Object.assign(document.createElement("div"), { className: "toasts" }));
  }
  box.appendChild(t);
  setTimeout(() => t.remove(), kind === "err" ? 7000 : 3500);
}
function timeAgo(iso) {
  if (!iso) return "—";
  const d = new Date(iso);
  const s = (Date.now() - d.getTime()) / 1000;
  if (s < 60) return "vừa xong";
  if (s < 3600) return Math.floor(s / 60) + " phút trước";
  if (s < 86400) return Math.floor(s / 3600) + " giờ trước";
  if (s < 86400 * 7) return Math.floor(s / 86400) + " ngày trước";
  return d.toLocaleDateString("vi-VN");
}
function fmtSize(n) {
  if (!n) return "—";
  if (n < 1 << 20) return Math.max(1, Math.round(n / 1024)) + " KB";
  if (n < 1 << 30) return Math.round(n / (1 << 20)) + " MB";
  return (n / (1 << 30)).toFixed(1) + " GB";
}
function proxyText(p) {
  if (!p || !p.type || !p.host) return "";
  return `${p.host}:${p.port}`;
}
function byId(id) { return state.profiles.find((p) => p.id === id); }

const ICON = {
  play: '<svg viewBox="0 0 24 24"><path d="M7 4v16l13-8z"/></svg>',
  stop: '<svg viewBox="0 0 24 24"><rect x="6" y="6" width="12" height="12" rx="1"/></svg>',
  edit: '<svg viewBox="0 0 24 24"><path d="M12 20h9"/><path d="M16.5 3.5a2.1 2.1 0 0 1 3 3L7 19l-4 1 1-4Z"/></svg>',
  key: '<svg viewBox="0 0 24 24"><circle cx="8" cy="15" r="4"/><path d="m10.8 12.2 8.7-8.7M17 6l2 2M15 8l2 2"/></svg>',
  copy: '<svg viewBox="0 0 24 24"><rect x="9" y="9" width="12" height="12" rx="2"/><path d="M5 15H4a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1h10a1 1 0 0 1 1 1v1"/></svg>',
  folder: '<svg viewBox="0 0 24 24"><path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2Z"/></svg>',
  globe: '<svg viewBox="0 0 24 24"><circle cx="12" cy="12" r="9"/><path d="M3 12h18M12 3a14 14 0 0 1 0 18M12 3a14 14 0 0 0 0 18"/></svg>',
  trash: '<svg viewBox="0 0 24 24"><path d="M4 7h16M10 11v6M14 11v6M6 7l1 13h10l1-13M9 7V4h6v3"/></svg>',
  cloud: '<svg viewBox="0 0 24 24"><path d="M17.5 19a4.5 4.5 0 1 0-1.3-8.8A6 6 0 0 0 4.6 12.5 3.5 3.5 0 0 0 6.5 19Z"/></svg>',
  lock: '<svg viewBox="0 0 24 24" style="width:12px;height:12px;vertical-align:-1px;margin-left:4px"><rect x="5" y="11" width="14" height="9" rx="2"/><path d="M8 11V8a4 4 0 0 1 8 0v3"/></svg>',
};

// ---------- render ----------
function filtered() {
  const q = $("#q").value.trim().toLowerCase();
  const g = $("#fGroup").value;
  const st = $("#fStatus").value;
  const h = state.health || {};
  const list = state.profiles.filter((p) => {
    if (p.trashedAt) return false;
    if (g === "__none" ? p.group : g && p.group !== g) return false;
    if (st === "bad") return h[p.id] && !h[p.id].ok;
    if (st === "on" && !p.running) return false;
    if (st === "off" && p.running) return false;
    if (!q) return true;
    return [p.name, p.group, p.notes, proxyText(p.proxy), p.startUrl, p.account?.uid, p.account?.email]
      .join(" ").toLowerCase().includes(q);
  });
  const sort = $("#fSort").value;
  const sizes = state.sizes || {};
  const key = {
    name: (a, b) => a.name.localeCompare(b.name, "vi", { numeric: true }),
    recent: (a, b) => (b.lastOpened || "").localeCompare(a.lastOpened || ""),
    group: (a, b) => (a.group || "~").localeCompare(b.group || "~", "vi") || a.name.localeCompare(b.name, "vi", { numeric: true }),
    size: (a, b) => (sizes[b.id] || 0) - (sizes[a.id] || 0),
  }[sort];
  return list.map((p, i) => [p, i]).sort(([a, ia], [b, ib]) =>
    (b.pinned === true) - (a.pinned === true) || (key ? key(a, b) : 0) || ia - ib).map(([p]) => p);
}

function render() {
  // drop selections of deleted profiles
  for (const id of [...selected]) if (!byId(id)) selected.delete(id);

  const active = state.profiles.filter((p) => !p.trashedAt);
  $("#statTotal").textContent = active.length;
  $("#trashCount").textContent = state.profiles.length - active.length;
  $("#btnTile").hidden = !active.some((p) => p.running);
  renderVpsBanner();
  if ($("#dlgHub").open) renderHubStatus();
  $("#statRunning").textContent = state.profiles.filter((p) => p.running).length;
  $("#chromeWarn").hidden = !!state.chromePath;
  renderSyncBtn();
  if (state.version) {
    $("#verChip").textContent = "v" + state.version;
    document.title = "Quản lý Chrome v" + state.version;
  }

  // group filter + datalist
  const fg = $("#fGroup");
  const cur = fg.value;
  fg.innerHTML = '<option value="">Tất cả nhóm</option><option value="__none">— Không nhóm —</option>' +
    state.groups.map((g) => `<option value="${esc(g)}">${esc(g)}</option>`).join("");
  fg.value = [...fg.options].some((o) => o.value === cur) ? cur : "";
  $("#groupList").innerHTML = state.groups.map((g) => `<option value="${esc(g)}">`).join("");

  const list = filtered();
  $("#rows").innerHTML = list.map(rowHTML).join("");
  const empty = list.length === 0;
  $("#empty").hidden = !empty;
  $("#emptyText").textContent = active.length ? "Không có hồ sơ khớp bộ lọc." : "Chưa có hồ sơ nào.";
  $("#btnNew2").hidden = active.length > 0;

  const vis = list.map((p) => p.id);
  const allSel = vis.length > 0 && vis.every((id) => selected.has(id));
  $("#checkAll").checked = allSel;
  $("#checkAll").indeterminate = !allSel && vis.some((id) => selected.has(id));
  $("#bulkbar").hidden = selected.size === 0;
  $("#selCount").textContent = `${selected.size} đã chọn`;
}

function rowHTML(p) {
  const sel = selected.has(p.id);
  const vp = vpnOfProfile(p);
  const ck = checks.get(p.id) || (state.health || {})[p.id];
  const iso = vp ? COUNTRY_ISO[vp.country] : (ck && ck.ok ? ck.country : "");
  const label = vp ? `<span class="pcountry">${esc(vp.name !== vp.country ? vp.name + " · " : "")}${esc(VN_COUNTRY[vp.country] || vp.country)}</span>` : "";
  const px = p.proxy && p.proxy.type && p.proxy.host
    ? `<div class="proxy">${flagHTML(iso)}<span class="ptype">${esc(p.proxy.type)}</span>${esc(proxyText(p.proxy))}${(p.proxy.user || p.proxy.pass) ? ICON.lock : ""}${label}</div>${checkHTML(p.id)}`
    : '<span class="muted">Không dùng</span>';
  const isBusy = busy.has(p.id);
  const sy = state.sync || {};
  const away = sy.locks && sy.locks[p.id];
  const cloud = sy.cloud && sy.cloud[p.id];
  let statusHTML = `<span class="status ${p.running ? "on" : "off"}"><span class="dot"></span>${p.running ? "Đang mở" : "Đã đóng"}</span>`;
  if (p.loginMode) statusHTML += `<div class="also" title="Mở không có cổng điều khiển: đăng nhập Google xong hãy đóng cửa sổ Chrome rồi mở lại bình thường">Chế độ đăng nhập Google</div>`;
  if (!p.running && away) statusHTML = `<span class="status away" title="Từ ${esc(new Date(away.since).toLocaleString("vi-VN"))}"><span class="dot"></span>Mở ở máy ${esc(away.deviceName || "khác")}</span>`;
  else if (p.running && away) statusHTML += `<div class="also">Cũng đang mở ở máy ${esc(away.deviceName || "khác")}</div>`;
  const cloudHTML = cloud ? `<span class="cloud" title="Đăng nhập web đã lưu online lúc ${esc(new Date(cloud.updatedAt).toLocaleString("vi-VN"))}">${ICON.cloud}</span>` : "";
  const toggle = p.running
    ? `<button class="btn sm" data-act="close" ${isBusy ? "disabled" : ""}>${ICON.stop}Đóng</button>`
    : `<button class="btn sm success" data-act="open" ${isBusy ? "disabled" : ""}>${ICON.play}Mở</button><button class="iconbtn gbtn" data-act="glogin" ${isBusy ? "disabled" : ""} title="Mở để đăng nhập Google (dùng khi Google báo &quot;Trình duyệt không an toàn&quot;)">G</button>`;
  return `<tr data-id="${p.id}" class="${sel ? "sel" : ""}">
    <td class="c-check"><input type="checkbox" data-act="sel" ${sel ? "checked" : ""}></td>
    <td><div class="pname"><span class="pin ${p.pinned ? "on" : ""}" data-act="pin" title="${p.pinned ? "Bỏ ghim" : "Ghim lên đầu"}">${p.pinned ? "★" : "☆"}</span>${esc(p.name)}${cloudHTML}${p.extensions && p.extensions.length ? `<span class="extcount" title="${p.extensions.length} tiện ích">🧩${p.extensions.length}</span>` : ""}</div>${p.notes ? `<div class="pnote" title="${esc(p.notes)}">${esc(p.notes)}</div>` : ""}</td>
    <td>${p.group ? `<span class="chip">${esc(p.group)}</span>` : '<span class="muted">—</span>'}</td>
    <td>${px}</td>
    <td>${statusHTML}</td>
    <td class="muted" title="${p.lastOpened ? esc(new Date(p.lastOpened).toLocaleString("vi-VN")) : ""}">${timeAgo(p.lastOpened)}</td>
    <td class="muted">${fmtSize((state.sizes || {})[p.id])}</td>
    <td><div class="rowact">${toggle}
      <button class="iconbtn" data-act="acc" title="Thông tin tài khoản & mã 2FA">${ICON.key}</button>
      <button class="iconbtn" data-act="edit" title="Sửa">${ICON.edit}</button>
      <button class="iconbtn" data-act="check" title="Kiểm tra proxy">${ICON.globe}</button>
      <button class="iconbtn" data-act="clone" title="Nhân bản (giữ tiện ích, không cookie)">${ICON.copy}</button>
      <button class="iconbtn" data-act="folder" title="Mở thư mục dữ liệu">${ICON.folder}</button>
    </div></td>
  </tr>`;
}

function checkHTML(id) {
  const c = checks.get(id) || (state.health || {})[id];
  if (!c) return "";
  if (c.wait) return '<div class="pcheck wait">Đang kiểm tra…</div>';
  if (!c.ok) return `<div class="pcheck bad" title="${esc(c.error)}">⚠ Proxy lỗi: ${esc(c.error)}${c.at ? " · " + timeAgo(c.at) : ""}</div>`;
  const loc = [c.city, c.country].filter(Boolean).join(", ");
  return `<div class="pcheck ok">✓ ${flagOf(c.country)} ${esc(c.ip)}${loc ? " · " + esc(loc) : ""} · ${c.ms} ms</div>`;
}

// ---------- actions ----------
async function openProfiles(ids, force = false, login = false, noProxy = false) {
  ids = ids.filter((id) => byId(id) && !byId(id).running);
  if (!ids.length) return;
  ids.forEach((id) => busy.add(id));
  render();
  let locked = {};
  try {
    const r = await api("POST", "/api/open", { ids, force, login, noProxy });
    if (login && !Object.keys(r.errors || {}).length && !Object.keys(r.locked || {}).length)
      toast("Đã mở ở chế độ đăng nhập Google. Đăng nhập xong hãy ĐÓNG cửa sổ Chrome, rồi mở lại bình thường để dùng.");
    reportErrors(r.errors);
    locked = r.locked || {};
  } catch (e) { toast(e.message, "err"); }
  ids.forEach((id) => busy.delete(id));
  setTimeout(refresh, 700);
  const lockedIds = Object.keys(locked);
  if (lockedIds.length) {
    const ok = await confirmBox(
      "Hồ sơ đang mở ở máy khác",
      lockedIds.map((id) => `• ${byId(id)?.name || id}: ${locked[id]}`).join("\n") +
        "\n\nMở cùng lúc ở hai máy có thể làm mất đăng nhập hoặc bị Facebook/Google bắt xác minh. Chỉ mở nếu chắc chắn máy kia đã ngừng dùng.",
      "Vẫn mở",
    );
    if (ok) openProfiles(lockedIds, true, login, noProxy);
  }
}

async function closeProfiles(ids) {
  ids = ids.filter((id) => byId(id) && byId(id).running);
  if (!ids.length) return;
  ids.forEach((id) => busy.add(id));
  render();
  try {
    const r = await api("POST", "/api/close", { ids });
    reportErrors(r.errors);
  } catch (e) { toast(e.message, "err"); }
  setTimeout(() => { ids.forEach((id) => busy.delete(id)); refresh(); }, 1500);
}

function reportErrors(errs) {
  const entries = Object.entries(errs || {});
  if (!entries.length) return;
  toast(entries.map(([id, m]) => `${byId(id)?.name || id}: ${m}`).join("\n"), "err");
}

async function checkProfiles(ids) {
  ids = ids.filter((id) => byId(id)?.proxy?.type);
  if (!ids.length) { toast("Các hồ sơ đã chọn chưa có proxy"); return; }
  ids.forEach((id) => checks.set(id, { wait: true }));
  render();
  const queue = [...ids];
  const worker = async () => {
    while (queue.length) {
      const id = queue.shift();
      try { checks.set(id, await api("POST", "/api/proxy/check", { id })); }
      catch (e) { checks.set(id, { ok: false, error: e.message }); }
      render();
    }
  };
  await Promise.all(Array.from({ length: Math.min(5, ids.length) }, worker));
}

function openGLogin(id) {
  const p = byId(id);
  if (!p) return;
  const hasProxy = !!(p.proxy && p.proxy.type && p.proxy.host);
  $("#glName").textContent = p.name;
  $("#glProxyRow").hidden = !hasProxy;
  $("#glNoProxy").checked = false;
  const d = $("#dlgGLogin");
  d.returnValue = "";
  d.onclose = () => { if (d.returnValue === "yes") openProfiles([id], false, true, hasProxy && $("#glNoProxy").checked); };
  d.showModal();
}

function confirmBox(title, text, yesLabel = "Đồng ý") {
  return new Promise((resolve) => {
    const d = $("#dlgConfirm");
    $("#cfTitle").textContent = title;
    $("#cfText").textContent = text;
    $("#cfYes").textContent = yesLabel;
    d.returnValue = "";
    d.onclose = () => resolve(d.returnValue === "yes");
    d.showModal();
  });
}

async function deleteProfiles(ids) {
  const running = ids.filter((id) => byId(id)?.running);
  if (running.length) { toast("Hãy đóng các hồ sơ đang mở trước khi xóa", "err"); return; }
  const names = ids.map((id) => byId(id)?.name).filter(Boolean);
  const ok = await confirmBox(
    `Xóa ${ids.length} hồ sơ?`,
    `${names.slice(0, 5).join(", ")}${names.length > 5 ? "…" : ""}\n\nHồ sơ được chuyển vào Thùng rác 🗑 và giữ 7 ngày (khôi phục được cả đăng nhập), sau đó tự xóa vĩnh viễn.`,
    "Chuyển vào thùng rác",
  );
  if (!ok) return;
  try {
    const r = await api("POST", "/api/delete", { ids });
    ids.forEach((id) => { selected.delete(id); checks.delete(id); });
    toast(`Đã chuyển ${r.trashed} hồ sơ vào thùng rác`, "ok");
  } catch (e) { toast(e.message, "err"); }
  refresh();
}

// ---------- profile dialog ----------
function openProfileDialog(id) {
  editingId = id || null;
  const f = $("#formProfile");
  f.reset();
  $("#pfErr").textContent = "";
  $("#pfCheckRes").textContent = "";
  $("#pfCheckRes").className = "muted";
  $("#pfPaste").value = "";
  $("#pfTitle").textContent = id ? "Sửa hồ sơ" : "Tạo hồ sơ";
  $("#countField").hidden = !!id;
  $("#pfSave").textContent = id ? "Lưu" : "Tạo";
  if (id) {
    const p = byId(id);
    f.name.value = p.name; f.group.value = p.group || ""; f.notes.value = p.notes || ""; f.startUrl.value = p.startUrl || "";
    f.ptype.value = p.proxy?.type || ""; f.phost.value = p.proxy?.host || ""; f.pport.value = p.proxy?.port || "";
    f.puser.value = p.proxy?.user || ""; f.ppass.value = p.proxy?.pass || "";
    const a = p.account || {};
    f.auid.value = a.uid || ""; f.aemail.value = a.email || ""; f.apass.value = a.password || "";
    f.a2fa.value = a.twofa || ""; f.aextra.value = a.extra || "";
    f.sites.value = p.siteDomains || "";
    f.exts.value = (p.extensions || []).map((x) => "https://chromewebstore.google.com/detail/" + x).join("\n");
  } else {
    const g = $("#fGroup").value;
    if (g && g !== "__none") f.group.value = g;
    f.exts.value = localStorage.getItem("lastExts") || "";
  }
  $("#dlgProfile").showModal();
  f.name.focus();
  fillVpnSelect();
}

function extIDs(text) {
  return [...new Set((text.toLowerCase().match(/[a-p]{32}/g) || []))];
}

async function clearCache(ids) {
  const ok = await confirmBox(
    `Dọn bộ nhớ đệm ${ids.length} hồ sơ?`,
    "Chỉ xóa ảnh, video, file tạm Chrome đã tải về để mở trang nhanh hơn.\nKHÔNG xóa đăng nhập (cookie), mật khẩu, lịch sử, tiện ích, cài đặt.\nHồ sơ đang mở sẽ được bỏ qua.",
    "Dọn ngay",
  );
  if (!ok) return;
  try {
    const r = await api("POST", "/api/clear-cache", { ids });
    toast(`Đã dọn ${r.cleared} hồ sơ, giải phóng ${fmtSize(r.freed) === "—" ? "0 MB" : fmtSize(r.freed)}` +
      (r.skipped ? `\nBỏ qua ${r.skipped} hồ sơ đang mở` : ""), "ok");
  } catch (e) { toast(e.message, "err"); }
  setTimeout(refresh, 1500);
}

// ---------- Zalo hub ----------
function renderHubStatus() {
  const h = state.hub || {};
  const el = $("#hubStatus");
  if (!h.enabled) el.innerHTML = '<span class="muted">Đang tắt trên máy này.</span>';
  else if (h.connected) el.innerHTML = `<span class="ok">● Đã kết nối máy chủ</span> · ${h.viewers ? `<b>${h.viewers}</b> nơi đang xem` : "chưa có ai đang xem"}`;
  else el.innerHTML = `<span class="bad">● Chưa kết nối</span>${h.error ? " — " + esc(h.error) : " — đang thử lại…"}`;
  $("#hubURL").textContent = h.url && h.url.startsWith("http") ? h.url : "— (cần đăng nhập Đồng bộ)";
}
function openHub() {
  if (!state.sync || !state.sync.enabled) { toast("Hãy đăng nhập Đồng bộ trước — Trung tâm Zalo chạy qua máy chủ đồng bộ", "err"); return; }
  const hs = state.settings.hub || {};
  const chosen = new Set(hs.profiles || []);
  $("#hubOn").checked = !!hs.enabled;
  $("#hubList").innerHTML = state.profiles.filter((p) => !p.trashedAt).map((p) =>
    `<label><input type="checkbox" value="${esc(p.id)}" ${chosen.has(p.id) ? "checked" : ""}>${esc(p.name)}</label>`).join("") || '<span class="muted">Chưa có hồ sơ.</span>';
  $("#hubErr").textContent = "";
  $("#hubPass").value = "";
  renderHubStatus();
  $("#dlgHub").showModal();
}
$("#btnHub").addEventListener("click", openHub);
$("#hubSave").addEventListener("click", (e) => busyBtn(e.target, async () => {
  $("#hubErr").textContent = "";
  const profiles = [...document.querySelectorAll("#hubList input:checked")].map((c) => c.value);
  try {
    await api("POST", "/api/hub/settings", { enabled: $("#hubOn").checked, profiles });
  } catch (err) { $("#hubErr").textContent = err.message; return; }
  const open = profiles.filter((id) => byId(id)?.running).length;
  toast("Đã lưu Trung tâm Zalo" + (open ? `\n${open} hồ sơ đang mở: hãy đóng và mở lại để hình ảnh luôn cập nhật khi cửa sổ bị che.` : ""), "ok");
}));
$("#hubSetPass").addEventListener("click", (e) => busyBtn(e.target, async () => {
  const pw = $("#hubPass").value;
  if (pw.length < 8) throw new Error("Mật khẩu cần ít nhất 8 ký tự");
  await api("POST", "/api/hub/password", { password: pw });
  $("#hubPass").value = "";
  toast("Đã đặt mật khẩu Trung tâm Zalo", "ok");
}));
$("#hubCopy").addEventListener("click", () => { navigator.clipboard.writeText($("#hubURL").textContent); toast("Đã copy địa chỉ", "ok"); });
$("#hubOpen").addEventListener("click", () => api("POST", "/api/open-url").catch((err) => toast(err.message, "err")));

// ---------- VPS expiry ----------
function renderVpsBanner() {
  const b = $("#vpsWarn");
  const d = state.meta && state.meta.vpsExpiry;
  if (!d || !(state.sync && state.sync.enabled)) { b.hidden = true; return; }
  const today = new Date(); today.setHours(0, 0, 0, 0);
  const days = Math.round((new Date(d + "T00:00:00") - today) / 86400000);
  if (days > 7) { b.hidden = true; return; }
  const when = new Date(d).toLocaleDateString("vi-VN");
  b.className = "banner" + (days <= 2 ? " danger" : "");
  b.innerHTML = days >= 0
    ? `⏰ VPS sẽ hết hạn ngày <b>${when}</b> (còn ${days} ngày). Hãy gia hạn để không mất Proxy VPN và đồng bộ. <button class="btn sm" id="vpsRenewed">Đã gia hạn</button>`
    : `⛔ VPS đã quá hạn từ ngày <b>${when}</b>. Gia hạn ngay để Proxy VPN và đồng bộ hoạt động lại. <button class="btn sm" id="vpsRenewed">Đã gia hạn</button>`;
  b.hidden = false;
  $("#vpsRenewed").onclick = () => { openSettings(); setTimeout(() => $("#formSettings").vpsExpiry.focus(), 50); toast("Nhập ngày hết hạn mới của VPS rồi bấm Lưu"); };
}

// ---------- trash ----------
function renderTrash() {
  const list = state.profiles.filter((p) => p.trashedAt);
  $("#trEmpty").hidden = list.length > 0;
  $("#trRows").innerHTML = list.map((p) => {
    const left = Math.max(0, 7 - Math.floor((Date.now() - new Date(p.trashedAt)) / 86400000));
    return `<tr data-id="${p.id}"><td class="c-check"><input type="checkbox" class="trsel"></td>
      <td><b>${esc(p.name)}</b>${p.group ? ` <span class="chip">${esc(p.group)}</span>` : ""}</td>
      <td class="muted">${timeAgo(p.trashedAt)}</td><td class="muted">${left} ngày</td></tr>`;
  }).join("");
  $("#trAll").checked = false;
}
const trIDs = () => [...document.querySelectorAll("#trRows .trsel:checked")].map((c) => c.closest("tr").dataset.id);
$("#btnTrash").addEventListener("click", () => { renderTrash(); $("#dlgTrash").showModal(); });
$("#btnTile").addEventListener("click", (e) => busyBtn(e.target, async () => {
  const ids = [...selected].filter((id) => byId(id)?.running);
  const r = await api("POST", "/api/tile", { ids });
  toast(`Đã xếp ${r.arranged} cửa sổ`, "ok");
}));
$("#trAll").addEventListener("change", (e) => document.querySelectorAll("#trRows .trsel").forEach((c) => (c.checked = e.target.checked)));
$("#trRestore").addEventListener("click", (e) => busyBtn(e.target, async () => {
  const ids = trIDs();
  if (!ids.length) throw new Error("Hãy tick chọn hồ sơ cần khôi phục");
  await api("POST", "/api/restore", { ids });
  toast(`Đã khôi phục ${ids.length} hồ sơ`, "ok");
  await refresh(); renderTrash();
}));
$("#trDelete").addEventListener("click", async (e) => {
  const ids = trIDs();
  if (!ids.length) { toast("Hãy tick chọn hồ sơ cần xóa", "err"); return; }
  const ok = await confirmBox(`Xóa vĩnh viễn ${ids.length} hồ sơ?`, "Toàn bộ dữ liệu Chrome và đăng nhập của các hồ sơ này sẽ bị xóa trên mọi máy, KHÔNG khôi phục được.", "Xóa vĩnh viễn");
  if (!ok) return;
  busyBtn(e.target, async () => {
    await api("POST", "/api/delete", { ids, permanent: true });
    toast(`Đã xóa vĩnh viễn ${ids.length} hồ sơ`, "ok");
    await refresh(); renderTrash();
  });
});

// ---------- account quick view ----------
let accID = null, accTimer = null;
function openAccount(id) {
  const p = byId(id);
  accID = id;
  const a = p.account || {};
  $("#acTitle").textContent = "Tài khoản · " + p.name;
  const rows = [["UID", a.uid], ["Email / SĐT", a.email], ["Mật khẩu", a.password], ["Thông tin thêm", a.extra]].filter(([, v]) => v);
  $("#acRows").innerHTML = rows.length
    ? rows.map(([k, v], i) => `<div class="acrow"><span class="muted small">${k}</span><code>${k === "Mật khẩu" ? "••••••••" : esc(v)}</code><button type="button" class="btn sm" data-copy="${i}">Copy</button></div>`).join("")
    : '<p class="muted">Chưa có thông tin. Bấm "Sửa" để thêm UID, email, mật khẩu, mã 2FA.</p>';
  $("#acRows").onclick = (e) => {
    const b = e.target.closest("[data-copy]");
    if (!b) return;
    navigator.clipboard.writeText(rows[+b.dataset.copy][1]);
    toast(`Đã copy ${rows[+b.dataset.copy][0]}`, "ok");
  };
  $("#acErr").textContent = "";
  $("#acTotp").hidden = !a.twofa;
  clearInterval(accTimer);
  if (a.twofa) { tickTotp(); accTimer = setInterval(tickTotp, 1000); }
  $("#dlgAcc").showModal();
}
async function tickTotp() {
  if (!$("#dlgAcc").open) { clearInterval(accTimer); return; }
  try {
    const r = await api("GET", `/api/totp/${accID}`);
    $("#acCode").textContent = r.code.slice(0, 3) + " " + r.code.slice(3);
    $("#acBar").style.width = (r.remaining / 30) * 100 + "%";
  } catch (e) { $("#acErr").textContent = e.message; clearInterval(accTimer); }
}
$("#acCopyCode").addEventListener("click", () => {
  navigator.clipboard.writeText($("#acCode").textContent.replace(/\s/g, ""));
  toast("Đã copy mã 2FA", "ok");
});
$("#acEdit").addEventListener("click", () => { $("#dlgAcc").close(); openProfileDialog(accID); });

// ---------- clone ----------
let cloneSrc = null;
function openClone(id) {
  const p = byId(id);
  if (p.running) { toast("Hãy đóng hồ sơ gốc trước khi nhân bản", "err"); return; }
  cloneSrc = id;
  const f = $("#formClone");
  f.reset();
  $("#clSrc").textContent = p.name;
  f.name.value = p.name + " - bản sao";
  f.group.value = p.group || "";
  $("#clErr").textContent = "";
  $("#dlgClone").showModal();
  f.name.select();
}
$("#formClone").addEventListener("submit", async (e) => {
  e.preventDefault();
  const f = e.target;
  const btn = $("#clSubmit");
  btn.disabled = true;
  btn.textContent = "Đang sao chép…";
  try {
    const r = await api("POST", "/api/clone", {
      id: cloneSrc, name: f.name.value, count: parseInt(f.count.value, 10) || 1,
      group: f.group.value, copyProxy: f.copyProxy.checked,
    });
    $("#dlgClone").close();
    toast(`Đã nhân bản ${r.created} hồ sơ`, "ok");
    refresh();
  } catch (err) { $("#clErr").textContent = err.message; }
  btn.disabled = false;
  btn.textContent = "Nhân bản";
});

// ---------- Proxy VPN ----------
let vpn = null; // last status from the VPS
const VN_COUNTRY = { "Vietnam": "Việt Nam", "Singapore": "Singapore", "Japan": "Nhật Bản", "Hong Kong": "Hồng Kông", "Thailand": "Thái Lan",
  "South Korea": "Hàn Quốc", "Taiwan": "Đài Loan", "Malaysia": "Malaysia", "Indonesia": "Indonesia", "USA": "Mỹ", "UK": "Anh",
  "Germany": "Đức", "France": "Pháp", "Canada": "Canada", "Australia": "Úc", "Cambodia": "Campuchia", "Laos": "Lào" };
const cname = (c) => (VN_COUNTRY[c] ? `${VN_COUNTRY[c]} (${c})` : c);
// ExpressVPN location -> ISO 3166 code (for the flag)
const COUNTRY_ISO = { "Vietnam": "VN", "Singapore": "SG", "Japan": "JP", "Hong Kong": "HK", "Thailand": "TH", "South Korea": "KR",
  "Taiwan": "TW", "Malaysia": "MY", "Indonesia": "ID", "Philippines (via Singapore)": "PH", "Cambodia": "KH", "Laos": "LA",
  "Myanmar": "MM", "Macau": "MO", "India (via Singapore)": "IN", "India (via UK)": "IN", "USA": "US", "Canada": "CA", "Mexico": "MX",
  "UK": "GB", "Germany": "DE", "France": "FR", "Netherlands": "NL", "Spain": "ES", "Italy": "IT", "Switzerland": "CH", "Sweden": "SE",
  "Norway": "NO", "Denmark": "DK", "Finland": "FI", "Ireland": "IE", "Belgium": "BE", "Austria": "AT", "Poland": "PL",
  "Czech Republic": "CZ", "Romania": "RO", "Turkey": "TR", "Australia": "AU", "New Zealand": "NZ", "Brazil": "BR", "Argentina": "AR",
  "Chile": "CL", "Colombia": "CO", "Peru": "PE", "South Africa": "ZA", "Egypt": "EG", "Israel": "IL", "Albania": "AL", "Algeria": "DZ",
  "Andorra": "AD", "Azerbaijan": "AZ", "Bahamas": "BS", "Bangladesh": "BD", "Belarus": "BY", "Bermuda": "BM", "Bhutan": "BT",
  "Bolivia": "BO", "Brunei": "BN", "Bulgaria": "BG", "Cayman Islands": "KY", "Costa Rica": "CR", "Croatia": "HR", "Cuba": "CU",
  "Cyprus": "CY", "Dominican Republic": "DO", "Ecuador": "EC", "Estonia": "EE", "Georgia": "GE", "Ghana": "GH", "Greece": "GR",
  "Guam": "GU", "Guatemala": "GT", "Honduras": "HN", "Hungary": "HU", "Iceland": "IS", "Jamaica": "JM", "Kazakhstan": "KZ",
  "Latvia": "LV", "Liechtenstein": "LI", "Lithuania": "LT", "Luxembourg": "LU", "Moldova": "MD", "Mongolia": "MN", "Morocco": "MA",
  "Nepal": "NP", "North Macedonia": "MK", "Panama": "PA", "Portugal": "PT", "Puerto Rico": "PR", "Serbia": "RS", "Slovakia": "SK",
  "Slovenia": "SI", "Sri Lanka": "LK", "Trinidad and Tobago": "TT", "Ukraine": "UA", "Uruguay": "UY", "Venezuela": "VE" };
function flagOf(iso) {
  if (!iso || !/^[A-Za-z]{2}$/.test(iso)) return "";
  return String.fromCodePoint(...[...iso.toUpperCase()].map((ch) => 0x1f1e6 + ch.charCodeAt(0) - 65));
}
const flagHTML = (iso) => (flagOf(iso) ? `<span class="flag">${flagOf(iso)}</span>` : "");
const countryFlag = (c) => flagOf(COUNTRY_ISO[c]);
// The VPN proxy a profile uses, if any.
function vpnOfProfile(p) {
  if (!vpn || !vpn.proxies || !p.proxy) return null;
  return vpn.proxies.find((x) => p.proxy.host === vpnHost() && Number(p.proxy.port) === x.port) || null;
}
function vpnHost() {
  try { return new URL(state.sync.server).hostname; } catch { return ""; }
}
function vpnLine(p) { return `${vpnHost()}:${p.port}:${p.user}:${p.pass}`; }
function vpnUsers(p) {
  const host = vpnHost();
  return state.profiles.filter((x) => x.proxy && x.proxy.host === host && Number(x.proxy.port) === p.port);
}
async function loadVpn() {
  vpn = await api("GET", "/api/vpn/status");
  return vpn;
}
async function fillVpnSelect() {
  const field = $("#pfVpnField");
  field.hidden = true;
  if (!state.sync || !state.sync.enabled) return;
  try { if (!vpn) await loadVpn(); } catch { return; }
  if (!vpn.proxies || !vpn.proxies.length) return;
  const f = $("#formProfile");
  const sel = $("#pfVpn");
  sel.innerHTML = '<option value="">— Chọn —</option>' + vpn.proxies.map((p) =>
    `<option value="${esc(p.id)}">${countryFlag(p.country)} ${esc(p.name)} · ${esc(cname(p.country))}${p.ip ? " · " + esc(p.ip) : ""} · ${vpnUsers(p).length} hồ sơ</option>`).join("");
  const cur = vpn.proxies.find((p) => f.phost.value === vpnHost() && Number(f.pport.value) === p.port);
  sel.value = cur ? cur.id : "";
  field.hidden = false;
}
$("#pfVpn").addEventListener("change", (e) => {
  const p = vpn && vpn.proxies.find((x) => x.id === e.target.value);
  if (!p) return;
  const f = $("#formProfile");
  f.ptype.value = "http"; f.phost.value = vpnHost(); f.pport.value = p.port; f.puser.value = p.user; f.ppass.value = p.pass;
  $("#pfCheckRes").textContent = "";
});

function vstate(st) {
  if (/healthy\)/.test(st) && !/unhealthy/.test(st)) return ["ok", "Đã kết nối"];
  if (/^Up/.test(st)) return /unhealthy/.test(st) ? ["bad", "Lỗi kết nối VPN"] : ["wait", "Đang kết nối…"];
  if (/Restarting/.test(st)) return ["wait", "Đang khởi động lại…"];
  return ["bad", st || "Không chạy"];
}
function renderVpn() {
  const v = vpn || {};
  $("#vpErr").textContent = !v.docker ? "Docker chưa chạy trên VPS — hãy cập nhật máy chủ (chạy lại install.sh)." : (!v.tun ? "VPS không hỗ trợ /dev/net/tun — không chạy được VPN trên VPS này." : "");
  $("#vpCredsForm").hidden = !!v.credsSet && !$("#vpCredsBox").dataset.edit;
  $("#vpCredsInfo").innerHTML = v.credsSet ? `Đã lưu tài khoản <b>${esc(v.credUser)}</b> · <a href="#" id="vpEditCreds">Đổi tài khoản</a>` : "Chưa nhập tài khoản ExpressVPN.";
  const ec = $("#vpEditCreds");
  if (ec) ec.onclick = (e) => { e.preventDefault(); $("#vpCredsBox").dataset.edit = "1"; renderVpn(); };
  $("#vpCreate").hidden = !v.credsSet;
  const sel = $("#vpCountry");
  if (!sel.options.length && v.countries) sel.innerHTML = v.countries.map((c) => `<option value="${esc(c)}">${countryFlag(c)} ${esc(cname(c))}</option>`).join("");
  const list = v.proxies || [];
  $("#vpEmpty").hidden = list.length > 0;
  $("#vpRows").innerHTML = list.map((p) => {
    const [cls, label] = vstate(p.status);
    const users = vpnUsers(p);
    const ipinfo = p.ip ? `${flagHTML(p.ipCountry)}${esc(p.ip)}<div class="muted small">${esc([p.city, p.ipCountry].filter(Boolean).join(", "))}</div>` : '<span class="muted">—</span>';
    return `<tr data-vid="${esc(p.id)}">
      <td>${flagHTML(COUNTRY_ISO[p.country])}<b>${esc(p.name)}</b><div class="muted small">${esc(cname(p.country))}</div></td>
      <td class="proxy">${esc(vpnHost())}:${p.port}</td>
      <td>${ipinfo}</td>
      <td><span class="vstate ${cls}" title="${esc(p.status)}">${esc(label)}</span></td>
      <td title="${esc(users.map((u) => u.name).join(", "))}">${users.length} hồ sơ</td>
      <td><div class="rowact">
        <button type="button" class="btn sm" data-v="assign" title="Gán proxy này cho các hồ sơ đang chọn">Gán cho ${selected.size} hồ sơ</button>
        <button type="button" class="btn sm" data-v="check">Kiểm tra IP</button>
        <button type="button" class="btn sm" data-v="restart" title="Kết nối lại VPN, thường sẽ ra IP mới cùng quốc gia">Đổi IP</button>
        <button type="button" class="btn sm" data-v="copy" title="Copy dạng host:port:user:pass">Copy</button>
        <button type="button" class="btn sm" data-v="logs">Nhật ký</button>
        <button type="button" class="btn sm danger" data-v="del">Xóa</button>
      </div></td></tr>`;
  }).join("");
}
async function openVpn() {
  if (!state.sync || !state.sync.enabled) { toast("Hãy đăng nhập Đồng bộ trước — Proxy VPN chạy trên VPS của bạn", "err"); return; }
  $("#vpLog").hidden = true;
  delete $("#vpCredsBox").dataset.edit;
  $("#dlgVpn").showModal();
  $("#vpErr").textContent = "Đang tải…";
  try { await loadVpn(); renderVpn(); } catch (e) { $("#vpErr").textContent = e.message; }
}
$("#btnVpn").addEventListener("click", openVpn);
$("#vpSaveCreds").addEventListener("click", (e) => busyBtn(e.target, async () => {
  await api("PUT", "/api/vpn/creds", { user: $("#vpUser").value, pass: $("#vpPass").value });
  $("#vpUser").value = $("#vpPass").value = "";
  delete $("#vpCredsBox").dataset.edit;
  toast("Đã lưu tài khoản ExpressVPN trên VPS", "ok");
  await loadVpn(); renderVpn();
}));
$("#vpAdd").addEventListener("click", (e) => busyBtn(e.target, async () => {
  const p = await api("POST", "/api/vpn/proxies", { country: $("#vpCountry").value, name: $("#vpName").value });
  $("#vpName").value = "";
  toast(`Đã tạo proxy ${p.name}. VPN cần 20–60 giây để kết nối, sau đó bấm "Kiểm tra IP".`, "ok");
  await loadVpn(); renderVpn();
}));
$("#vpRows").addEventListener("click", async (e) => {
  const b = e.target.closest("[data-v]");
  if (!b) return;
  const id = b.closest("tr").dataset.vid;
  const p = vpn.proxies.find((x) => x.id === id);
  const act = b.dataset.v;
  if (act === "copy") { navigator.clipboard.writeText(vpnLine(p)); toast("Đã copy proxy", "ok"); return; }
  if (act === "del") {
    const n = vpnUsers(p).length;
    if (!(await confirmBox("Xóa proxy?", `${p.name} (${cname(p.country)})` + (n ? `\n${n} hồ sơ đang dùng proxy này sẽ mất mạng cho tới khi gán proxy khác.` : ""), "Xóa"))) return;
  }
  busyBtn(b, async () => {
    if (act === "assign") {
      const ids = [...selected];
      if (!ids.length) throw new Error("Hãy tick chọn hồ sơ ở bảng chính trước");
      await api("POST", "/api/proxy/assign", { ids, text: vpnLine(p), type: "http" });
      ids.forEach((x) => checks.delete(x));
      toast(`Đã gán ${p.name} cho ${ids.length} hồ sơ` + (ids.some((x) => byId(x)?.running) ? ". Hồ sơ đang mở cần đóng và mở lại." : ""), "ok");
      await refresh();
    } else if (act === "check") {
      const r = await api("POST", `/api/vpn/proxies/${id}/check`);
      toast(`${countryFlag(p.country)} ${p.name}: IP ${r.ip} · ${flagOf(r.country)} ${[r.city, r.country].filter(Boolean).join(", ")} · ${r.ms} ms`, "ok");
    } else if (act === "restart") {
      await api("POST", `/api/vpn/proxies/${id}/restart`);
      toast("Đang kết nối lại. Đợi ~30 giây rồi bấm Kiểm tra IP.", "ok");
    } else if (act === "logs") {
      const r = await api("GET", `/api/vpn/proxies/${id}/logs`);
      $("#vpLog").textContent = r.logs || "(trống)";
      $("#vpLog").hidden = false;
    } else if (act === "del") {
      await api("DELETE", `/api/vpn/proxies/${id}`);
      toast("Đã xóa proxy", "ok");
    }
    await loadVpn(); renderVpn();
  });
});

function formProxy(f) {
  return {
    type: f.ptype.value,
    host: f.phost.value.trim(),
    port: parseInt(f.pport.value, 10) || 0,
    user: f.puser.value,
    pass: f.ppass.value,
  };
}

// Client-side version of the server's proxy line parser, for the "paste" box.
function parseProxyLine(line) {
  line = line.trim();
  if (!line) return null;
  let type = "http";
  const m = line.match(/^(\w+):\/\/(.*)$/);
  if (m) { type = /^socks/i.test(m[1]) ? "socks5" : "http"; line = m[2]; }
  let user = "", pass = "";
  const at = line.lastIndexOf("@");
  if (at > 0) { [user, pass = ""] = splitOnce(line.slice(0, at), ":"); line = line.slice(at + 1); }
  const parts = line.split(":");
  if (parts.length !== 2 && parts.length !== 4) return null;
  const port = parseInt(parts[1], 10);
  if (!parts[0] || !(port > 0 && port < 65536)) return null;
  if (parts.length === 4) { user = parts[2]; pass = parts[3]; }
  return { type, host: parts[0], port, user, pass };
}
function splitOnce(s, sep) { const i = s.indexOf(sep); return i < 0 ? [s] : [s.slice(0, i), s.slice(i + 1)]; }

$("#pfPaste").addEventListener("input", (e) => {
  const p = parseProxyLine(e.target.value);
  if (!p) return;
  const f = $("#formProfile");
  if (!f.ptype.value || /:\/\//.test(e.target.value)) f.ptype.value = p.type;
  f.phost.value = p.host; f.pport.value = p.port; f.puser.value = p.user; f.ppass.value = p.pass;
});

$("#pfCheck").addEventListener("click", async () => {
  const f = $("#formProfile");
  const res = $("#pfCheckRes");
  const px = formProxy(f);
  res.className = "muted";
  res.textContent = px.type ? "Đang kiểm tra…" : "Đang kiểm tra kết nối trực tiếp (không proxy)…";
  try {
    const c = await api("POST", "/api/proxy/check", { proxy: px });
    if (c.ok) {
      res.className = "ok";
      res.textContent = `✓ ${flagOf(c.country)} IP: ${c.ip}${c.city || c.country ? " · " + [c.city, c.country].filter(Boolean).join(", ") : ""} · ${c.ms} ms`;
    } else {
      res.className = "bad";
      res.textContent = "✕ " + c.error;
    }
  } catch (e) { res.className = "bad"; res.textContent = "✕ " + e.message; }
});

$("#formProfile").addEventListener("submit", async (e) => {
  e.preventDefault();
  const f = e.target;
  const body = {
    name: f.name.value, group: f.group.value, notes: f.notes.value, startUrl: f.startUrl.value,
    proxy: formProxy(f), count: parseInt(f.count.value, 10) || 1,
    extensions: extIDs(f.exts.value),
    siteDomains: f.sites.value,
    account: { uid: f.auid.value, email: f.aemail.value, password: f.apass.value, twofa: f.a2fa.value, extra: f.aextra.value },
  };
  if (f.exts.value.trim() && !body.extensions.length) {
    $("#pfErr").textContent = "Không nhận ra link tiện ích. Hãy dán link dạng https://chromewebstore.google.com/detail/…";
    return;
  }
  if (!editingId) localStorage.setItem("lastExts", f.exts.value);
  if (body.proxy.type && (!body.proxy.host || !body.proxy.port)) {
    $("#pfErr").textContent = "Proxy cần có Host và Cổng (hoặc chọn \"Không dùng\")";
    return;
  }
  try {
    if (editingId) {
      await api("PUT", "/api/profiles/" + editingId, body);
      checks.delete(editingId);
      const p = byId(editingId);
      toast(p && p.running ? "Đã lưu. Đóng và mở lại hồ sơ để áp dụng proxy mới." : "Đã lưu hồ sơ", "ok");
    } else {
      const r = await api("POST", "/api/profiles", body);
      toast(`Đã tạo ${r.created} hồ sơ`, "ok");
    }
    $("#dlgProfile").close();
    refresh();
  } catch (err) { $("#pfErr").textContent = err.message; }
});

// ---------- bulk dialogs ----------
$("#formProxy").addEventListener("submit", async (e) => {
  e.preventDefault();
  const f = e.target;
  const ids = [...selected];
  try {
    const r = await api("POST", "/api/proxy/assign", { ids, text: f.text.value, type: f.type.value });
    ids.forEach((id) => checks.delete(id));
    toast(r.proxies ? `Đã gán ${r.proxies} proxy cho ${r.assigned} hồ sơ` : `Đã gỡ proxy khỏi ${r.assigned} hồ sơ`, "ok");
    $("#dlgProxy").close();
    refresh();
  } catch (err) { $("#bpErr").textContent = err.message; }
});

$("#formGroup").addEventListener("submit", async (e) => {
  e.preventDefault();
  const group = e.target.group.value;
  const ids = [...selected];
  try {
    for (const id of ids) {
      const p = byId(id);
      if (p) await api("PUT", "/api/profiles/" + id, { name: p.name, group, notes: p.notes, startUrl: p.startUrl, proxy: p.proxy });
    }
    toast(`Đã đổi nhóm cho ${ids.length} hồ sơ`, "ok");
    $("#dlgGroup").close();
    refresh();
  } catch (err) { toast(err.message, "err"); }
});

$("#formSettings").addEventListener("submit", async (e) => {
  e.preventDefault();
  try {
    if (e.target.vpsExpiry.value && e.target.vpsExpiry.value !== state.meta?.vpsExpiry) {
      await api("POST", "/api/meta", { vpsExpiry: e.target.vpsExpiry.value });
    }
    await api("POST", "/api/settings", {
      chromePath: e.target.chromePath.value,
      allowMultiOpen: e.target.allowMultiOpen.checked,
      chromeLang: e.target.chromeLang.value,
      cacheMB: parseInt(e.target.cacheMB.value, 10) || 0,
    });
    toast("Đã lưu cài đặt", "ok");
    $("#dlgSettings").close();
    refresh();
  } catch (err) { $("#setErr").textContent = err.message; }
});

function openSettings() {
  const f = $("#formSettings");
  f.chromePath.value = state.settings.chromePath || "";
  f.allowMultiOpen.checked = !!state.settings.allowMultiOpen;
  f.chromeLang.value = state.settings.chromeLang || "vi";
  f.vpsExpiry.value = (state.meta && state.meta.vpsExpiry) || "";
  f.cacheMB.value = String(state.settings.cacheMB || 0);
  $("#setChrome").textContent = state.chromePath || "Không tìm thấy";
  $("#setData").textContent = state.dataDir;
  $("#setVersion").textContent = state.version || "—";
  $("#setErr").textContent = "";
  $("#dlgSettings").showModal();
}

// ---------- events ----------
$("#rows").addEventListener("click", (e) => {
  const el = e.target.closest("[data-act]");
  if (!el) return;
  const id = el.closest("tr").dataset.id;
  switch (el.dataset.act) {
    case "sel": el.checked ? selected.add(id) : selected.delete(id); render(); break;
    case "open": openProfiles([id]); break;
    case "glogin": openGLogin(id); break;
    case "close": closeProfiles([id]); break;
    case "edit": openProfileDialog(id); break;
    case "check": checkProfiles([id]); break;
    case "clone": openClone(id); break;
    case "pin": api("POST", "/api/pin", { ids: [id], pinned: !byId(id).pinned }).then(refresh).catch((e) => toast(e.message, "err")); break;
    case "acc": openAccount(id); break;
    case "folder": api("POST", `/api/folder/${id}`).catch((err) => toast(err.message, "err")); break;
  }
});
$("#rows").addEventListener("dblclick", (e) => {
  if (e.target.closest("button, input")) return;
  const tr = e.target.closest("tr");
  if (tr) openProfileDialog(tr.dataset.id);
});

$("#checkAll").addEventListener("change", (e) => {
  const vis = filtered().map((p) => p.id);
  vis.forEach((id) => (e.target.checked ? selected.add(id) : selected.delete(id)));
  render();
});

$("#bulkbar").addEventListener("click", (e) => {
  const b = e.target.closest("[data-bulk]");
  if (!b) return;
  const ids = [...selected];
  switch (b.dataset.bulk) {
    case "open": openProfiles(ids); break;
    case "close": closeProfiles(ids); break;
    case "check": checkProfiles(ids); break;
    case "delete": deleteProfiles(ids); break;
    case "clear": selected.clear(); render(); break;
    case "cache": clearCache(ids); break;
    case "proxy":
      $("#formProxy").reset(); $("#bpErr").textContent = ""; $("#bpCount").textContent = ids.length;
      $("#dlgProxy").showModal(); break;
    case "group":
      $("#formGroup").reset(); $("#bgCount").textContent = ids.length;
      $("#dlgGroup").showModal(); break;
  }
});

document.querySelectorAll("[data-close]").forEach((b) => b.addEventListener("click", () => b.closest("dialog").close()));
$("#btnNew").addEventListener("click", () => openProfileDialog());
$("#btnNew2").addEventListener("click", () => openProfileDialog());
$("#btnSettings").addEventListener("click", openSettings);
$("#btnSync").addEventListener("click", () => (state.sync && state.sync.enabled ? openSyncPanel() : openLogin()));
$("#warnSettings").addEventListener("click", (e) => { e.preventDefault(); openSettings(); });
["#q", "#fGroup", "#fStatus"].forEach((s) => $(s).addEventListener("input", render));
$("#fSort").value = localStorage.getItem("sort") || "";
$("#fSort").addEventListener("input", () => { localStorage.setItem("sort", $("#fSort").value); render(); });

document.addEventListener("keydown", (e) => {
  if (e.ctrlKey && e.key.toLowerCase() === "n" && !document.querySelector("dialog[open]")) { e.preventDefault(); openProfileDialog(); }
  if (e.ctrlKey && e.key.toLowerCase() === "f" && !document.querySelector("dialog[open]")) { e.preventDefault(); $("#q").focus(); }
});

// ---------- self update ----------
let updateInfo = null;
async function checkUpdate() {
  try {
    updateInfo = await api("GET", "/api/update/check");
    const b = $("#btnUpdate");
    b.hidden = !updateInfo.available;
    if (updateInfo.available) {
      b.textContent = `⬆ Cập nhật lên ${updateInfo.latest}`;
      b.title = updateInfo.notes || "Có bản mới";
    }
  } catch { /* offline: ignore */ }
}
$("#btnUpdate").addEventListener("click", async () => {
  const running = state.profiles.filter((p) => p.running).length;
  const ok = await confirmBox(
    `Cập nhật lên bản ${updateInfo.latest}?`,
    (updateInfo.notes ? updateInfo.notes + "\n\n" : "") +
      "Phần mềm sẽ tự tải bản mới, đóng và mở lại (khoảng 10 giây). Hồ sơ và đăng nhập giữ nguyên." +
      (running ? `\n\n${running} hồ sơ đang mở vẫn chạy bình thường; proxy có mật khẩu sẽ ngắt vài giây rồi tự nối lại.` : ""),
    "Cập nhật ngay",
  );
  if (!ok) return;
  const b = $("#btnUpdate");
  b.disabled = true;
  b.textContent = "Đang tải bản mới…";
  try {
    await api("POST", "/api/update/apply");
    b.textContent = "Đang khởi động lại…";
  } catch (e) {
    toast(e.message, "err");
    b.disabled = false;
    b.textContent = `⬆ Cập nhật lên ${updateInfo.latest}`;
  }
});
setTimeout(checkUpdate, 3000);
setInterval(checkUpdate, 30 * 60 * 1000);

// ---------- sync UI ----------
function renderSyncBtn() {
  const sy = state.sync || {};
  const dot = $("#syncDot");
  let label = "Đồng bộ: tắt", cls = "";
  if (sy.enabled) {
    if (sy.busy) { label = sy.busy; cls = "busy"; }
    else if (sy.error) { label = "Lỗi đồng bộ"; cls = "err"; }
    else { label = sy.username + (sy.lastSync ? " · " + timeAgo(sy.lastSync) : ""); cls = "ok"; }
  } else if (sy.username && sy.server) {
    label = "Cần đăng nhập lại"; cls = "err";
  }
  dot.className = "sdot " + cls;
  $("#syncLabel").textContent = label;
  $("#btnSync").title = sy.error ? sy.error : "Đồng bộ online";
  if ($("#dlgSync").open) fillSyncPanel();
}

let registering = false;
function setRegister(on) {
  registering = on;
  $("#lgRegister").hidden = !on;
  $("#lgTitle").textContent = on ? "Tạo tài khoản đồng bộ" : "Đăng nhập đồng bộ";
  $("#lgSubmit").textContent = on ? "Tạo tài khoản" : "Đăng nhập";
  $("#lgToggle").textContent = on ? "Đã có tài khoản? Đăng nhập" : "Chưa có tài khoản? Tạo mới";
  $("#formLogin").password.autocomplete = on ? "new-password" : "current-password";
}
function openLogin() {
  const f = $("#formLogin");
  f.reset();
  f.server.value = state.sync?.server || DEFAULT_SERVER;
  f.username.value = state.sync?.username || "";
  $("#lgErr").textContent = "";
  setRegister(false);
  $("#dlgLogin").showModal();
  (f.username.value ? f.password : f.username).focus();
}
$("#lgToggle").addEventListener("click", (e) => { e.preventDefault(); setRegister(!registering); });
$("#formLogin").addEventListener("submit", async (e) => {
  e.preventDefault();
  const f = e.target;
  if (registering && f.password.value !== f.password2.value) { $("#lgErr").textContent = "Hai mật khẩu không khớp"; return; }
  const btn = $("#lgSubmit");
  btn.disabled = true;
  $("#lgErr").textContent = "";
  btn.textContent = "Đang xử lý…";
  try {
    const r = await api("POST", "/api/sync/login", {
      server: f.server.value, username: f.username.value, password: f.password.value,
      code: f.code.value, register: registering,
    });
    $("#dlgLogin").close();
    toast(registering ? "Đã tạo tài khoản và bật đồng bộ" : `Đã đăng nhập. Có ${r.profiles} hồ sơ.`, "ok");
    if (r.warning) toast(r.warning, "err");
    await refresh();
    if (registering && state.profiles.length) openSyncPanel(true);
  } catch (err) { $("#lgErr").textContent = err.message; }
  btn.disabled = false;
  setRegister(registering);
});

function fillSyncPanel() {
  const sy = state.sync || {};
  $("#syUser").textContent = sy.username || "—";
  $("#syServer").textContent = sy.server || "—";
  $("#syDevice").textContent = sy.deviceName || "—";
  $("#syLast").textContent = sy.lastSync ? new Date(sy.lastSync).toLocaleString("vi-VN") : "Chưa";
  $("#syErr").textContent = sy.error || "";
  $("#syBusy").textContent = sy.busy || "";
}
function openSyncPanel(suggestUpload) {
  fillSyncPanel();
  $("#bkRes").textContent = suggestUpload
    ? "Gợi ý: bấm \"Lưu đăng nhập của tất cả hồ sơ\" để đưa trạng thái đăng nhập hiện có lên máy chủ."
    : "";
  $("#dlgSync").showModal();
}
async function busyBtn(btn, fn) {
  const t = btn.textContent;
  btn.disabled = true;
  btn.textContent = "Đang xử lý…";
  try { await fn(); } catch (e) { toast(e.message, "err"); }
  btn.disabled = false;
  btn.textContent = t;
  refresh();
}
$("#syLog").addEventListener("click", () => api("POST", "/api/open-path", { path: state.dataDir }).catch((err) => toast(err.message, "err")));
$("#syNow").addEventListener("click", (e) => busyBtn(e.target, async () => {
  await api("POST", "/api/sync/now");
  toast("Đã đồng bộ", "ok");
}));
$("#syUploadAll").addEventListener("click", (e) => busyBtn(e.target, async () => {
  const poll = setInterval(refresh, 1000);
  try {
    const r = await api("POST", "/api/sync/upload-all");
    toast(`Đã lưu đăng nhập của ${r.uploaded} hồ sơ` + (r.warning ? `\nLỗi: ${r.warning}` : ""), r.warning ? "err" : "ok");
  } finally { clearInterval(poll); }
}));
$("#bkExport").addEventListener("click", (e) => busyBtn(e.target, async () => {
  const pass = $("#bkPass").value;
  if (pass.length < 8) throw new Error("Mật khẩu file sao lưu cần ít nhất 8 ký tự");
  const r = await api("POST", "/api/sync/export", { password: pass });
  $("#bkRes").innerHTML = `Đã lưu ${r.profiles} hồ sơ vào:<br><code>${esc(r.path)}</code> <a href="#" id="bkOpen">Mở thư mục</a><br>Hãy cất file này lên Google Drive/USB và nhớ mật khẩu.`;
  $("#bkOpen").onclick = (ev) => { ev.preventDefault(); api("POST", "/api/open-path", { path: r.dir }); };
}));
$("#bkImport").addEventListener("click", (e) => busyBtn(e.target, async () => {
  const file = $("#bkFile").files[0];
  if (!file) throw new Error("Hãy chọn file sao lưu (.qlcbak)");
  const pass = $("#bkPass").value;
  if (!pass) throw new Error("Nhập mật khẩu của file sao lưu vào ô phía trên");
  const buf = new Uint8Array(await file.arrayBuffer());
  let bin = "";
  for (let i = 0; i < buf.length; i += 0x8000) bin += String.fromCharCode.apply(null, buf.subarray(i, i + 0x8000));
  const r = await api("POST", "/api/sync/import", { data: btoa(bin), password: pass });
  $("#bkRes").textContent = `Đã khôi phục ${r.profiles} hồ sơ.`;
}));
$("#pwChange").addEventListener("click", (e) => busyBtn(e.target, async () => {
  await api("POST", "/api/sync/password", { old: $("#pwOld").value, new: $("#pwNew").value });
  $("#pwOld").value = $("#pwNew").value = "";
  toast("Đã đổi mật khẩu", "ok");
}));
$("#syLogout").addEventListener("click", async () => {
  const ok = await confirmBox("Đăng xuất đồng bộ?", "Hồ sơ vẫn giữ trên máy này nhưng sẽ không đồng bộ nữa. Đăng nhập lại bất cứ lúc nào.", "Đăng xuất");
  if (!ok) return;
  await api("POST", "/api/sync/logout").catch((err) => toast(err.message, "err"));
  $("#dlgSync").close();
  refresh();
});

refresh();
setInterval(() => {
  const open = document.querySelector("dialog[open]");
  if (!open || open.id === "dlgSync" || open.id === "dlgHub") refresh();
}, 2000);
