'use strict';

/* QuickShare 首页。
 *
 * 外观设置（主题 / 背景图 / 模糊度 / 不透明度）、文本保留时间、文件存储位置，
 * 以及那个设置面板本身，都在 settings.js 里——两页共用同一套。这里只留
 * 首页自己的东西：上传、文件列表、链接与二维码。
 */

const state = {
  config: null,
  files: [],
  token: localStorage.getItem('qs_token') || '',

  // 文件列表的搜索与排序**全在前端做**（理由见下面 visibleFiles 的注释）。
  // query 是"这一次的临时筛选"，sort 是"用户偏好"（存 localStorage）。
  // sort 的真实初值在 bind() 里从 <select> 读——那里才知道用户上次选的是什么。
  query: '',
  sort: 'time-desc',

  // 正在"就地改保留时长"的那个文件的 id（同时只有一个）。列表整块重渲染，
  // 所以用 id 而不是保存 DOM 引用——重渲染之后引用就失效了。
  ttlEditId: null,
};

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

/* ------------------------------------------------------------ 初始化 */

function showAuth() {
  $('authCard').hidden = false;
  $('uploadCard').hidden = true;
  $('authInput').focus();
}

async function init() {
  // config 和 settings **并行发**：两者互不依赖（一个给分片大小 / 要不要口令，
  // 一个管外观），串着等会白白多出一个往返。切页后列表迟迟不出来，有一部分就是这个。
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

  // 外观设置要在鉴权判断之前套上，否则开口令的部署每次刷新都闪一下默认配色。
  // 拉不到也不影响主流程，退回默认外观继续。
  if (settings) QSSettings.apply(settings);
  else QSSettings.syncThemeUI();

  $('chunkHint').textContent = `分片 ${fmtSize(state.config.chunkSize)}`;

  if (state.config.needAuth && !state.token) {
    showAuth();
    return;
  }
  await refreshAll();
  connectEvents();

  // /#settings 直接进面板、以及"已经在本页时改 hash 也要开"这两件事，
  // 都交给 QSSettings.init() —— 它给两页绑了同一套 hashchange 监听。
}

async function refreshAll() {
  try {
    const [stats, files] = await Promise.all([
      api('GET', '/api/stats'),
      api('GET', '/api/files'),
    ]);
    state.files = files || [];
    $('stats').innerHTML =
      `文件 <b>${stats.files}</b> · 占用 <b>${fmtSize(stats.totalSize)}</b>`;
    $('authCard').hidden = true;
    $('uploadCard').hidden = false;
    renderFiles();
    return true;
  } catch (e) {
    toast(e.message, 'err');
    return false;
  }
}

/* ------------------------------------------------------------ 渲染 */

const FILE_ICON = `<svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M14 3v5h5M14 3H7a2 2 0 00-2 2v14a2 2 0 002 2h10a2 2 0 002-2V8l-5-5z"/></svg>`;

const LINK_ICON = `<svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M10 13a5 5 0 0 0 7.54.54l3-3a5 5 0 0 0-7.07-7.07l-1.72 1.71"/><path d="M14 11a5 5 0 0 0-7.54-.54l-3 3a5 5 0 0 0 7.07 7.07l1.71-1.71"/></svg>`;

// 文件的可分享地址。
//
// 刻意用不带文件名的短链 /f/{id}：服务端是按 ID 定位文件、文件名从库里取的，
// 末尾那段文件名只影响链接好不好看，不参与定位。而二维码每多一个字节版本就往上走、
// 模块变密，贴到屏幕上反而更难扫——中文文件名按 %XX 编码后要膨胀三倍。
// 短链和长链的下载行为完全一样。
function shareURL(f) {
  return new URL('/f/' + encodeURIComponent(f.id), location.origin).href;
}

function pageURL() {
  return location.origin + '/';
}

/* ------------------------------------------------------------ 搜索与排序 */

