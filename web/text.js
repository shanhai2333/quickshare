'use strict';

/* QuickShare 文本页。
 *
 * 用途：把一段文本从这台设备丢到另一台设备上，不用登录任何聊天软件。
 * 内容存服务端数据库（几 KB 的东西不值得落成文件），页面间靠 SSE 自动同步。
 */

const state = {
  config: null,
  token: localStorage.getItem('qs_token') || '',
  texts: [],
  devices: [],
  editingId: null,
  ttl: { value: 0, unit: 'day' },
  // 验证码那一档（服务端默认 10 分钟）。它只影响"被识别成验证码"的那几条，
  // 所以列表头那句提示要把它单独说出来，不然用户会以为整页都是这个时长。
  codeTTL: { value: 10, unit: 'minute' },

  // 搜索与筛选**全在前端做**：文本已经一次性全拉进内存了，本地过滤零延迟，
  // 也不用每敲一个字就发一次请求。文本量（几十到几百条）远没到需要服务端
  // 分页或全文检索的规模——真到了那一步再说。
  query: '',
  deviceFilter: '',
  selectMode: false,
  selected: new Set(),
};

// 与服务端 maxTextLen 对齐。服务端是按**字节**算的（32 KiB），
// 所以这里也必须按 UTF-8 字节数校验——一个汉字占 3 字节，
// 按 content.length（UTF-16 码元）算会低估三倍，让用户以为还能贴。
const TEXT_MAX = 32 << 10;

const $ = (id) => document.getElementById(id);

/* ------------------------------------------------------------ 工具 */

function esc(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
  }[c]));
}

function fmtSize(n) {
  if (!n) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0, v = n;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return (i === 0 ? v : v.toFixed(1)) + ' ' + units[i];
}

const p2 = (n) => String(n).padStart(2, '0');

function fmtTime(sec) {
  const d = new Date(sec * 1000);
  const diff = (Date.now() - d.getTime()) / 1000;
  if (diff < 60) return '刚刚';
  if (diff < 3600) return Math.floor(diff / 60) + ' 分钟前';
  if (diff < 86400) return Math.floor(diff / 3600) + ' 小时前';
  if (diff < 86400 * 7) return Math.floor(diff / 86400) + ' 天前';
  return `${d.getFullYear()}-${p2(d.getMonth() + 1)}-${p2(d.getDate())}`;
}

function utf8Len(s) {
  return new TextEncoder().encode(s).length;
}

function toast(msg, kind = '') {
  const el = document.createElement('div');
  el.className = 'toast ' + kind;
  el.textContent = msg;
  $('toasts').appendChild(el);
  setTimeout(() => {
    el.style.transition = 'opacity .2s';
    el.style.opacity = '0';
    setTimeout(() => el.remove(), 220);
  }, 2800);
}

/* ------------------------------------------------------------ 设备标识 */

// 设备身份由**服务端**从请求来源 IP 推导，前端既不生成也不上报。
//
// 早先这里是浏览器生成一个随机串存在 localStorage 里。问题是 localStorage
// 严格按 origin 隔离：同一个服务用 127.0.0.1、localhost、内网 IP 打开就是三个
// 不同的 origin，各存各的串——同一台机器被记成三台设备，用户"新开个窗口"
// 就会看到设备列表里多一条。
//
// 现在前端只消费服务端给的 isMe 标记，自己不需要知道"我是谁"。

/* ------------------------------------------------------------ 请求 */

async function api(method, path, body) {
  const headers = {};
  if (state.token) headers['X-Admin-Token'] = state.token;
  if (body !== undefined) headers['Content-Type'] = 'application/json';

  const res = await fetch(path, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
  });

  if (res.status === 401) {
    showAuth();
    throw new Error('需要访问口令');
  }
  if (!res.ok) {
    let msg = `请求失败 (${res.status})`;
    try { msg = (await res.json()).error || msg; } catch (_) { /* 忽略 */ }
    throw new Error(msg);
  }
  const ct = res.headers.get('content-type') || '';
  return ct.includes('application/json') ? res.json() : null;
}

/* ------------------------------------------------------------ 复制 */

