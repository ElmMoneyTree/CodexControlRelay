package server

import "net/http"

func (s *Server) claimPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := pathID(r.URL.Path, "/claim/", ""); !ok {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "Not found"})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' blob:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(claimHTML))
}

func (s *Server) claimAsset(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "public, max-age=3600")
	switch r.URL.Path {
	case "/claim/app.js":
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		_, _ = w.Write([]byte(claimJavaScript))
	case "/claim/app.css":
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		_, _ = w.Write([]byte(claimCSS))
	default:
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "Not found"})
	}
}

const claimHTML = `<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width,initial-scale=1,viewport-fit=cover">
  <title>认领 Codex 设备</title>
  <link rel="stylesheet" href="/claim/app.css?v=3">
  <script src="/claim/app.js?v=3" defer></script>
</head>
<body>
  <main class="card">
    <div class="brand">Codex Control</div>
    <h1 id="heading">认领这台 Codex 设备</h1>
    <p id="status" class="muted">正在读取安全认领请求……</p>
    <section id="mobile-claim" hidden>
      <div class="qr"><img id="qr" alt="使用 Codex Control 手机 App 扫描"></div>
      <p class="hint center">请使用已登录的 Codex Control 手机 App 扫码，在手机上核对安全指纹、选择空间并确认认领。</p>
      <p class="hint center">二维码仅在几分钟内有效，且只能使用一次。认领完成后，Codex 插件将自动连接，可关闭此页面。</p>
    </section>
    <section id="complete" hidden role="status" aria-live="polite">
      <p>认领成功，请在手机设备查看。</p>
      <p class="hint">正在尝试关闭此页面。若页面未自动关闭，可以手动关闭。</p>
    </section>
    <p id="error" class="error" role="alert"></p>
  </main>
</body>
</html>`

const claimCSS = `:root{color-scheme:light dark;font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;background:#f4f6f8;color:#171a1f}*{box-sizing:border-box}body{margin:0;min-height:100vh;display:grid;place-items:center;padding:24px;background:radial-gradient(circle at top,#dfe8ff,#f7f8fa 55%)}.card{width:min(100%,480px);padding:32px;border-radius:24px;background:rgba(255,255,255,.92);box-shadow:0 24px 80px rgba(31,45,80,.16);backdrop-filter:blur(20px)}.brand{font-size:14px;font-weight:700;color:#3867d6;letter-spacing:.04em}h1{font-size:28px;margin:10px 0 8px}.muted,.hint{color:#68707d}.hint{font-size:13px;line-height:1.5}.center{text-align:center}.qr{display:grid;place-items:center;margin:18px 0 4px}.qr img{width:190px;height:190px;border-radius:12px;background:white;padding:8px}.error{color:#c62828;min-height:1.5em}@media(prefers-color-scheme:dark){:root{background:#111318;color:#f2f4f8}body{background:radial-gradient(circle at top,#18223e,#111318 55%)}.card{background:rgba(28,31,38,.94);box-shadow:none}.muted,.hint{color:#aab1bd}}`

const claimJavaScript = `(() => {
  const claimId = decodeURIComponent(location.pathname.slice('/claim/'.length));
  const secretKey = 'codex-claim-secret:' + claimId;
  const completeKey = 'codex-claim-complete:' + claimId;
  const fragmentSecret = new URLSearchParams(location.hash.slice(1)).get('secret');
  if (fragmentSecret) {
    sessionStorage.setItem(secretKey, fragmentSecret);
    history.replaceState(null, '', location.pathname);
  }
  const claimSecret = sessionStorage.getItem(secretKey);
  const byId = id => document.getElementById(id);
  const endpoint = '/v1/device-claims/' + encodeURIComponent(claimId);
  let qrURL = null;
  let timer;
  let stopped = false;

  function hideQRCode() {
    byId('mobile-claim').hidden = true;
    byId('qr').removeAttribute('src');
    if (qrURL) URL.revokeObjectURL(qrURL);
    qrURL = null;
  }

  function complete() {
    stopped = true;
    clearTimeout(timer);
    hideQRCode();
    sessionStorage.removeItem(secretKey);
    sessionStorage.setItem(completeKey, 'true');
    byId('heading').textContent = '认领成功';
    byId('status').textContent = '';
    byId('error').textContent = '';
    byId('complete').hidden = false;
    // Script-opened tabs can close; regular browser tabs retain the success notice.
    timer = setTimeout(() => { try { window.close(); } catch {} }, 1500);
  }

  function fail(message) {
    stopped = true;
    hideQRCode();
    sessionStorage.removeItem(secretKey);
    byId('status').textContent = '';
    byId('error').textContent = message;
  }

  async function request(suffix) {
    const response = await fetch(endpoint + suffix, {
      headers: { 'X-Claim-Secret': claimSecret }, cache: 'no-store',
      signal: AbortSignal.timeout(10000)
    });
    if (!response.ok) {
      const error = new Error('请求失败（' + response.status + '）');
      error.status = response.status;
      throw error;
    }
    return response;
  }

  async function checkStatus() {
    if (stopped) return;
    try {
      const progress = await (await request('/status')).json();
      if (stopped) return;
      if (progress.status === 'approved') { complete(); return; }
      if (progress.status === 'expired' || progress.status === 'rejected') {
        fail('二维码已失效，请从电脑端插件重新发起认领。'); return;
      }
      if (progress.status !== 'pending') throw new Error('未知认领状态');
      if (!qrURL) {
        let response;
        try { response = await request('/qr'); }
        catch (error) {
          // Approval may land between the status check and QR request.
          if (error.status === 410) { timer = setTimeout(checkStatus, 1000); return; }
          throw error;
        }
        const blob = await response.blob();
        if (stopped) return;
        qrURL = URL.createObjectURL(blob);
        byId('qr').src = qrURL;
        byId('mobile-claim').hidden = false;
      }
      byId('status').textContent = '请使用手机 App 扫码认领设备。';
      byId('error').textContent = '';
    } catch (error) {
      if (stopped) return;
      if (error.status === 410) { fail('认领链接无效，请从电脑端插件重新发起认领。'); return; }
      byId('error').textContent = '暂时无法获取认领状态，正在自动重试……';
    }
    if (!stopped) timer = setTimeout(checkStatus, 2000);
  }

  window.addEventListener('pagehide', event => {
    if (!event.persisted) { stopped = true; clearTimeout(timer); hideQRCode(); }
  });
  if (sessionStorage.getItem(completeKey) === 'true') { complete(); return; }
  if (!claimId || !claimSecret) { fail('认领链接无效或缺少安全信息。请从插件重新发起认领。'); return; }
  checkStatus();
})();`