// 搜索与排序**全在前端做**：`/api/files` 本来就是一次给全量，列表已经在内存里了，
// 本地过滤零延迟，也不用每敲一个字就发一次请求。跟文本页是同一套做法——所以
// `ListFiles()` 刻意不加查询参数，真到了列表大到必须分页的规模再说。
//
// 每个比较器都从**原始顺序**出发（`state.files` 是服务端给的"新→旧"，同秒的按
// rowid 倒序兜底），而不是在上一次排序的结果上再排：`Array.prototype.sort` 在
// 现代引擎里是稳定的，于是比较器没覆盖到的那一段自然保留服务端的顺序，不会出现
// 每次刷新顺序都在变的抖动。
//
// 键必须和 index.html 里 <select id="fileSort"> 的 option value 一一对应。
const SORTS = {
  'time-desc': { cmp: (a, b) => b.createdAt - a.createdAt },
  // 升序先整体倒过来再稳定排序。服务端同秒的行是按 rowid 倒序给的，倒过来之后
  // 同秒的变成正序——这才是"旧→新"该有的样子。不这么做的话，同秒那几条在两个
  // 方向下顺序一模一样，看着像排序没生效。（一次拖 5 个文件上传，它们几乎必然
  // 落在同一秒里，这不是理论问题。）
  'time-asc': { cmp: (a, b) => a.createdAt - b.createdAt, rev: true },
  'name-asc': { cmp: (a, b) => NAME_COLLATOR.compare(a.name, b.name) },
  'name-desc': { cmp: (a, b) => NAME_COLLATOR.compare(b.name, a.name) },
  'size-desc': { cmp: (a, b) => b.size - a.size },
  'size-asc': { cmp: (a, b) => a.size - b.size },
};

// 按中文习惯排（拼音序）。numeric 让「第2章」排在「第10章」前面——纯字符串比较
// 会把 10 排到 2 前面；sensitivity:'base' 让大小写不敏感，跟搜索框的语义一致。
const NAME_COLLATOR = new Intl.Collator('zh', { numeric: true, sensitivity: 'base' });

// 当前搜索 / 排序条件下要显示的文件。
function visibleFiles() {
  const q = state.query.trim().toLowerCase();
  const list = state.files.filter((f) => !q || f.name.toLowerCase().includes(q));
  const s = SORTS[state.sort] || SORTS['time-desc'];
  return s.rev ? list.reverse().sort(s.cmp) : list.sort(s.cmp);
}

function renderFiles() {
  const tbody = $('fileRows');
  const list = visibleFiles();

  // 计数：没筛过就是总数；筛过之后写成「命中 / 总数」——只报总数会让人以为屏幕上
  // 这几行就是全部。（文本页那边只报总数，是因为它不做排序，这里信息量不一样。）
  $('fileCount').textContent = list.length === state.files.length
    ? String(state.files.length)
    : `${list.length} / ${state.files.length}`;

  // 空状态要分清"一个文件都没有"和"被筛掉了"——后者写成"还没有文件"会让用户以为
  // 自己的东西丢了。（同 text.js 的 #textEmpty。）
  //
  // 用 hidden 而不是 style.display：HTML 里 #fileEmpty 默认 hidden（首帧不能报
  // 一个还不知道真假的结论），而样式表里的 `[hidden] { display: none !important }`
  // 会压过行内样式——两边混用的话它永远显示不出来。
  const empty = $('fileEmpty');
  empty.hidden = list.length > 0;
  if (!empty.hidden) {
    empty.textContent = state.files.length ? '没有匹配的文件' : '还没有文件，先上传一个吧';
  }

  tbody.innerHTML = list.map((f) => {
    const url = esc(f.url);
    const share = esc(shareURL(f));
    return `
    <tr>
      <td>
        <div class="fname">${FILE_ICON}<a href="${url}" target="_blank" rel="noopener" title="${esc(f.name)}">${esc(f.name)}</a></div>
        ${ttlCell(f)}
      </td>
      <td class="col-hide-sm num">${fmtSize(f.size)}</td>
      <td class="col-hide-sm muted">${fmtTime(f.createdAt)}</td>
      <td>
        <div class="actions">
          <a class="btn btn-sm" href="${url}?dl=1" download>下载</a>
          <button class="btn btn-sm btn-icon" data-copy="${share}" data-qr="${share}" title="复制链接 / 悬停出二维码">${LINK_ICON}</button>
          <button class="btn btn-sm btn-danger" data-del="${esc(f.id)}">删除</button>
        </div>
      </td>
    </tr>`;
  }).join('');
}