// 复制文本到剪贴板。
//
// navigator.clipboard 只在"安全上下文"里可用（HTTPS 或 localhost）。
// 内网部署基本都是 http://192.168.x.x:8080，那里它压根不存在——
// 所以必须留一条 execCommand 的降级路径，否则这功能在真实环境里直接失效。
async function copyText(text) {
  if (navigator.clipboard && window.isSecureContext) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch (_) { /* 落到降级路径 */ }
  }
  return legacyCopy(text);
}

function legacyCopy(text) {
  const ta = document.createElement('textarea');
  ta.value = text;
  ta.setAttribute('readonly', '');
  // 不能用 display:none / visibility:hidden，那样根本选不中
  ta.style.position = 'fixed';
  ta.style.top = '-1000px';
  ta.style.opacity = '0';
  document.body.appendChild(ta);

  const sel = document.getSelection();
  const saved = sel && sel.rangeCount ? sel.getRangeAt(0) : null;

  ta.select();
  ta.setSelectionRange(0, text.length); // iOS Safari 少了这句选不中
  let ok = false;
  try { ok = document.execCommand('copy'); } catch (_) { ok = false; }
  ta.remove();

  if (saved && sel) { sel.removeAllRanges(); sel.addRange(saved); }
  return ok;
}

/* ------------------------------------------------------------ 初始化 */

function showAuth() {
  $('authCard').hidden = false;
  $('composeCard').hidden = true;
  $('textsCard').hidden = true;
  $('authInput').focus();
}

async function init() {
  // config 和 settings **并行发**：两者互不依赖（一个决定要不要口令、一个管外观），
  // 串着等会白白多出一个往返。切页后列表迟迟不出来，有一部分就是这个。
  // 失败都收敛成 null，下面按"缺了就退回默认"处理，行为跟原来一致。
  const [config, settings] = await Promise.all([
    fetch('/api/config').then((r) => r.json()).catch(() => null),
    fetch('/api/settings').then((r) => r.json()).catch(() => null),
  ]);
  if (!config) {
    toast('无法连接服务', 'err');
    return;
  }
  state.config = config;

  // 外观要在鉴权判断之前套上，否则开口令的部署每次刷新都闪一下默认配色。
  // 拉不到也不影响主流程，退回默认外观继续。
  if (settings) QSSettings.apply(settings);
  else QSSettings.syncThemeUI();

  if (state.config.needAuth && !state.token) {
    showAuth();
    return;
  }
  await refreshAll();
  connectEvents();
}

async function refreshAll() {
  try {
    const [texts, devices, settings] = await Promise.all([
      api('GET', '/api/texts'),
      api('GET', '/api/devices'),
      // 保留时长是公开接口，直接 fetch；它决定列表头部那句提示
      fetch('/api/settings').then((r) => r.json()),
    ]);
    state.texts = texts || [];
    state.devices = devices || [];
    state.ttl = (settings && settings.textTTL) || { value: 0, unit: 'day' };
    state.codeTTL = (settings && settings.codeTTL) || { value: 10, unit: 'minute' };

    // 选中的条目可能已经不在了（本机删的、或别的设备删的）。不清掉的话
    // 会留下幽灵选中项：计数显示"已选 2 条"，点删除却提示"没选"。
    pruneSelection();

    $('authCard').hidden = true;
    $('composeCard').hidden = false;
    $('textsCard').hidden = false;
    renderDeviceHint();
    renderFilters();
    renderTTL();
    renderTexts();
    syncBulkBar();
    return true;
  } catch (e) {
    toast(e.message, 'err');
    return false;
  }
}

function pruneSelection() {
  if (!state.selected.size) return;
  const alive = new Set(state.texts.map((t) => t.id));
  for (const id of [...state.selected]) {
    if (!alive.has(id)) state.selected.delete(id);
  }
}

// 当前筛选条件下要显示的文本。
function visibleTexts() {
  const q = state.query.trim().toLowerCase();
  return state.texts.filter((t) => {
    if (state.deviceFilter && t.deviceId !== state.deviceFilter) return false;
    if (!q) return true;
    return t.content.toLowerCase().includes(q) ||
      (t.deviceName || '').toLowerCase().includes(q);
  });
}

