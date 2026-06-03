const tabCaptures = new Map();
const MAX_ITEMS_PER_TAB = 100;

function captureSignature(capture) {
  return [
    capture.source || "",
    capture.url || "",
    capture.sitekey || "",
    capture.action || "",
    capture.cdata || "",
    capture.widgetId || "",
    capture.token || ""
  ].join("|");
}

function upsertCapture(tabId, capture) {
  const current = tabCaptures.get(tabId) || [];
  const sig = captureSignature(capture);
  const existingIndex = current.findIndex((item) => captureSignature(item) === sig);

  if (existingIndex >= 0) {
    current[existingIndex] = {
      ...current[existingIndex],
      ...capture,
      seenCount: (current[existingIndex].seenCount || 1) + 1
    };
  } else {
    current.unshift({
      ...capture,
      seenCount: 1
    });
    if (current.length > MAX_ITEMS_PER_TAB) {
      current.length = MAX_ITEMS_PER_TAB;
    }
  }

  tabCaptures.set(tabId, current);
}

chrome.runtime.onMessage.addListener((message, sender, sendResponse) => {
  if (message?.type === "turnstileCapture") {
    const tabId = sender?.tab?.id;
    if (typeof tabId === "number") {
      upsertCapture(tabId, {
        ...message.payload,
        capturedAt: Date.now()
      });
    }
    sendResponse({ ok: true });
    return;
  }

  if (message?.type === "getCapturesForTab") {
    const tabId = Number(message.tabId);
    sendResponse({
      captures: Number.isNaN(tabId) ? [] : (tabCaptures.get(tabId) || [])
    });
    return;
  }
});

chrome.tabs.onRemoved.addListener((tabId) => {
  tabCaptures.delete(tabId);
});