// 文件名下面那行「还剩 X 天 / 永久保留」，点一下就地改这个文件的保留时长。
//
// 放在文件名格里而不是新加一列：新列在 720px 以下要么被 .col-hide-sm 藏掉
// （那就再也改不了了）、要么把操作列挤出去。挂在这里窄屏也点得到。
//
// expiresAt 是**服务端算好的绝对时刻**，已经把「单条覆盖 > 全局设置」这条优先级
// 算进去了，所以 0 就真的是"不会自动删"，不用前端再判断一次。
function ttlCell(f) {
  if (state.ttlEditId === f.id) {
    return `<div class="fttl">${QSSettings.ttlEditorMarkup(
      f.ttlSeconds, QSSettings.followNote('fileTTL'))}</div>`;
  }
  const text = Number(f.expiresAt) > 0
    ? QSSettings.fmtLeft(f.expiresAt)
    : '永久保留';
  const title = Number(f.expiresAt) > 0
    ? '点这里改这个文件的保留时长'
    : '不会自动删除。点这里给这个文件单独设一个保留时长';
  return `<div class="fttl"><span class="ttl-chip" data-ttl="${esc(f.id)}" title="${title}">${text}</span></div>`;
}

// 提交单条文件的保留时长。
//
// 值没变就不发请求：服务端改一次就会 notify 一次，别的设备上的列表会跟着抖一下，
// 而用户其实什么都没改。
async function saveFileTTL(box) {
  const id = state.ttlEditId;
  const f = state.files.find((x) => x.id === id);
  if (!f || !box) return;

  const r = QSSettings.readTtlEditor(box);
  if (!r.ok) {
    toast(r.msg, 'err');
    return;
  }
  if (r.secs === Number(f.ttlSeconds || 0)) {
    state.ttlEditId = null;
    renderFiles();
    return;
  }

  try {
    await api('PUT', `/api/files/${encodeURIComponent(id)}`, { ttlSeconds: r.secs });
    state.ttlEditId = null;
    toast('保留时长已更新', 'ok');
    await refreshAll();
  } catch (e) {
    toast(e.message, 'err');
  }
}

/* ------------------------------------------------------------ 复制与二维码 */

// 复制文本到剪贴板。
//
// navigator.clipboard 只在"安全上下文"里可用（HTTPS 或 localhost）。
// 内网部署基本都是 http://192.168.x.x:8080，那里它压根不存在——
// 所以必须留一条 execCommand 的降级路径，否则这功能在真实环境里直接失效，
// 而且是在开发机上测不出来、一上 NAS 就坏的那种失效。
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

  // 别把用户原本选中的东西弄丢
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

let qrHideTimer = 0;

// 在 anchor 旁边弹出二维码
function showQR(anchor, text) {
  clearTimeout(qrHideTimer);

  const pop = $('qrPop');
  const img = $('qrImg');

  // 内容没变就别重设 src，否则鼠标在按钮上晃会反复发请求
  const src = '/api/qr?d=' + encodeURIComponent(text);
  if (img.getAttribute('src') !== src) img.setAttribute('src', src);

  pop.hidden = false;

  // 得先显示出来才量得到尺寸
  const r = anchor.getBoundingClientRect();
  const p = pop.getBoundingClientRect();
  const gap = 10;

  let left = r.left + r.width / 2 - p.width / 2;
  left = Math.max(8, Math.min(left, window.innerWidth - p.width - 8));

  let top = r.bottom + gap;
  if (top + p.height > window.innerHeight - 8) top = r.top - p.height - gap; // 下面放不下就翻到上面

  pop.style.left = Math.round(left) + 'px';
  pop.style.top = Math.round(Math.max(8, top)) + 'px';
}

function hideQR() {
  clearTimeout(qrHideTimer);
  // 留一点延迟，鼠标快速划过一排按钮时才不会疯狂闪
  qrHideTimer = setTimeout(() => { $('qrPop').hidden = true; }, 110);
}

// 立刻收起（页面滚动/尺寸变化时用，那会儿位置已经不对了）
function collapseQR() {
  clearTimeout(qrHideTimer);
  $('qrPop').hidden = true;
}

/* ------------------------------------------------------------ 上传 */

const queue = [];

function addToQueue(file) {
  const el = document.createElement('div');
  el.className = 'qitem';
  el.innerHTML = `
    <div class="qitem-top">
      <span class="qitem-name">${esc(file.name)}</span>
      <span class="qitem-meta">等待中</span>
    </div>
    <div class="bar"><i></i></div>`;
  $('queue').appendChild(el);

  const item = { file, el, meta: el.querySelector('.qitem-meta'), bar: el.querySelector('.bar > i') };
  queue.push(item);
  runQueue();
}