function renderFilters() {
  const sel = $('deviceFilter');
  const cur = state.deviceFilter;
  sel.innerHTML = '<option value="">全部设备</option>' +
    state.devices.map((d) => `<option value="${esc(d.id)}">${esc(d.name)}</option>`).join('');
  sel.value = cur;
  // 选中的设备可能已经不在列表里了（比如它的文本都被删了）。赋一个不存在的
  // value 会让 select 自己变回空，这里顺手把状态也纠正过来，别让筛选条件
  // 和界面显示不一致。
  if (sel.value !== cur) {
    state.deviceFilter = '';
    sel.value = '';
  }
}

function renderTTL() {
  const { value, unit } = state.ttl;
  const code = state.codeTTL;
  const el = $('ttlHint');
  const label = QSSettings.ttlUnitText;
  const base = value ? `保留 ${value} ${label(unit)}` : '永久保留';
  // 验证码规则开着才提它，关着（0）时提了只会让人以为多了个没开的开关
  const codeOn = !!(code && Number(code.value) > 0);
  el.textContent = codeOn
    ? `${base}（验证码 ${code.value} ${label(code.unit)}）`
    : base;
  el.title = value
    ? '超过这个时长的文本会被自动删除。点这里可以在设置里调整'
    : '文本不会自动删除。点这里可以在设置里改成按时间自动清理';
}

/* ------------------------------------------------------------ 渲染 */

const COPY_ICON = `<svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="9" width="11" height="11" rx="2"/><path d="M5 15V5a2 2 0 012-2h10"/></svg>`;

// 本机提示。把 IP 一起显示出来：设备身份现在就是 IP，换地址或换设备时
// 能直接对照，不用猜"这台到底是不是我"。
//
// 还没发过文本时设备表里没有我，这时退回到 /api/config 回显的地址，
// 让"我是哪台"从打开页面起就可见。
function renderDeviceHint() {
  const me = state.devices.find((d) => d.isMe);
  if (me) {
    $('deviceHint').textContent = `本机：${me.name}（${me.id}）`;
    return;
  }
  const ip = state.config && state.config.clientIp;
  $('deviceHint').textContent = ip ? `本机：${ip}（还没发过文本）` : '';
}

function renderTexts() {
  const list = visibleTexts();
  $('textCount').textContent = state.texts.length;

  // 空状态要分清"一条都没有"和"被筛掉了"——后者说"还没有文本"会让用户
  // 以为自己的东西丢了。
  //
  // 用 `hidden` 而不是 `style.display`：样式表里有 `[hidden] { display: none !important }`，
  // 而 style.display 是行内样式——两者同时存在时 `!important` 会赢。HTML 里给
  // #textEmpty 加了 hidden（首帧不能报一个还不知道真假的结论），如果这里还写
  // `style.display = 'block'`，它就会被那条 !important 按死、永远显示不出来。
  const empty = $('textEmpty');
  empty.hidden = list.length > 0;
  if (!empty.hidden) {
    empty.textContent = state.texts.length ? '没有匹配的文本' : '还没有文本，发一条试试';
  }

  $('textList').innerHTML = list.map(renderItem).join('');

  // 正在编辑的那条，光标要落在编辑框里（否则点完"编辑"还得再点一下）
  if (state.editingId) {
    const el = $('edit-' + state.editingId);
    if (el) {
      el.focus();
      el.setSelectionRange(el.value.length, el.value.length);
    }
  }
}

// 这条文本"什么时候会被自动删掉"。expiresAt 是**服务端算好的绝对时刻**，
// 已经把「单条覆盖 > 验证码规则 > 全局设置」这条优先级算进去了——前端不重算，
// 否则规则会有两份，迟早不一致。
function ttlTextOf(t) {
  return Number(t.expiresAt) > 0 ? QSSettings.fmtLeft(t.expiresAt) : '永久保留';
}

