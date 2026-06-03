const CAPTURE_SOURCE = "turnstile-inspector";
const INJECTED_SOURCE = "turnstile-inspector-page";
const SEEN_SIGNATURES = new Set();

function sanitizeValue(value) {
  if (value === null || value === undefined) {
    return "";
  }
  if (typeof value === "string") {
    return value.trim();
  }
  try {
    return JSON.stringify(value);
  } catch (_err) {
    return String(value);
  }
}

function buildSignature(payload) {
  return [
    payload.source || "",
    payload.url || "",
    payload.sitekey || "",
    payload.action || "",
    payload.cdata || "",
    payload.widgetId || "",
    payload.token || ""
  ].join("|");
}

function sendCapture(payload) {
  const signature = buildSignature(payload);
  if (SEEN_SIGNATURES.has(signature)) {
    return;
  }
  SEEN_SIGNATURES.add(signature);

  chrome.runtime.sendMessage({
    type: "turnstileCapture",
    payload: {
      ...payload,
      source: payload.source || CAPTURE_SOURCE,
      pageTitle: document.title,
      url: window.location.href
    }
  });
}

function parseIframeCapture(iframe) {
  const src = iframe.getAttribute("src") || "";
  if (!src.includes("challenges.cloudflare.com")) {
    return null;
  }

  let parsed;
  try {
    parsed = new URL(src);
  } catch (_err) {
    return null;
  }

  return {
    source: "iframe-src",
    sitekey: sanitizeValue(parsed.searchParams.get("k") || parsed.searchParams.get("sitekey")),
    action: sanitizeValue(parsed.searchParams.get("action")),
    cdata: sanitizeValue(parsed.searchParams.get("cData") || parsed.searchParams.get("cdata")),
    iframeSrc: src
  };
}

function scanDomForTurnstile() {
  const widgets = document.querySelectorAll(".cf-turnstile,[data-sitekey]");
  widgets.forEach((node) => {
    const sitekey = sanitizeValue(node.getAttribute("data-sitekey"));
    const action = sanitizeValue(node.getAttribute("data-action"));
    const cdata = sanitizeValue(node.getAttribute("data-cdata"));

    if (!sitekey && !action && !cdata) {
      return;
    }

    sendCapture({
      source: "dom-attributes",
      sitekey,
      action,
      cdata,
      htmlSnippet: (node.outerHTML || "").slice(0, 500)
    });
  });

  const iframes = document.querySelectorAll('iframe[src*="challenges.cloudflare.com"]');
  iframes.forEach((iframe) => {
    const payload = parseIframeCapture(iframe);
    if (payload) {
      sendCapture(payload);
    }
  });
}

function injectPageHook() {
  const script = document.createElement("script");
  script.textContent = `
    (() => {
      const SOURCE = "${INJECTED_SOURCE}";

      function sanitize(value) {
        if (value === null || value === undefined) return "";
        if (typeof value === "string") return value.trim();
        try { return JSON.stringify(value); } catch (_err) { return String(value); }
      }

      function emit(payload) {
        window.postMessage({ source: SOURCE, payload }, "*");
      }

      function captureFromRender(params, widgetId) {
        const p = (params && typeof params === "object") ? params : {};
        emit({
          source: "turnstile-render",
          widgetId: sanitize(widgetId),
          sitekey: sanitize(p.sitekey || p.siteKey || ""),
          action: sanitize(p.action || ""),
          cdata: sanitize(p.cData || p.cdata || ""),
          retry: sanitize(p.retry || ""),
          execution: sanitize(p.execution || "")
        });
      }

      function wrapCallbacks(params, widgetId) {
        if (!params || typeof params !== "object") {
          return params;
        }

        const callback = params.callback;
        if (typeof callback === "function") {
          params.callback = function(token) {
            emit({
              source: "turnstile-callback",
              widgetId: sanitize(widgetId),
              token: sanitize(token),
              sitekey: sanitize(params.sitekey || params.siteKey || ""),
              action: sanitize(params.action || ""),
              cdata: sanitize(params.cData || params.cdata || "")
            });
            return callback.apply(this, arguments);
          };
        }

        return params;
      }

      function patchTurnstile(turnstileObj) {
        if (!turnstileObj || turnstileObj.__turnstileInspectorPatched) {
          return;
        }

        const originalRender = turnstileObj.render;
        if (typeof originalRender === "function") {
          turnstileObj.render = function(container, params) {
            const wrappedParams = wrapCallbacks(params, "");
            const widgetId = originalRender.call(this, container, wrappedParams);
            captureFromRender(wrappedParams, widgetId);
            return widgetId;
          };
        }

        turnstileObj.__turnstileInspectorPatched = true;
      }

      let innerTurnstile = window.turnstile;
      patchTurnstile(innerTurnstile);

      try {
        Object.defineProperty(window, "turnstile", {
          configurable: true,
          enumerable: true,
          get() {
            return innerTurnstile;
          },
          set(value) {
            innerTurnstile = value;
            patchTurnstile(innerTurnstile);
          }
        });
      } catch (_err) {}

      const interval = setInterval(() => {
        patchTurnstile(window.turnstile || innerTurnstile);
      }, 500);

      setTimeout(() => clearInterval(interval), 120000);
    })();
  `;

  (document.documentElement || document.head || document.body).appendChild(script);
  script.remove();
}

window.addEventListener("message", (event) => {
  if (event.source !== window) {
    return;
  }
  if (event.data?.source !== INJECTED_SOURCE) {
    return;
  }

  const payload = event.data.payload || {};
  sendCapture({
    source: sanitizeValue(payload.source),
    sitekey: sanitizeValue(payload.sitekey),
    action: sanitizeValue(payload.action),
    cdata: sanitizeValue(payload.cdata),
    token: sanitizeValue(payload.token),
    widgetId: sanitizeValue(payload.widgetId),
    retry: sanitizeValue(payload.retry),
    execution: sanitizeValue(payload.execution)
  });
});

chrome.runtime.onMessage.addListener((message, _sender, sendResponse) => {
  if (message?.type === "scanTurnstileNow") {
    scanDomForTurnstile();
    sendResponse({ ok: true });
  }
});

injectPageHook();
scanDomForTurnstile();

const observer = new MutationObserver(() => {
  scanDomForTurnstile();
});

observer.observe(document.documentElement || document.body, {
  childList: true,
  subtree: true,
  attributes: true,
  attributeFilter: ["data-sitekey", "data-action", "data-cdata", "src"]
});
