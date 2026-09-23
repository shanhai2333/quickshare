'use strict';

/* QuickShare 设置面板 —— 首页和文本页共用。
 *
 * 为什么抽成一个文件：面板原来只长在首页（app.js 里），文本页只放了一句
 * 「去首页改」的提示，点一下跳走。而「文本保留时间」这项恰恰是**关于文本页**
 * 的——让用户为了改它离开当前页，再自己找回来，很别扭。
 *
 * 抽出来之后两页加载同一份标记 + 同一份逻辑：以后加设置项两页同时生效，
 * 不会出现"首页改了、文本页还是旧样子"。也顺带消掉了原来两页各写一遍
 * 主题逻辑的那份重复——那两份已经开始分叉了（文本页在深色模式下仍会
 * 显示背景图，首页不会）。
 *
 * 加载顺序：本文件放在页面自己的脚本**之前**，因为 app.js / text.js 在
 * 文件末尾就会用到 QSSettings。它按名字引用页面里的 $ / esc / fmtSize /
 * toast / api —— 那些是页面脚本定义的全局，在**函数被调用时**才解析，
 * 那时页面脚本已经执行过了，所以没问题。
 *
 * 页面要调 QSSettings.init({...}) 注入几个页面相关的钩子（见文件末尾）。
 */