function renderItem(t) {
  const id = esc(t.id);
  const meta = `<span class="titem-dev">${esc(t.deviceName)}</span>
        <span class="muted">${fmtTime(t.createdAt)}</span>
        ${t.updatedAt > t.createdAt ? '<span class="titem-tag">已编辑</span>' : ''}
        ${t.isCode ? `<span class="ttl-tag-code" title="整条是 4~8 位数字，或短文里出现「验证码」这类关键词并带数字">验证码</span>` : ''}`;

  if (state.editingId === t.id) {
    return `
    <div class="titem editing">
      <div class="titem-head">${meta}</div>
      <textarea class="titem-edit" id="edit-${id}" spellcheck="false">${esc(t.content)}</textarea>
      <div class="titem-ttl-row">
        <span class="ttl-lead">保留</span>
        ${QSSettings.ttlFieldsMarkup(t.ttlSeconds)}
        <span class="ttl-edit-note">${QSSettings.followNote('textTTL')}</span>
      </div>
      <div class="titem-actions">
        <span class="spacer"></span>
        <button class="btn btn-sm" data-cancel="${id}">取消</button>
        <button class="btn btn-sm btn-primary" data-save="${id}">保存</button>
      </div>
    </div>`;
  }

  // 多选模式下只留勾选框：这一行的点击语义变成了"选中/取消"，
  // 再摆着复制、编辑按钮会让人分不清点哪儿会发生什么。保留时长那枚 chip 同理——
  // 它在多选模式下点了只会变成"选中这条"，留着反而误导。
  if (state.selectMode) {
    const on = state.selected.has(t.id);
    return `
    <div class="titem${on ? ' picked' : ''}" data-id="${id}">
      <div class="titem-head">
        <input type="checkbox" class="titem-pick" data-pick="${id}"${on ? ' checked' : ''}
          aria-label="选择这条文本">
        ${meta}
      </div>
      <pre class="titem-body">${esc(t.content)}</pre>
    </div>`;
  }

  return `
    <div class="titem" data-id="${id}">
      <div class="titem-head">${meta}
        <span class="ttl-chip" data-ttl-edit="${id}" title="点这里改这条文本的保留时长">${ttlTextOf(t)}</span>
        <span class="spacer"></span>
        <!-- 三个按钮包一层：窄屏下 .titem-head 会换行，不包的话它们会被拆成
             "复制 / 编辑 / 删除"散在两行里（实测 390px 下就是这样）。包起来
             整组一起换行，看着才是有意为之。 -->
        <span class="titem-btns">
          <button class="btn btn-sm btn-icon" data-copy="${id}" title="复制内容">${COPY_ICON}</button>
          <button class="btn btn-sm" data-edit="${id}">编辑</button>
          <button class="btn btn-sm btn-danger" data-del="${id}">删除</button>
        </span>
      </div>
      <pre class="titem-body">${esc(t.content)}</pre>
    </div>`;
}

/* ------------------------------------------------------------ 多选 */

function setSelectMode(on) {
  state.selectMode = on;
  if (!on) state.selected.clear();
  // 编辑和多选不共存：编辑框里的内容会被整块重渲染冲掉，
  // 与其让它莫名其妙地消失，不如进多选时先退出编辑。
  else state.editingId = null;
  renderTexts();
  syncBulkBar();
}

function syncBulkBar() {
  $('bulkBar').hidden = !state.selectMode;
  $('selectBtn').classList.toggle('active', state.selectMode);
  $('selectBtn').textContent = state.selectMode ? '退出多选' : '多选';
  $('selCount').textContent = `已选 ${state.selected.size} 条`;
  $('bulkDelete').disabled = state.selected.size === 0;

  const list = visibleTexts();
  const all = list.length > 0 && list.every((t) => state.selected.has(t.id));
  $('selectAll').checked = all;
  $('selectAll').indeterminate = !all && list.some((t) => state.selected.has(t.id));
}

function toggleSelect(id) {
  if (state.selected.has(id)) state.selected.delete(id);
  else state.selected.add(id);
  renderTexts();
  syncBulkBar();
}

// 全选作用于**当前筛选结果**，不是全部文本：用户筛出"老王的手机"之后点全选，
// 期望的是把这几条都选上，而不是把别人发的也一起选走。
function toggleSelectAll(on) {
  for (const t of visibleTexts()) {
    if (on) state.selected.add(t.id);
    else state.selected.delete(t.id);
  }
  renderTexts();
  syncBulkBar();
}

