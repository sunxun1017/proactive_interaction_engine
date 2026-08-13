package webpanel

const indexTemplate = `<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <meta name="desktop-token" content="{{.Token}}">
  <title>主动陪伴</title>
  <link rel="stylesheet" href="/assets/styles.css">
</head>
<body>
  <main>
    <p id="connection-status" role="status">正在连接本地服务…</p>
    <section id="avatar" data-wasm="/assets/app.wasm" aria-live="polite">
      <p id="avatar-mode">IDLE</p>
      <p id="avatar-text"></p>
    </section>
    <section aria-label="采集状态">
      <p id="camera-indicator">摄像头：未启用</p>
      <p id="microphone-indicator">麦克风：未启用</p>
      <p id="biometric-indicator">生物识别：未启用</p>
    </section>
    <section id="permissions" aria-label="隐私权限"></section>
    <section id="providers" aria-label="能力状态"></section>
    <button id="reject-button" type="button">停止当前互动</button>
  </main>
  <script src="/assets/wasm_exec.js"></script>
  <script src="/assets/bootstrap.js"></script>
</body>
</html>
`

const stylesAsset = `:root { color-scheme: light dark; font-family: system-ui, sans-serif; }
body { margin: 0; min-height: 100vh; display: grid; place-items: center; }
main { width: min(42rem, calc(100% - 2rem)); display: grid; gap: 1rem; }
#avatar { min-height: 12rem; display: grid; place-content: center; text-align: center; border: 1px solid currentColor; border-radius: 1rem; }
#reject-button { min-height: 3rem; }
`

const bootstrapAsset = `"use strict";
(async () => {
  const status = document.getElementById("connection-status");
  const avatar = document.getElementById("avatar");
  try {
    if (!avatar || !globalThis.Go) throw new Error("WebAssembly runtime unavailable");
    const wasmURL = avatar.dataset.wasm;
    if (!wasmURL || !wasmURL.startsWith("/")) throw new Error("WebAssembly path unavailable");
    const go = new Go();
    const response = await fetch(wasmURL, { cache: "no-store", credentials: "same-origin" });
    if (!response.ok) throw new Error("WebAssembly download failed");
    const result = await WebAssembly.instantiateStreaming(response, go.importObject);
    status.textContent = "本地服务已连接";
    await go.run(result.instance);
  } catch (_) {
    status.textContent = "本地面板启动失败";
  }
})();
`