const QSSettings = (() => {

  // ---------------------------------------------------------------- 常量

  // 与服务端 maxBackgroundSize 对齐
  const BG_MAX = 10 * 1024 * 1024;

  // 与服务端 server/settings.go 里的 defaultOpacity 保持一致
  const DEFAULT_OPACITY = { topbar: 85, upload: 85, files: 85 };

  // 三个不透明度滑块的 [元素 id, 数值标签 id, 设置键名]
  const OPACITY_FIELDS = [
    ['opTopbar', 'opTopbarVal', 'topbar'],
    ['opUpload', 'opUploadVal', 'upload'],
    ['opFiles', 'opFilesVal', 'files'],
  ];

  // 三档保留时长的控件描述。key 跟 /api/settings 里的字段名一一对应，
  // 加一档只要在这里加一行，标记、回显、保存都会跟着走。
  //
  // 0 的含义在这三档里**不一样**：文本和文件那两档是"永不删除"，
  // 验证码那档是"关掉这条规则"。所以文案分开写——说错会让人按错方向理解。
  const TTL_SPECS = [
    {
      key: 'textTTL',
      valueId: 'ttlValue', unitId: 'ttlUnit', saveId: 'ttlSave', hintId: 'ttlHintText',
      lead: '超过', subject: '的文本会被自动删除',
      zero: '不自动删除，文本会一直留着',
      zeroToast: '已关闭自动清理', onToast: '已开启自动清理',
      def: { value: 0, unit: 'day' },
    },
    {
      key: 'codeTTL',
      valueId: 'codeTtlValue', unitId: 'codeTtlUnit', saveId: 'codeTtlSave', hintId: 'codeTtlHint',
      lead: '识别为验证码的文本超过', subject: '会被自动删除',
      zero: '不特殊处理，验证码按上面的「文本保留时间」走',
      zeroToast: '已关掉验证码规则', onToast: '已开启验证码规则',
      def: { value: 10, unit: 'minute' },
    },
    {
      key: 'fileTTL',
      valueId: 'fileTtlValue', unitId: 'fileTtlUnit', saveId: 'fileTtlSave', hintId: 'fileTtlHint',
      lead: '超过', subject: '的文件会被自动删除',
      zero: '不自动删除，文件会一直留着',
      zeroToast: '已关闭自动清理', onToast: '已开启自动清理',
      def: { value: 0, unit: 'day' },
    },
  ];

  // 保留时长的单位。**加「分钟」主要是为了验证码那档**：默认 10 分钟用"天"
  // 根本表达不出来。另外两档也一起有了，它们本来就有短保留的需求。
  const TTL_UNITS = [['minute', '分钟'], ['hour', '小时'], ['day', '天']];

  function ttlUnitLabel(u) {
    for (const [v, t] of TTL_UNITS) if (v === u) return t;
    return '天';
  }

  function ttlUnitValid(u) {
    return TTL_UNITS.some(([v]) => v === u);
  }

  // 单位的人话写法。首页、文本页的列表都要用，所以从这里出——两边各写一份的话，
  // 迟早一边写"分钟"、另一边漏了 minute 掉进默认分支显示成"天"。
  function ttlUnitText(u) {
    return ttlUnitLabel(u);
  }

  // 把绝对到期时刻写成"还剩多久"。
  //
  // **单位按"四舍五入后的数值"挑，不是按"够不够一个单位"挑。** 这个区别很实际：
  // 到期时刻是 `created_at + ttl`，而用户是过一会儿才去设这个时长的——按
  // "够不够"挑的话，刚把保留时长设成 1 小时，列表上立刻显示"还剩 59 分钟"
  // （因为文件是十几秒前传的），看着像没设上。同理 2 天会显示成"还剩 1 天"。
  // 四舍五入之后这两种都落在正确的那一档上。
  //
  // 先判天数再判小时：`Math.round` 会给出 0，而 0 表示"该用更小的单位"。
  function fmtLeft(expiresAt) {
    const left = Number(expiresAt) - Math.floor(Date.now() / 1000);
    if (!(left > 0)) return '即将删除';
    const days = Math.round(left / 86400);
    if (days >= 1) return `还剩 ${days} 天`;
    const hours = Math.round(left / 3600);
    if (hours >= 1) return `还剩 ${hours} 小时`;
    const mins = Math.round(left / 60);
    if (mins >= 1) return `还剩 ${mins} 分钟`;
    return '即将删除';
  }

  // 「0」这一档到底等于什么，取决于全局设置，所以提示里要把全局值写出来。
  //
  // 只写「0 = 跟随全局设置」是不够的：用户看到列表上写着"还剩 1 小时"、点开编辑器
  // 却是"0 天"，会以为编辑器读错了。把全局值带上，0 才不是个谜。
  //
  // 读不到设置时退回该档的默认值（和"全局没开自动删除"是同一个意思），
  // 这样"设置还没拉回来"和"全局真的关着"显示的是同一句话，不会先错一下再改。
  function followNote(key) {
    const spec = TTL_SPECS.find((x) => x.key === key);
    const g = settings[key] || (spec && spec.def);
    if (g && Number(g.value) > 0) {
      return `0 = 跟随全局设置（${g.value} ${ttlUnitLabel(g.unit)}）`;
    }
    return '0 = 跟随全局设置（现在没开自动删除）';
  }

  function ttlToSecs(value, unit) {
    const per = { minute: 60, hour: 3600, day: 86400 }[unit] || 86400;
    return Math.max(0, Math.round(Number(value) || 0)) * per;
  }

  // 秒数 → (数值, 单位)。单条覆盖在库里只存秒数（只给机器用），要给人看、给人改
  // 就得还原成"1 天"而不是"86400 秒"。**优先用大单位，能整除才用**：86400 秒
  // 写成"1 天"，5400 秒写成"90 分钟"（面板只收整数，写成"1.5 小时"填不回去）。
  function secsToTtl(sec) {
    const n = Math.max(0, Math.round(Number(sec) || 0));
    if (n === 0) return { value: 0, unit: 'day' };
    if (n % 86400 === 0) return { value: n / 86400, unit: 'day' };
    if (n % 3600 === 0) return { value: n / 3600, unit: 'hour' };
    return { value: Math.max(1, Math.round(n / 60)), unit: 'minute' };
  }

  // 只出「[数字] [单位]」这两个控件。文本页的编辑框里用它——那边的保存/取消
  // 已经由卡片自己的按钮承担了，再摆一套会让人不知道该按哪个。
  function ttlFieldsMarkup(secs) {
    const t = secsToTtl(secs);
    const opts = TTL_UNITS.map(([v, label]) =>
      `<option value="${v}"${v === t.unit ? ' selected' : ''}>${label}</option>`).join('');
    return `<input type="number" class="ttl-num" min="0" max="10000" step="1" value="${t.value}" aria-label="保留时长">
          <select class="ttl-unit" aria-label="保留时长的单位">${opts}</select>`;
  }

  // 列表里"就地改单条保留时长"的那个小编辑器。首页和文本页共用同一份标记：
  // 各写一遍的话，"0 = 跟随全局设置"这句提示迟早只在一边出现——而它恰恰是
  // 这一档最容易理解错的地方（0 不是"立刻删"，也不是"永不删"）。
  function ttlEditorMarkup(secs, note) {
    return `<span class="ttl-edit">
          ${ttlFieldsMarkup(secs)}
          <button class="btn btn-sm" data-ttl-save>应用</button>
          <button class="btn btn-sm" data-ttl-cancel>取消</button>
          <span class="ttl-edit-note">${note || '0 = 跟随全局设置'}</span>
        </span>`;
  }

  // 读回编辑器里的秒数。校验口径和服务端一致（0..10000 的整数），免得填个
  // 3.5 天先跑到服务端再被打回来。
  function readTtlEditor(root) {
    const num = root && root.querySelector('.ttl-num');
    const unit = root && root.querySelector('.ttl-unit');
    if (!num || !unit) return { ok: false, msg: '找不到保留时长输入框' };
    const raw = String(num.value || '').trim();
    const value = raw === '' ? 0 : Number(raw);
    if (!Number.isInteger(value) || value < 0 || value > 10000) {
      return { ok: false, msg: '保留时长要填 0 到 10000 之间的整数' };
    }
    return { ok: true, secs: ttlToSecs(value, unit.value) };
  }

  // 三档保留时长的标记长得一模一样，用生成而不是抄三遍——抄的话以后改一处
  // 漏两处，而"看起来一样、行为不一样"的偏差最难查。
  function ttlBlock(spec, page, label, note) {
    const opts = TTL_UNITS
      .map(([v, t]) => `<option value="${v}">${t}</option>`).join('');
    return `
      <div class="field" data-page="${page}">
        <div class="field-label">${label}</div>
        <div class="path-row">
          <span class="ttl-lead">保留</span>
          <input type="number" class="ttl-num" id="${spec.valueId}" min="0" max="10000" step="1" value="${spec.def.value}">
          <select class="ttl-unit" id="${spec.unitId}">${opts}</select>
          <button class="btn btn-sm" id="${spec.saveId}">应用</button>
        </div>
        <div class="hint ttl-now" id="${spec.hintId}"></div>
        <div class="hint">${note}</div>
      </div>`;
  }

  const ICON_SUN = `<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="4.2"/><path d="M12 2v2.4M12 19.6V22M4.2 4.2l1.7 1.7M18.1 18.1l1.7 1.7M2 12h2.4M19.6 12H22M4.2 19.8l1.7-1.7M18.1 5.9l1.7-1.7"/></svg>`;
  const ICON_MOON = `<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M20.5 14.6A8.6 8.6 0 019.4 3.5a8.6 8.6 0 1011.1 11.1z"/></svg>`;

  // ---------------------------------------------------------------- 标记

  // 面板由 JS 注入而不是写死在两个 HTML 里：写两份迟早改一处漏一处，
  // 而这种"看起来一模一样、行为不一样"的偏差最难查。
  const MARKUP = `
<div class="overlay" id="overlay" hidden>
  <div class="sheet" role="dialog" aria-modal="true" aria-labelledby="sheetTitle">
    <div class="sheet-head">
      <h3 id="sheetTitle">设置</h3>
      <button class="icon-btn" id="sheetClose" title="关闭">
        <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round">
          <path d="M6 6l12 12M18 6L6 18"/>
        </svg>
      </button>
    </div>
    <div class="sheet-body">

      <div class="field">
        <div class="field-label">主题</div>
        <div class="seg" id="themeSeg">
          <button class="seg-item" data-theme-opt="">跟随系统</button>
          <button class="seg-item" data-theme-opt="light">浅色</button>
          <button class="seg-item" data-theme-opt="dark">深色</button>
        </div>
      </div>

      <div class="field">
        <div class="field-label">背景图</div>
        <div class="bg-row">
          <div class="bg-preview" id="bgPreview"></div>
          <div class="bg-actions">
            <button class="btn btn-sm" id="bgPick">选择图片</button>
            <button class="btn btn-sm btn-danger" id="bgClear">移除背景</button>
            <div class="hint">JPG / PNG / WebP / GIF / AVIF，不超过 10 MB</div>
          </div>
        </div>
        <div class="hint dark-only" id="bgDarkHint" hidden>深色模式下不显示背景图，切回浅色即可看到。</div>
      </div>

      <div class="field" id="blurField">
        <div class="field-label">背景图模糊度</div>
        <div class="slider">
          <input type="range" id="blurRange" min="0" max="40" step="1" value="0">
          <span class="slider-val" id="blurVal">0</span>
        </div>
      </div>

      <div class="field">
        <div class="field-label">面板不透明度</div>
        <div class="slider">
          <span class="slider-label">顶部栏</span>
          <input type="range" id="opTopbar" min="20" max="100" step="1" value="85">
          <span class="slider-val" id="opTopbarVal">85%</span>
        </div>
        <!-- 「上传区」「文件列表」只有首页有这两个区域，文本页放这两行没意义。
             data-page 加在 .slider 这一行上而不是整个 .field 上：顶部栏那一行
             两页都要留着，标在 .field 上会连它一起藏掉。 -->
        <div class="slider" data-page="files">
          <span class="slider-label">上传区</span>
          <input type="range" id="opUpload" min="20" max="100" step="1" value="85">
          <span class="slider-val" id="opUploadVal">85%</span>
        </div>
        <div class="slider" data-page="files">
          <span class="slider-label">文件列表</span>
          <input type="range" id="opFiles" min="20" max="100" step="1" value="85">
          <span class="slider-val" id="opFilesVal">85%</span>
        </div>
        <div class="hint">只在显示背景图时生效。调低能让背景透出来，但文字也会变难认。</div>
      </div>

      <div class="field" data-page="files">
        <div class="field-label">上传分片大小</div>
        <div class="path-row">
          <input type="number" id="chunkSizeValue" min="1" max="64" step="1" value="8" aria-label="上传分片大小">
          <span>MiB</span>
          <button class="btn btn-sm" id="chunkSizeSave">应用</button>
        </div>
        <div class="hint" id="chunkSizeHint">建议 4–16 MiB；iPhone / iPad 或 Wi-Fi 不稳定时建议 1–4 MiB，千兆内网可用 8–16 MiB。分片越大，单片失败时需要重传的数据越多。</div>
      </div>

      ${ttlBlock(TTL_SPECS[0], 'text', '文本保留时间',
        '只影响文本页的文本，不涉及上传的文件。')}

      ${ttlBlock(TTL_SPECS[1], 'text', '验证码过期时间',
        '整条是 4~8 位数字，或者短文里出现「验证码」「动态码」这类关键词并带着数字，就会被当成验证码。填 0 表示关掉这条规则，验证码按上面的「文本保留时间」走。')}

      <div class="field" data-page="text">
        <div class="field-label">设备记录</div>
        <div class="seg" id="pruneSeg">
          <button class="seg-item" data-prune-opt="0">保留</button>
          <button class="seg-item" data-prune-opt="1">随文本一起删除</button>
        </div>
        <div class="hint" id="pruneHint">设备记录留着，你给它起的名字下次还在。选「随文本一起删除」后，某台设备一条文本都不剩时，它的记录（含备注）也会被删掉——刚打开时会把已经空掉的记录立刻清一遍。只动设备记录，不删任何文本。</div>
      </div>

      ${ttlBlock(TTL_SPECS[2], 'files', '文件保留时间',
        '按上传时间算，超过就删，文件本体和分片一起清掉。')}

      <div class="field" data-page="files">
        <div class="field-label">文件存储位置</div>
        <div class="path-row">
          <input type="text" id="dataDirInput" spellcheck="false" autocomplete="off" placeholder="例如 /volume1/quickshare/data">
          <button class="btn btn-sm" id="dataDirSave">应用</button>
        </div>
        <div class="hint" id="dataDirHint"></div>
      </div>

      <div class="field">
        <div class="field-label">关于</div>
        <div class="path-row">
          <span id="verCurrent">QuickShare —</span>
          <span class="spacer"></span>
          <a class="btn btn-sm" id="verLink" target="_blank" rel="noopener noreferrer" hidden></a>
          <button class="btn btn-sm" id="verCheck">检查更新</button>
        </div>
        <div class="hint" id="verHint"></div>
      </div>

      <div class="field-note">
        设置保存在服务端，换设备、换浏览器打开都是同一套外观。
      </div>

    </div>
  </div>
</div>

<input type="file" id="bgInput" accept="image/jpeg,image/png,image/webp,image/gif,image/avif" hidden>
`;

  // ---------------------------------------------------------------- 状态

  let settings = {
    theme: '',
    background: null,
    bgBlur: 0,
    opacity: DEFAULT_OPACITY,
    textTTL: { value: 0, unit: 'day' },
    codeTTL: { value: 10, unit: 'minute' },
    fileTTL: { value: 0, unit: 'day' },
    pruneDevices: false,
    chunkSize: 8 * 1024 * 1024,
  };

  let storage = null;
  let mounted = false;

  // 页面相关的几个动作由页面注入。默认值是空实现，这样"忘了注入"最多是
  // 少个提示，不会整页炸掉。
  //
  // page 是**这块面板属于哪一页**（`files` / `text`），用来把 data-page 的块
  // 藏掉另一半。留空 = 全显示。
  let host = {
    page: '',
    getToken: () => '',
    onUnauthorized: () => {},
    onDataDirChanged: async () => {},
    onApplied: () => {},
  };

  // ---------------------------------------------------------------- 外观

  function mount() {
    if (mounted) return;
    const box = document.createElement('div');
    box.innerHTML = MARKUP;
    while (box.firstChild) document.body.appendChild(box.firstChild);
    mounted = true;
  }

  // 显式主题优先；为空（跟随系统）时看系统偏好
  function effectiveTheme() {
    return settings.theme ||
      (window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light');
  }

  // theme 为空字符串表示跟随系统，此时移除属性，让 CSS 的媒体查询接管
  function setThemeAttr(theme) {
    const root = document.documentElement;
    if (theme) root.setAttribute('data-theme', theme);
    else root.removeAttribute('data-theme');
  }

  function syncThemeUI() {
    const eff = effectiveTheme();
    const btn = $('themeBtn');
    if (btn) {
      btn.innerHTML = eff === 'dark' ? ICON_SUN : ICON_MOON;
      btn.title = eff === 'dark' ? '切换到浅色' : '切换到深色';
    }
    document.querySelectorAll('#themeSeg .seg-item').forEach((b) => {
      b.classList.toggle('active', b.getAttribute('data-theme-opt') === settings.theme);
    });
  }

  // 背景图只在浅色模式下显示。深色配色本身已经很暗，再压一张图上去，
  // 面板和背景会糊成一片，文字对比度掉得厉害。
  function bgShown() {
    const bg = settings.background;
    return !!(bg && bg.url) && effectiveTheme() === 'light';
  }

  // 把 0-100 的百分比转成 CSS 用的透明度
  function alpha(v) {
    const n = Math.min(100, Math.max(0, Number(v) || 0));
    return (n / 100).toFixed(2);
  }

  // 把一整套设置套到界面上。
  //
  // 所有外观改动都汇到这一个函数里，避免出现"换了主题却忘了重算背景图
  // 该不该显示"这类漏改——那种 bug 表现为切主题后背景图还赖着不走。
  function apply(s) {
    settings = s;
    const root = document.documentElement;
    const op = Object.assign({}, DEFAULT_OPACITY, s.opacity || {});
    const blur = Math.max(0, Number(s.bgBlur) || 0);
    const bg = s.background;
    const hasBg = !!(bg && bg.url);

    setThemeAttr(s.theme || '');

    // 预览缩略图：不管当前主题显不显示背景，预览都照常给出来，
    // 否则深色用户会以为自己的图丢了
    if (hasBg) {
      $('bgPreview').style.backgroundImage = `url("${bg.url}")`;
      $('bgPreview').textContent = '';
      $('bgClear').disabled = false;
    } else {
      $('bgPreview').style.backgroundImage = '';
      $('bgPreview').textContent = '未设置';
      $('bgClear').disabled = true;
    }

    // 背景图层的两个变量一起开关：图不显示时（没图 / 深色模式）模糊量也没意义。
    // 补边量由 CSS 从 --bg-blur 推导（inset: calc(-2 * var(--bg-blur))），
    // 所以这里只要如实写上模糊半径，放大程度就会跟着平滑变化，不会跳。
    if (bgShown()) {
      root.style.setProperty('--bg-image', `url("${bg.url}")`);
      root.style.setProperty('--bg-blur', blur + 'px');
      root.classList.add('has-bg');
    } else {
      root.style.removeProperty('--bg-image');
      root.style.removeProperty('--bg-blur');
      root.classList.remove('has-bg');
    }

    root.style.setProperty('--alpha-topbar', alpha(op.topbar));
    root.style.setProperty('--alpha-upload', alpha(op.upload));
    root.style.setProperty('--alpha-files', alpha(op.files));

    // 控件回显
    $('blurRange').value = blur;
    $('blurVal').textContent = blur + 'px';
    for (const [id, valId, key] of OPACITY_FIELDS) {
      $(id).value = op[key];
      $(valId).textContent = op[key] + '%';
    }

    // 没有背景图时，模糊度和不透明度调了也看不出来，直接禁用并说明
    for (const [id] of OPACITY_FIELDS) $(id).disabled = !hasBg;
    $('blurRange').disabled = !hasBg;
    $('bgDarkHint').hidden = !(hasBg && effectiveTheme() === 'dark');

    // 三档保留时长一起回显。缺字段时退回 spec.def——服务端还没升级、或者
    // 页面自己那次"只带外观"的 apply，都可能不带这几项；用默认值不会写错方向。
    for (const spec of TTL_SPECS) {
      const t = s[spec.key] || spec.def;
      $(spec.valueId).value = Number(t.value) || 0;
      $(spec.unitId).value = ttlUnitValid(t.unit) ? t.unit : spec.def.unit;
      syncTTLHint(spec);
    }

    const chunk = Number(s.chunkSize) || 8 * 1024 * 1024;
    $('chunkSizeValue').value = Math.round(chunk / (1024 * 1024));

    // 设备记录：跟 #themeSeg 一样是分段控件，省得为一个布尔开关另写一套
    // switch 组件、再补一遍两套主题的对比度
    const prune = !!s.pruneDevices;
    document.querySelectorAll('#pruneSeg .seg-item').forEach((b) => {
      b.classList.toggle('active', b.getAttribute('data-prune-opt') === (prune ? '1' : '0'));
    });

    syncThemeUI();
    host.onApplied(s);
  }

  // 把当前填的保留时长写成一句人话。
  //
  // 0 必须明确说成"不自动删除"（验证码那档是"不特殊处理"）——只写个 0 或者
  // "0 天"，用户会理解成"立刻就删"，方向正好相反。所以文案按 spec 分开写。
  function syncTTLHint(spec) {
    const v = Number($(spec.valueId).value) || 0;
    const unit = ttlUnitLabel($(spec.unitId).value);
    $(spec.hintId).textContent = v > 0
      ? `${spec.lead} ${v} ${unit}${spec.subject}`
      : spec.zero;
  }

  // 保存保留时长。数字和单位是一组，所以用「应用」按钮提交，
  // 而不是像滑块那样改完就发——否则改单位的一瞬间会用一个半截的值落库。
  async function saveChunkSize() {
    const raw = $('chunkSizeValue').value.trim();
    const value = raw === '' ? 8 : Number(raw);
    if (!Number.isInteger(value) || value < 1 || value > 64) {
      toast('分片大小要填 1 到 64 MiB 之间的整数', 'err');
      return;
    }
    if (await save({ chunkSize: value * 1024 * 1024 })) {
      toast('分片大小已更新；只影响新的上传任务', 'ok');
    }
  }

  async function saveTTL(spec) {
    const raw = $(spec.valueId).value.trim();
    const value = raw === '' ? 0 : Number(raw);
    if (!Number.isInteger(value) || value < 0 || value > 10000) {
      toast('保留时长要填 0 到 10000 之间的整数', 'err');
      return;
    }
    await save({ [spec.key]: { value, unit: $(spec.unitId).value } });
    toast(value > 0 ? spec.onToast : spec.zeroToast, 'ok');
  }

  // 本地即时预览：拖动滑块时先把效果套上去，不发请求。
  // 松手（change）才真正落库，避免一次拖动打几十个请求。
  function preview(patch) {
    const next = Object.assign({}, settings, patch);
    if (patch.opacity) {
      next.opacity = Object.assign({}, settings.opacity, patch.opacity);
    }
    apply(next);
  }

  // 保存外观改动。先乐观更新到界面，存不上再从服务端拉回真值——
  // 拖滑块要立刻见效，不能等一个来回。
  async function save(patch) {
    preview(patch);

    try {
      apply(await api('PUT', '/api/settings', patch));
      return true;
    } catch (e) {
      toast(e.message, 'err');
      // 回滚到服务端的真实状态，而不是刚才的预览值
      try {
        apply(await fetch('/api/settings').then((r) => r.json()));
      } catch (_) { /* 拉不回来就先这样，下次刷新会纠正 */ }
      return false;
    }
  }

  function saveTheme(theme) {
    return save({ theme });
  }

  async function uploadBackground(file) {
    if (file.size > BG_MAX) {
      toast(`背景图不能超过 ${fmtSize(BG_MAX)}`, 'err');
      return;
    }

    const headers = { 'Content-Type': file.type || 'application/octet-stream' };
    const token = host.getToken();
    if (token) headers['X-Admin-Token'] = token;

    const btn = $('bgPick');
    btn.disabled = true;
    btn.textContent = '上传中…';
    try {
      const res = await fetch('/api/background', { method: 'POST', headers, body: file });
      if (res.status === 401) {
        host.onUnauthorized();
        throw new Error('需要访问口令');
      }
      if (!res.ok) {
        let msg = `上传失败 (${res.status})`;
        try { msg = (await res.json()).error || msg; } catch (_) { /* 忽略 */ }
        throw new Error(msg);
      }
      apply(await res.json());
      toast('背景已更新', 'ok');
    } catch (e) {
      toast(e.message, 'err');
    } finally {
      btn.disabled = false;
      btn.textContent = '选择图片';
    }
  }

  async function clearBackground() {
    if (!confirm('移除当前背景图？')) return;
    try {
      apply(await api('DELETE', '/api/background'));
      toast('背景已移除', 'ok');
    } catch (e) {
      toast(e.message, 'err');
    }
  }

  // ---------------------------------------------------------------- 存储位置

  async function loadStorage() {
    const input = $('dataDirInput');
    const btn = $('dataDirSave');
    const hint = $('dataDirHint');

    try {
      storage = await api('GET', '/api/storage');
    } catch (_) {
      storage = null;
    }

    if (!storage) {
      input.value = '';
      input.disabled = true;
      btn.disabled = true;
      hint.textContent = '需要管理权限才能查看和修改存储位置。';
      return;
    }

    input.value = storage.dataDir;
    input.disabled = !!storage.locked;
    btn.disabled = !!storage.locked;
    hint.textContent = storage.locked
      ? storage.reason
      : '改完立即生效。若目录里已有文件，会先问你要不要一起迁移过去。';
  }

  async function applyDataDir() {
    const path = $('dataDirInput').value.trim();
    if (!path) {
      toast('目录不能为空', 'err');
      return;
    }
    if (storage && path === storage.dataDir) {
      toast('目录没有变化');
      return;
    }

    // 迁移是有代价的（跨盘时要真拷一遍），所以让用户明确选一次
    const migrate = confirm(
      `把文件存储位置切换到：\n${path}\n\n` +
      '「确定」= 连同现有文件一起迁移过去\n' +
      '「取消」= 只换目录，新目录从空开始（原文件仍留在旧目录）'
    );

    const btn = $('dataDirSave');
    btn.disabled = true;
    btn.textContent = '切换中…';
    try {
      const res = await api('PUT', '/api/storage', { path, migrate });
      toast(`已切换到 ${res.dataDir}`, 'ok');
      if (res.leftBehind) {
        toast(`旧数据仍保留在 ${res.leftBehind}，确认无误后可自行删除`);
      }
      if (res.warning) toast(res.warning, 'err');
      await loadStorage();
      await host.onDataDirChanged();
    } catch (e) {
      toast(e.message, 'err');
      await loadStorage();
    } finally {
      btn.disabled = false;
      btn.textContent = '应用';
    }
  }

  // ---------------------------------------------------------------- 版本

  // 版本与更新提示。
  //
  // 服务端**第一次**查询是在后台跑的（NAS 上没外网时同步查会卡满超时），
  // 所以这里可能先拿到一个 checking=true 的空结果。补问两轮就够——
  // 不做无限轮询：没有外网这个状态永远不会有结果，轮下去只是白费请求。
  async function loadVersion(retries) {
    let v;
    try {
      v = await api('GET', '/api/version');
    } catch (_) {
      return; // 拿不到就整块留空，别在设置面板里弹错误
    }
    renderVersion(v);
    if (v.checking && retries > 0) {
      setTimeout(() => { if (isOpen()) loadVersion(retries - 1); }, 1500);
    }
  }

  function renderVersion(v) {
    $('verCurrent').textContent = 'QuickShare ' + (v.current || '未知');

    // 只认 http(s)：这个地址虽然是服务端给的，但服务端又是从外部 API 抄回来的，
    // 没必要因为一个畸形字段让页面里冒出 javascript: 链接。
    const url = /^https?:\/\//i.test(v.url || '') ? v.url : '';
    const link = $('verLink');
    link.hidden = !(v.hasUpdate && url);
    if (link.hidden) {
      link.removeAttribute('href');
    } else {
      link.href = url;
      link.textContent = '有新版本 ' + v.latest;
    }

    const hint = $('verHint');
    if (v.hasUpdate) {
      hint.textContent = url
        ? '去发布页下载新版本，或把镜像换成新标签后重启。'
        : '有新版本 ' + v.latest + '。';
    } else if (v.checking) {
      hint.textContent = '正在检查更新…';
    } else if (v.error) {
      hint.textContent = '检查更新失败：' + v.error;
    } else if (!v.enabled) {
      hint.textContent = '这个构建没有配置更新检查。';
    } else if (!v.latest) {
      // **配置了却读不到**，和"没配置"是两回事，以前共用一句话是错的。
      // 最常见的成因：仓库是私有的——未认证请求查私有仓库一律 404，
      // 而服务端刻意不把 404 当错误（"还没发过 Release"也确实正常）。
      // 所以这里不能报错，但也不能说成"没配置"，那会把人引到错误的方向。
      hint.textContent = '已配置更新检查，但读不到发布信息（仓库可能不是公开的，或还没发过 Release）。';
    } else if (v.current === 'dev') {
      // 开发版比不出大小，所以永远不提示"有更新"，但要让人看得到最新发布版是哪个
      hint.textContent = '开发版构建；最新发布版是 ' + v.latest + '。';
    } else {
      hint.textContent = '已是最新版本。';
    }
  }

  // 「检查更新」按钮：绕过服务端缓存，同步查一次。
  async function checkVersion() {
    const btn = $('verCheck');
    btn.disabled = true;
    btn.textContent = '检查中…';
    try {
      renderVersion(await api('POST', '/api/version/check'));
    } catch (e) {
      toast(e.message, 'err');
    } finally {
      btn.disabled = false;
      btn.textContent = '检查更新';
    }
  }

  // ---------------------------------------------------------------- 开关

  function open(on) {
    $('overlay').hidden = !on;
    document.documentElement.classList.toggle('modal-open', on);
    if (on) {
      loadStorage();
      loadVersion(2);
    }

    // 把开关状态同步到地址栏，于是 #settings 能直接打开面板。
    // replaceState 而不是 location.hash，后者会往历史里塞一条，用户按返回键
    // 只会关掉面板而不是离开页面。file:// 下可能不允许，忽略即可。
    try {
      if (on) history.replaceState(null, '', '#settings');
      else if (location.hash === '#settings') history.replaceState(null, '', location.pathname);
    } catch (_) { /* 忽略 */ }
  }

  function isOpen() {
    return !$('overlay').hidden;
  }

  // ---------------------------------------------------------------- 事件

  function bind() {
    // 顶栏那个按钮是"一键反转"：当前是深色就去浅色，反之亦然
    $('themeBtn').addEventListener('click', () => {
      saveTheme(effectiveTheme() === 'dark' ? 'light' : 'dark');
    });

    $('settingsBtn').addEventListener('click', () => open(true));
    $('sheetClose').addEventListener('click', () => open(false));
    $('verCheck').addEventListener('click', checkVersion);
    $('overlay').addEventListener('click', (e) => {
      if (e.target === $('overlay')) open(false);
    });
    // Escape 关面板。页面自己（文本页）也监听 document 的 Escape，用来退多选 /
    // 取消编辑 / 关设备面板——两层叠着时，一次按键只该关掉最上面那层。
    //
    // 光靠页面那层判断 `QSSettings.isOpen()` 是不够的：那个判断依赖"本监听器先跑"，
    // 而它确实先跑（init() 在页面 bind() 之前），跑完面板已经 hidden 了，页面那层
    // 再看到 isOpen() 就是 false，于是**一次 Escape 关掉面板又退出多选**。
    // 所以这里把这次按键截住，不让它继续往下传。
    //
    // 配合页面那层的 isOpen() 判断，两边各守一半，谁先注册都不会错：
    //   本监听器先跑 -> 关面板 + 截住，页面那层根本收不到
    //   页面那层先跑 -> 它看到 isOpen() 为真直接 return，本监听器再关面板
    document.addEventListener('keydown', (e) => {
      if (e.key !== 'Escape' || !isOpen()) return;
      open(false);
      e.stopImmediatePropagation();
    });

    $('themeSeg').addEventListener('click', (e) => {
      const btn = e.target.closest('[data-theme-opt]');
      if (btn) saveTheme(btn.getAttribute('data-theme-opt'));
    });

    // 跟随系统时，系统换了配色要同步按钮图标
    window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', () => {
      if (!settings.theme) syncThemeUI();
    });

    $('bgPick').addEventListener('click', () => $('bgInput').click());
    $('bgInput').addEventListener('change', () => {
      const f = $('bgInput').files[0];
      $('bgInput').value = '';
      if (f) uploadBackground(f);
    });
    $('bgClear').addEventListener('click', clearBackground);

    // 模糊度：拖动即时预览，松手落库
    const blurEl = $('blurRange');
    blurEl.addEventListener('input', () => preview({ bgBlur: Number(blurEl.value) }));
    blurEl.addEventListener('change', () => save({ bgBlur: Number(blurEl.value) }));

    // 三个区域的不透明度，同一个套路
    for (const [id, , key] of OPACITY_FIELDS) {
      const el = $(id);
      el.addEventListener('input', () => preview({ opacity: { [key]: Number(el.value) } }));
      el.addEventListener('change', () => save({ opacity: { [key]: Number(el.value) } }));
    }

    // 存储位置
    $('dataDirSave').addEventListener('click', applyDataDir);
    $('dataDirInput').addEventListener('keydown', (e) => {
      if (e.key === 'Enter') applyDataDir();
    });
    $('chunkSizeSave').addEventListener('click', saveChunkSize);
    $('chunkSizeValue').addEventListener('keydown', (e) => {
      if (e.key === 'Enter') saveChunkSize();
    });

    // 三档保留时长，同一个套路
    for (const spec of TTL_SPECS) {
      $(spec.saveId).addEventListener('click', () => saveTTL(spec));
      $(spec.valueId).addEventListener('input', () => syncTTLHint(spec));
      $(spec.unitId).addEventListener('change', () => syncTTLHint(spec));
      $(spec.valueId).addEventListener('keydown', (e) => {
        if (e.key === 'Enter') saveTTL(spec);
      });
    }

    // 设备记录：点一下即落库（和主题一样，没有"半截值"的问题，不需要应用按钮）
    $('pruneSeg').addEventListener('click', (e) => {
      const btn = e.target.closest('[data-prune-opt]');
      if (btn) save({ pruneDevices: btn.getAttribute('data-prune-opt') === '1' });
    });

    // 地址栏带 #settings 直接进面板；已经在本页时改 hash 也要开
    // （同文档导航不会重新加载文档，init() 不会再跑）。
    // 不会和 open() 里的 replaceState 打架：replaceState 不触发 hashchange。
    window.addEventListener('hashchange', () => {
      if (location.hash === '#settings') open(true);
      else if (isOpen()) open(false);
    });
  }

  // ---------------------------------------------------------------- 入口

  // 带 data-page 的块只在自己那页显示（没标的 = 两页都显示）。
  //
  // 用 hidden 属性而不是给 <html> 挂 class：`[hidden]{display:none!important}`
  // 样式表里已经有了，不必为此去改两个页面共用的 style.css，也不必让每个 HTML
  // 各记一份页面标识——那种"两份要同步"的东西迟早分叉。
  //
  // host.page 为空时**整块都不动**：宁可在不该显示的页上多显示几行，也不要
  // 因为某个页面忘了传标识，让整个设置面板空掉。
  function applyPage(page) {
    if (!page) return;
    document.querySelectorAll('#overlay [data-page]').forEach((el) => {
      el.hidden = el.getAttribute('data-page') !== page;
    });
  }

  function init(h) {
    host = Object.assign({}, host, h || {});
    mount();
    applyPage(host.page);
    bind();
    if (location.hash === '#settings') open(true);
    return settings;
  }

  return {
    init,
    apply,
    open,
    isOpen,
    saveTheme,
    syncThemeUI,
    effectiveTheme,
    // 两页共用的小工具：单位人话写法、剩余时间、单条保留时长的就地编辑器
    ttlUnitText,
    fmtLeft,
    secsToTtl,
    ttlToSecs,
    followNote,
    ttlFieldsMarkup,
    ttlEditorMarkup,
    readTtlEditor,
    get settings() { return settings; },
  };
})();