async function bulkDelete() {
  const ids = [...state.selected];
  if (!ids.length) {
    toast('还没有选中任何文本', 'err');
    return;
  }
  if (!confirm(`确定删除选中的 ${ids.length} 条文本？删了找不回来。`)) return;

  const btn = $('bulkDelete');
  btn.disabled = true;
  try {
    const res = await api('POST', '/api/texts/delete', { ids });
    // 实际条数可能少于选中的——某几条已经被别的设备删掉了，
    // 那不是错误，如实报数即可
    toast(res.deleted < ids.length
      ? `已删除 ${res.deleted} 条（${ids.length - res.deleted} 条已被别处删掉）`
      : `已删除 ${res.deleted} 条`, 'ok');
    setSelectMode(false);
    await refreshAll();
  } catch (e) {
    toast(e.message, 'err');
  } finally {
    btn.disabled = false;
  }
}

/* ------------------------------------------------------------ 设备面板 */

function openDev(open) {
  $('devOverlay').hidden = !open;
  document.documentElement.classList.toggle('modal-open', open);
  if (open) renderDevices();
}

function renderDevices() {
  const box = $('devList');
  if (!state.devices.length) {
    box.innerHTML = '<div class="empty">还没有设备发过文本</div>';
    return;
  }
  box.innerHTML = state.devices.map((d) => `
    <div class="devitem" data-id="${esc(d.id)}">
      <div class="devitem-head">
        <span class="devitem-name">${esc(d.name)}</span>
        ${d.isMe ? '<span class="titem-tag">本机</span>' : ''}
        <span class="spacer"></span>
        <span class="muted">${fmtTime(d.lastSeen)}</span>
      </div>
      <div class="devitem-ip">${esc(d.id)}</div>
      <div class="devitem-ua" title="${esc(d.ua)}">${esc(d.ua || '（未记录 UA）')}</div>
      <div class="path-row">
        <input type="text" class="devitem-input" value="${esc(d.remark)}"
          maxlength="40" spellcheck="false" placeholder="给它起个名字，例如 老王的手机">
        <button class="btn btn-sm" data-remark="${esc(d.id)}">保存</button>
      </div>
      <div class="devitem-foot">
        <span class="devitem-count">${d.textCount > 0 ? `还有 ${d.textCount} 条文本` : '没有文本'}</span>
        <span class="spacer"></span>
        <button class="btn btn-sm btn-danger" data-del="${esc(d.id)}">删除设备</button>
      </div>
    </div>`).join('');
}

// 删掉一台设备的记录。**不删它的文本**——用户点的是"把这个设备条目去掉"，
// 不是"清空它的内容"，顺手删内容属于替他做了没要求的事，而且删了找不回来。
//
// 所以确认框里要说清两件事：文本会留着（只是显示名回落成「未知设备」），
// 以及删的是当前这台的话，下次发文本它还会回来。这两条不说，用户点完会困惑。
async function deleteDevice(id) {
  const d = state.devices.find((x) => x.id === id);
  if (!d) return;

  const lines = [`删除设备「${d.name}」的记录？`];
  if (d.textCount > 0) {
    lines.push('', `它还有 ${d.textCount} 条文本。文本不会被删掉，` +
      '只是文本列表里的发送方会显示成「未知设备」。');
  } else {
    lines.push('', '它已经没有文本了。');
  }
  if (d.isMe) {
    lines.push('', '这是你当前用的这台，下次发文本时它会重新出现在这里。');
  }
  if (!confirm(lines.join('\n'))) return;

  try {
    await api('DELETE', `/api/devices/${encodeURIComponent(id)}`);
    toast('设备记录已删除', 'ok');
    // 文本行里的发送方名字也变了，一起重拉
    await refreshAll();
    renderDevices();
  } catch (e) {
    toast(e.message, 'err');
  }
}

async function saveRemark(id) {
  const item = document.querySelector(`.devitem[data-id="${CSS.escape(id)}"]`);
  const input = item && item.querySelector('.devitem-input');
  if (!input) return;
  try {
    await api('PUT', `/api/devices/${encodeURIComponent(id)}`, { remark: input.value });
    toast('备注已保存', 'ok');
    // 文本列表里显示的就是这个名字，所以要一起重拉
    await refreshAll();
    renderDevices();
  } catch (e) {
    toast(e.message, 'err');
  }
}

/* ------------------------------------------------------------ 实时更新（SSE） */

// 长连接的建立与重连交给 live.js（首页也用同一份）。
// 本页只关心 texts 主题；files 主题跟这里无关，不做无谓的拉取。
let live = null;
let remoteTimer = null;