let running = 0;

function runQueue() {
  while (running < 2) {
    const item = queue.find((i) => !i.started);
    if (!item) return;
    item.started = true;
    running++;
    uploadFile(item).finally(() => { running--; runQueue(); });
  }
}

async function uploadFile(item) {
  const { file, el, meta, bar } = item;

  if (state.config.maxFileSize > 0 && file.size > state.config.maxFileSize) {
    el.classList.add('err');
    meta.textContent = `超过单文件上限 ${fmtSize(state.config.maxFileSize)}`;
    return;
  }

  const t0 = Date.now();
  let lastTick = 0;

  try {
    const init = await api('POST', '/api/upload/init', {
      name: file.name, size: file.size, mime: file.type || '',
    });

    const { uploadId, chunkSize, totalChunks } = init;
    const received = new Set(init.received || []);
    const pending = [];
    for (let i = 0; i < totalChunks; i++) {
      if (!received.has(i)) pending.push(i);
    }

    let done = received.size;
    const report = (force) => {
      const now = Date.now();
      if (!force && now - lastTick < 120) return;
      lastTick = now;
      const sent = Math.min(done * chunkSize, file.size);
      const pct = file.size ? (sent / file.size) * 100 : 100;
      bar.style.width = pct.toFixed(1) + '%';
      const secs = (now - t0) / 1000;
      const speed = secs > 0.4 ? sent / secs : 0;
      meta.textContent = `${pct.toFixed(0)}% · ${fmtSize(sent)} / ${fmtSize(file.size)}` +
        (speed ? ` · ${fmtSize(speed)}/s` : '');
    };
    report(true);

    if (init.resumed && received.size > 0) {
      toast(`继续上传 ${file.name}（已完成 ${received.size}/${totalChunks} 片）`);
    }

    const CONCURRENCY = 3;
    const workers = Array.from({ length: Math.min(CONCURRENCY, Math.max(pending.length, 1)) }, async () => {
      while (pending.length) {
        const idx = pending.shift();
        const start = idx * chunkSize;
        const blob = file.slice(start, Math.min(start + chunkSize, file.size));
        await putChunk(uploadId, idx, blob);
        done++;
        report(false);
      }
    });
    await Promise.all(workers);

    const result = await api('POST', `/api/upload/${uploadId}/complete`);
    bar.style.width = '100%';
    el.classList.add('done');
    const secs = (Date.now() - t0) / 1000;
    meta.textContent = `完成 · ${fmtSize(result.size)} · 用时 ${secs.toFixed(1)}s`;
    setTimeout(() => {
      el.remove();
      const i = queue.indexOf(item);
      if (i >= 0) queue.splice(i, 1);
    }, 4000);

    // 本机传的：紧接着服务端会推一条通知过来，标记一下让它静默处理
    markLocalChange();
    await refreshAll();
  } catch (e) {
    el.classList.add('err');
    meta.textContent = e.message || '上传失败';
  }
}

async function putChunk(uploadId, idx, blob) {
  const headers = {};
  if (state.token) headers['X-Admin-Token'] = state.token;
  const res = await fetch(`/api/upload/${uploadId}/${idx}`, { method: 'PUT', headers, body: blob });
  if (!res.ok) {
    let msg = `分片 ${idx} 上传失败`;
    try { msg = (await res.json()).error || msg; } catch (_) { /* 忽略 */ }
    throw new Error(msg);
  }
}

/* ------------------------------------------------------------ 实时更新（SSE） */

// 上一次"本机自己改动了列表"的时间戳。
//
// 本机上传 / 删除时，服务端一样会推一条通知过来。如果一视同仁地弹提示，
// 用户自己传完一个文件就会看到一条莫名其妙的"文件列表已更新"。
// 所以本机改动前先记个时间戳，这个窗口内到达的通知只刷新、不提示。
let lastLocalChange = 0;
const LOCAL_CHANGE_WINDOW = 3000;

function markLocalChange() {
  lastLocalChange = Date.now();
}

// 长连接的建立与重连交给 live.js（文本页也用同一份）。
// 这里只负责"收到 files 主题后做什么"。
let live = null;

