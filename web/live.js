'use strict';

/* QuickShare 的实时通知（SSE）客户端。首页和文本页共用这一份。
 *
 * 为什么是 SSE 而不是 WebSocket：这里只需要服务端单向通知，SSE 走的是普通 HTTP，
 * 不需要握手升级、不需要额外依赖，断线后前端自己重连即可。
 *
 * 为什么用 fetch + ReadableStream 而不是 EventSource：EventSource 不能带自定义
 * 请求头，而本项目的鉴权走 X-Admin-Token，一旦设了 QS_ADMIN_TOKEN，
 * EventSource 会直接 401 连不上。
 *
 * 抽成独立文件而不是各页各写一份：退避重连、代次守卫这些逻辑最容易写歪
 * （重连时叠加出第二条连接、旧连接的回调继续跑），两份迟早会不一致。
 *
 * 用法：
 *   const live = QSLive.connect({
 *     token() { return state.token; },   // 每次（重）连时取一次口令
 *     onTopic(topic) { ... },            // 收到某类变更，topic 是 'files' / 'texts'
 *     onAuthFail() { ... },              // 口令不对，重连也没用
 *   });
 *   live.restart();                      // 拿到口令后重连
 */
window.QSLive = (() => {
  // 解析一条 SSE 消息（已经被空行切开）。只关心事件名，data 一律忽略——
  // 事件本身没有载荷，前端收到任何通知都只是"重新拉一遍"。
  function parseSSE(raw) {
    let event = 'message';
    for (const line of raw.split('\n')) {
      if (line.startsWith(':')) continue; // 注释行，心跳用的
      const i = line.indexOf(':');
      const field = i < 0 ? line : line.slice(0, i);
      const value = i < 0 ? '' : line.slice(i + 1).replace(/^ /, '');
      if (field === 'event') event = value;
    }
    return event;
  }

  function connect(opts = {}) {
    const onTopic = opts.onTopic || (() => {});
    const onAuthFail = opts.onAuthFail || (() => {});
    const getToken = opts.token || (() => '');

    // 连接代次。每次重连都自增，用来让"上一代的异步回调"自己失效——
    // 否则 abort 掉的旧连接可能在 await 之后继续往下走，叠加出第二条连接。
    let gen = 0;
    let abort = null;
    let timer = null;
    let retry = 0;

    function stop() {
      gen++;
      if (timer) { clearTimeout(timer); timer = null; }
      if (abort) { abort.abort(); abort = null; }
    }

    // 退避重连：1s、2s、4s……封顶 30s。
    // 服务端重启或网络抖动时，别拿每秒一次的请求去砸它；连上后退避清零。
    function scheduleReconnect() {
      if (timer) return;
      const delay = Math.min(1000 * 2 ** retry, 30000);
      retry++;
      timer = setTimeout(() => {
        timer = null;
        start();
      }, delay);
    }

    async function start() {
      stop();
      const my = gen;
      const ctrl = new AbortController();
      abort = ctrl;

      const headers = {};
      const t = getToken();
      if (t) headers['X-Admin-Token'] = t;

      let res;
      try {
        res = await fetch('/api/events', { headers, signal: ctrl.signal });
      } catch (_) {
        if (my === gen) scheduleReconnect();
        return;
      }
      if (my !== gen) return; // 已被更新的连接取代

      if (res.status === 401) {
        // 口令不对，重连多少次都一样，交给鉴权界面
        onAuthFail();
        return;
      }
      if (!res.ok || !res.body) {
        scheduleReconnect();
        return;
      }

      retry = 0; // 连上了

      const reader = res.body.getReader();
      const decoder = new TextDecoder();
      let buf = '';
      try {
        for (;;) {
          const { value, done } = await reader.read();
          if (done) break;
          if (my !== gen) return;
          buf += decoder.decode(value, { stream: true });
          let i;
          while ((i = buf.indexOf('\n\n')) >= 0) {
            const raw = buf.slice(0, i);
            buf = buf.slice(i + 2);
            const ev = parseSSE(raw);
            // ready 只是"通道通了"的确认，不是变更
            if (ev && ev !== 'ready') onTopic(ev);
          }
        }
      } catch (_) {
        // abort 或连接断开，下面统一走重连
      }
      if (my === gen) scheduleReconnect();
    }

    start();
    return { stop, restart: start };
  }

  return { connect };
})();