function connectEvents() {
  if (live) {
    live.restart(); // 口令变了，用新口令重连
    return;
  }
  live = QSLive.connect({
    token: () => state.token,
    onAuthFail: showAuth,
    onTopic: (topic) => {
      if (topic !== 'texts') return;
      // 防抖：别人连发几条时合并成一次拉取。
      // 这里不区分"是不是本机改的"——本页本机改动后自己就刷新了，
      // 400ms 后重拉一次拿到的还是同一份数据，没有副作用。
      if (remoteTimer) clearTimeout(remoteTimer);
      remoteTimer = setTimeout(() => {
        remoteTimer = null;
        refreshAll();
      }, 400);
    },
  });
}

/* ------------------------------------------------------------ 事件 */

function updateComposeHint() {
  const n = utf8Len($('textInput').value);
  $('composeHint').textContent = n ? `${fmtSize(n)} / ${fmtSize(TEXT_MAX)}` : '';
  $('composeHint').classList.toggle('over', n > TEXT_MAX);
}

async function send() {
  const el = $('textInput');
  const content = el.value;
  if (!content.trim()) {
    toast('先写点什么再发', 'err');
    return;
  }
  if (utf8Len(content) > TEXT_MAX) {
    toast(`内容超过 ${fmtSize(TEXT_MAX)}，发不出去`, 'err');
    return;
  }

  const btn = $('sendBtn');
  btn.disabled = true;
  try {
    await api('POST', '/api/texts', { content });
    el.value = '';
    updateComposeHint();
    toast('已发送', 'ok');
    await refreshAll();
  } catch (e) {
    toast(e.message, 'err');
  } finally {
    btn.disabled = false;
  }
}

async function saveEdit(id) {
  const el = $('edit-' + id);
  if (!el) return;
  const t = state.texts.find((x) => x.id === id);
  if (!t) return;
  const content = el.value;
  if (!content.trim()) {
    toast('内容不能为空，想删就点删除', 'err');
    return;
  }
  if (utf8Len(content) > TEXT_MAX) {
    toast(`内容超过 ${fmtSize(TEXT_MAX)}，存不下`, 'err');
    return;
  }

  // 保留时长跟内容一起提交（同一个保存按钮），但**只提交真的变了的字段**：
  //   ① `UpdateText` 会把 updated_at 刷新，只改保留时长却把内容一起发过去，
  //      列表上会平白多出一个「已编辑」；
  //   ② 服务端本来就允许只传一个字段。
  const row = el.closest('.titem').querySelector('.titem-ttl-row');
  const r = QSSettings.readTtlEditor(row);
  if (!r.ok) {
    toast(r.msg, 'err');
    return;
  }

  const patch = {};
  if (content !== t.content) patch.content = content;
  if (r.secs !== Number(t.ttlSeconds || 0)) patch.ttlSeconds = r.secs;
  if (!Object.keys(patch).length) {
    // 什么都没改，就别发请求、也别刷新列表——刷新会让用户以为自己动了什么
    state.editingId = null;
    renderTexts();
    return;
  }

  try {
    await api('PUT', `/api/texts/${encodeURIComponent(id)}`, patch);
    state.editingId = null;
    toast('已保存', 'ok');
    await refreshAll();
  } catch (e) {
    toast(e.message, 'err');
  }
}