function connectEvents() {
  if (live) {
    live.restart(); // 口令变了，用新口令重连
    return;
  }
  live = QSLive.connect({
    token: () => state.token,
    onAuthFail: showAuth,
    onTopic: (topic) => {
      // 文本页的变更与本页无关，不做无谓的拉取
      if (topic === 'files') onRemoteChange();
    },
  });
}

// 远端改动 -> 刷新列表。
//
// 两个节流：400ms 防抖把"一次传多个文件"的连击合并成一次拉取；
// 提示另有 4 秒冷却，否则别人连传 5 个文件会连弹 5 条一样的提示。
let remoteTimer = null;
let lastToastAt = 0;

function onRemoteChange() {
  const self = Date.now() - lastLocalChange < LOCAL_CHANGE_WINDOW;
  if (remoteTimer) clearTimeout(remoteTimer);
  remoteTimer = setTimeout(async () => {
    remoteTimer = null;
    const ok = await refreshAll();
    if (ok && !self && Date.now() - lastToastAt > 4000) {
      lastToastAt = Date.now();
      toast('文件列表已更新', 'ok');
    }
  }, 400);
}

/* ------------------------------------------------------------ 事件 */

function bind() {
  const dz = $('dropzone');
  const input = $('fileInput');

  dz.addEventListener('click', () => input.click());
  input.addEventListener('change', () => {
    for (const f of input.files) addToQueue(f);
    input.value = '';
  });

  ['dragenter', 'dragover'].forEach((ev) =>
    dz.addEventListener(ev, (e) => { e.preventDefault(); dz.classList.add('over'); }));
  ['dragleave', 'drop'].forEach((ev) =>
    dz.addEventListener(ev, (e) => { e.preventDefault(); dz.classList.remove('over'); }));

  dz.addEventListener('drop', (e) => {
    const files = e.dataTransfer && e.dataTransfer.files;
    if (!files) return;
    for (const f of files) addToQueue(f);
  });

  // 整页拖拽也接收
  window.addEventListener('dragover', (e) => e.preventDefault());
  window.addEventListener('drop', (e) => {
    if (e.target.closest && e.target.closest('#dropzone')) return;
    e.preventDefault();
    if (e.dataTransfer && e.dataTransfer.files.length) {
      for (const f of e.dataTransfer.files) addToQueue(f);
    }
  });

  $('refreshBtn').addEventListener('click', refreshAll);

  // ---- 搜索与排序
  //
  // 排序方式是**用户偏好**（下次打开还该是这个顺序），搜索词是**这一次的临时筛选**，
  // 所以只把排序存进 localStorage。
  const sortSel = $('fileSort');
  const savedSort = localStorage.getItem('qs_file_sort');
  // 存的值可能来自旧版本、也可能被人手改过。**必须校验**：给 select 赋一个不存在的
  // value 会让它自己变回空串，之后 state.sort 就是空的、排序静默失效（而不是报错）。
  if (savedSort && SORTS[savedSort]) sortSel.value = savedSort;
  state.sort = sortSel.value;

  sortSel.addEventListener('change', () => {
    state.sort = sortSel.value;
    localStorage.setItem('qs_file_sort', state.sort);
    renderFiles();
  });

  $('fileSearch').addEventListener('input', () => {
    state.query = $('fileSearch').value;
    renderFiles();
  });

  // ---- 链接复制与二维码

  // 顶栏按钮的二维码内容就是本页地址，得运行时才知道（每台 NAS 的 IP 都不一样）
  $('pageLinkBtn').setAttribute('data-qr', pageURL());
  $('pageLinkBtn').addEventListener('click', async () => {
    const ok = await copyText(pageURL());
    toast(ok ? '本页网址已复制' : '复制失败，请手动复制', ok ? 'ok' : 'err');
  });

  // 悬停出二维码。委托到 document：列表行是动态渲染的，顶栏那个按钮
  // 和列表里的按钮共用同一套逻辑，不用分别绑。
  document.addEventListener('mouseover', (e) => {
    const el = e.target.closest && e.target.closest('[data-qr]');
    if (el) showQR(el, el.getAttribute('data-qr'));
  });
  document.addEventListener('mouseout', (e) => {
    const el = e.target.closest && e.target.closest('[data-qr]');
    if (el) hideQR();
  });

  // 页面一动，浮层的位置就不对了，直接收起来
  window.addEventListener('scroll', collapseQR, true);
  window.addEventListener('resize', collapseQR);

  // ---- 外观 / 设置面板
  //
  // 面板自己的事件（主题分段、背景图、模糊度、三个不透明度、存储位置、
  // 保留时间、开关与 Escape）全在 settings.js 的 bind() 里绑好了。
  // 这里一个都不绑——绑两遍的话，一次点击会落两次库。

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

  $('fileRows').addEventListener('click', async (e) => {
    // 就地改保留时长。放在最前面：这几样都不是按钮，后面的
    // `closest('button[data-del]')` 也不会误匹配，但顺序清楚点更好读。
    const chip = e.target.closest('[data-ttl]');
    if (chip) {
      state.ttlEditId = chip.getAttribute('data-ttl');
      renderFiles();
      const num = document.querySelector('.ttl-edit .ttl-num');
      if (num) { num.focus(); num.select(); }
      return;
    }
    if (e.target.closest('[data-ttl-save]')) {
      await saveFileTTL(e.target.closest('.ttl-edit'));
      return;
    }
    if (e.target.closest('[data-ttl-cancel]')) {
      state.ttlEditId = null;
      renderFiles();
      return;
    }

    // 复制下载链接
    const copyBtn = e.target.closest('[data-copy]');
    if (copyBtn) {
      const ok = await copyText(copyBtn.getAttribute('data-copy'));
      toast(ok ? '下载链接已复制' : '复制失败，请手动复制', ok ? 'ok' : 'err');
      return;
    }

    // 删除
    const btn = e.target.closest('button[data-del]');
    if (!btn) return;

    const id = btn.getAttribute('data-del');
    const f = state.files.find((x) => x.id === id);
    if (!f) return;
    if (!confirm(`确定删除「${f.name}」？`)) return;

    try {
      await api('DELETE', `/api/files/${id}`);
      toast('已删除', 'ok');
      markLocalChange();
      await refreshAll();
    } catch (err) {
      toast(err.message, 'err');
    }
  });

  // 就地改保留时长时的键盘操作：回车提交、Esc 放弃。
  //
  // Esc 要 stopPropagation：设置面板那层在 document 上监听 Esc（见 settings.js），
  // 不拦住的话，用户按 Esc 想撤掉这个输入框，会顺手把设置面板一起关掉。
  $('fileRows').addEventListener('keydown', (e) => {
    if (!e.target.classList || !e.target.classList.contains('ttl-num')) return;
    if (e.key === 'Enter') {
      e.preventDefault();
      saveFileTTL(e.target.closest('.ttl-edit'));
    } else if (e.key === 'Escape') {
      e.stopPropagation();
      state.ttlEditId = null;
      renderFiles();
    }
  });
}