function bind() {
  // 主题按钮、设置面板、以及"跟随系统时同步图标"都在 settings.js 里绑好了。

  // 列表头那句「保留 N 天 / 永久保留」原来指向 /#settings，点了会跳到首页。
  // 现在本页就能开面板（面板是两页共用的），所以拦下这次跳转——
  // href 保留着，万一 JS 没跑起来，点下去至少还能到首页设置。
  $('ttlHint').addEventListener('click', (e) => {
    e.preventDefault();
    QSSettings.open(true);
  });

  $('refreshBtn').addEventListener('click', refreshAll);
  $('sendBtn').addEventListener('click', send);
  $('textInput').addEventListener('input', updateComposeHint);
  // Ctrl / Cmd + Enter 直接发，贴完长文本不用再去够按钮
  $('textInput').addEventListener('keydown', (e) => {
    if ((e.ctrlKey || e.metaKey) && e.key === 'Enter') {
      e.preventDefault();
      send();
    }
  });

  // 设备面板
  $('devicesBtn').addEventListener('click', () => openDev(true));
  $('devClose').addEventListener('click', () => openDev(false));
  $('devOverlay').addEventListener('click', (e) => {
    if (e.target === $('devOverlay')) openDev(false);
  });
  $('devList').addEventListener('click', (e) => {
    // 删除要排在前面：它是破坏性操作，别被"保存备注"那条分支抢先
    const del = e.target.closest('[data-del]');
    if (del) { deleteDevice(del.getAttribute('data-del')); return; }
    const btn = e.target.closest('[data-remark]');
    if (btn) saveRemark(btn.getAttribute('data-remark'));
  });
  $('devList').addEventListener('keydown', (e) => {
    if (e.key !== 'Enter') return;
    // 只管输入框里的回车。不判这一下的话，焦点落在「删除设备」上按回车会
    // 既触发按钮自己的 click（删设备）又走到这里保存备注——一次按键两件事。
    if (!e.target.classList.contains('devitem-input')) return;
    const btn = e.target.closest('.devitem').querySelector('[data-remark]');
    if (btn) saveRemark(btn.getAttribute('data-remark'));
  });

  document.addEventListener('keydown', (e) => {
    if (e.key !== 'Escape') return;
    // 设置面板开着时，Escape 归它管（settings.js 里绑了）。不先让开的话，
    // 一次按键会同时关掉面板**和**退出多选，用户只按了一下却丢了两层状态。
    if (QSSettings.isOpen()) return;
    if (!$('devOverlay').hidden) { openDev(false); return; }
    if (state.editingId) { state.editingId = null; renderTexts(); return; }
    if (state.selectMode) setSelectMode(false);
  });

  // 文本列表上的操作（列表是动态渲染的，用委托）
  $('textList').addEventListener('click', async (e) => {
    // ---- 多选模式：点哪儿都是"切换选中"，不触发复制
    if (state.selectMode) {
      const row = e.target.closest('.titem');
      if (row && row.dataset.id) toggleSelect(row.dataset.id);
      return;
    }

    const copyBtn = e.target.closest('[data-copy]');
    if (copyBtn) {
      const t = state.texts.find((x) => x.id === copyBtn.getAttribute('data-copy'));
      if (t) {
        const ok = await copyText(t.content);
        toast(ok ? '内容已复制' : '复制失败，请手动选中复制', ok ? 'ok' : 'err');
      }
      return;
    }

    const editBtn = e.target.closest('[data-edit]');
    if (editBtn) {
      state.editingId = editBtn.getAttribute('data-edit');
      renderTexts();
      return;
    }

    // 点「还剩 X 天」那枚 chip = 进编辑（保留时长就在编辑框里），
    // 不用先找到「编辑」按钮再理解"改保留时长也要点编辑"。
    const ttlChip = e.target.closest('[data-ttl-edit]');
    if (ttlChip) {
      state.editingId = ttlChip.getAttribute('data-ttl-edit');
      renderTexts();
      const num = document.querySelector('.titem.editing .ttl-num');
      if (num) { num.focus(); num.select(); }
      return;
    }

    const cancelBtn = e.target.closest('[data-cancel]');
    if (cancelBtn) {
      state.editingId = null;
      renderTexts();
      return;
    }

    const saveBtn = e.target.closest('[data-save]');
    if (saveBtn) {
      await saveEdit(saveBtn.getAttribute('data-save'));
      return;
    }

    const delBtn = e.target.closest('[data-del]');
    if (delBtn) {
      const id = delBtn.getAttribute('data-del');
      if (!confirm('确定删除这条文本？删了找不回来。')) return;
      try {
        await api('DELETE', `/api/texts/${encodeURIComponent(id)}`);
        if (state.editingId === id) state.editingId = null;
        toast('已删除', 'ok');
        await refreshAll();
      } catch (err) {
        toast(err.message, 'err');
      }
      return;
    }

    // ---- 点整条卡片即复制
    //
    // 放在按钮判断之后，所以点按钮不会被这里截走。
    // 有选中文字时不触发：用户想手动选一段（比如只取链接的一半）时，
    // 点下去把整条复制走、还顺手清掉他的选择，很恼人。
    const row = e.target.closest('.titem');
    if (!row || !row.dataset.id) return;
    const sel = document.getSelection();
    if (sel && sel.toString().length > 0) return;

    const t = state.texts.find((x) => x.id === row.dataset.id);
    if (!t) return;
    const ok = await copyText(t.content);
    toast(ok ? '内容已复制' : '复制失败，请手动选中复制', ok ? 'ok' : 'err');
  });

  // ---- 搜索与筛选
  $('searchInput').addEventListener('input', () => {
    state.query = $('searchInput').value;
    renderTexts();
    syncBulkBar();
  });
  $('deviceFilter').addEventListener('change', () => {
    state.deviceFilter = $('deviceFilter').value;
    renderTexts();
    syncBulkBar();
  });

  // ---- 多选
  $('selectBtn').addEventListener('click', () => setSelectMode(!state.selectMode));
  $('bulkCancel').addEventListener('click', () => setSelectMode(false));
  $('bulkDelete').addEventListener('click', bulkDelete);
  $('selectAll').addEventListener('change', () => toggleSelectAll($('selectAll').checked));

  $('authBtn').addEventListener('click', async () => {
    state.token = $('authInput').value.trim();
    localStorage.setItem('qs_token', state.token);
    await refreshAll();
    if ($('authCard').hidden) {
      toast('已进入', 'ok');
      connectEvents(); // 拿到口令后才连得上（没口令时是 401）
    }
  });
  $('authInput').addEventListener('keydown', (e) => {
    if (e.key === 'Enter') $('authBtn').click();
  });
}

// 设置面板（标记 + 事件 + 外观逻辑）来自 settings.js，两页共用同一份。
// 这里只注入文本页相关的几个动作：口令从 state 读；撞上 401 要露出登录卡；
// 换了存储目录之后文本列表要重拉。
QSSettings.init({
  // 只显示文本页那几块：文本保留时间 / 验证码过期时间 / 设备记录。
  // 文件保留时间、文件存储位置、以及「上传区/文件列表」两个不透明度滑块
  // 在这一页会被藏掉（用户要的是"文本页只能看见文本页的设置"）。
  page: 'text',
  getToken: () => state.token,
  onUnauthorized: showAuth,
  onDataDirChanged: refreshAll,
  // 保留时长一改，列表头那句提示要立刻跟着变——它就在这一页上，
  // 等下次刷新才变会很怪。
  //
  // **不带 textTTL 的调用必须直接忽略**：页面加载时下面那次
  // `QSSettings.apply({...})` 是**只带外观**的，而 `apply()` 最后一行同样会回调到
  // 这里。拿它去覆盖 state.ttl，就会在首帧把提示写成"永久保留"——真实的保留时长
  // 要等 `init()` 的 `/api/settings` 回来才知道，中间那段显示的是**错信息**。
  // 实测（`.tmp/shots/probe-ttl-hint-flash.mjs` 录下每次文案变化）：
  //   +22ms "永久保留"  →  +26ms "保留 2 小时"
  // 本机只错 4ms 看不出来，NAS 上或弱网下就是肉眼可见的"明明开了自动清理、
  // 却写着永久保留"。忽略之后提示保持空着（HTML 里本来就是空的）——
  // 空 = 还不知道，比写错强。
  onApplied: (s) => {
    if (!s.textTTL) return;
    state.ttl = s.textTTL;
    if (s.codeTTL) state.codeTTL = s.codeTTL;
    renderTTL();
  },
});

// 初始外观：主题沿用服务端注入到 <html data-theme> 的值，避免首帧闪色。
// 服务端设置拉回来后，init() 里的 QSSettings.apply 会覆盖它。
//
// **这里刻意不写 textTTL**：这是"只带外观"的一次调用，写了就会被 onApplied
// 当成真实设置、把提示打成"永久保留"（见上面那段注释）。`apply()` 内部对缺字段
// 本来就有默认值，不写不会出错。
QSSettings.apply({
  theme: document.documentElement.getAttribute('data-theme') || '',
  background: null,
  bgBlur: 0,
  opacity: { topbar: 85, upload: 85, files: 85 },
  pruneDevices: false,
});

bind();
init();