// 设置面板（标记 + 事件 + 外观逻辑）都来自 settings.js。这里把首页相关的
// 几个动作注进去：口令从 state 读；上传背景图撞上 401 要露出登录卡；
// 换了存储目录之后要把文件列表重新拉一遍。
QSSettings.init({
  // 面板里带 data-page 的块按页显示：这一页看得到「文件保留时间」「文件存储位置」
  // 和「上传区 / 文件列表」两个不透明度滑块；文本页那几块会被藏掉。
  page: 'files',
  getToken: () => state.token,
  onUnauthorized: showAuth,
  onDataDirChanged: async () => {
    markLocalChange();
    await refreshAll();
  },
});

// 先用默认外观摆好确定的初始态：无背景、滑块禁用。
// 主题沿用服务端注入到 <html data-theme> 的值，避免首帧闪色。
// 服务端设置拉回来后，init() 里的 QSSettings.apply 会覆盖它。
//
// **刻意不写 textTTL**：这一页目前没注册 onApplied，写了也没人读；但它和
// `text.js` 里那次初始 apply 是同一个东西，而那边一旦带上 textTTL 就会被
// onApplied 当成真实设置、把提示打成"永久保留"（详见 text.js 的注释）。
// 两处保持同样的形状，省得将来给这一页加 onApplied 时踩同一个坑。
QSSettings.apply({
  theme: document.documentElement.getAttribute('data-theme') || '',
  background: null,
  bgBlur: 0,
  opacity: { topbar: 85, upload: 85, files: 85 },
  pruneDevices: false,
});

bind();
init();
