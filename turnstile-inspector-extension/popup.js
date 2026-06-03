const statusEl = document.getElementById("status");
const listEl = document.getElementById("list");
const refreshBtn = document.getElementById("refreshBtn");
const copyBtn = document.getElementById("copyBtn");

let activeTabId = null;
let currentCaptures = [];

function formatTime(ts) {
  if (!ts) return "";
  return new Date(ts).toLocaleTimeString();
}

function escapeHtml(value) {
  return String(value || "")
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;")
    .replaceAll("'", "&#39;");
}

function renderCaptures(captures) {
  currentCaptures = captures || [];
  if (!currentCaptures.length) {
    statusEl.textContent = "No Turnstile values captured yet on this tab.";
    listEl.innerHTML = "";
    return;
  }

  statusEl.textContent = `Captured ${currentCaptures.length} item(s).`;
  listEl.innerHTML = currentCaptures
    .map((item, index) => {
      const sitekey = escapeHtml(item.sitekey || "");
      const action = escapeHtml(item.action || "");
      const cdata = escapeHtml(item.cdata || "");
      const source = escapeHtml(item.source || "");
      const url = escapeHtml(item.url || "");
      const token = escapeHtml(item.token || "");
      const seenCount = Number(item.seenCount || 1);
      return `
        <article class="card">
          <div class="row"><div class="key">#${index + 1} Source</div><div class="value">${source}</div></div>
          <div class="row"><div class="key">Sitekey</div><div class="value">${sitekey || "-"}</div></div>
          <div class="row"><div class="key">Action</div><div class="value">${action || "-"}</div></div>
          <div class="row"><div class="key">CData</div><div class="value">${cdata || "-"}</div></div>
          <div class="row"><div class="key">Token</div><div class="value">${token || "-"}</div></div>
          <div class="row"><div class="key">URL</div><div class="value">${url || "-"}</div></div>
          <div class="row"><div class="key">Seen</div><div class="value">${seenCount} time(s), last at ${formatTime(item.capturedAt)}</div></div>
        </article>
      `;
    })
    .join("");
}

async function getActiveTab() {
  const tabs = await chrome.tabs.query({ active: true, currentWindow: true });
  return tabs[0] || null;
}

async function requestTabScan(tabId) {
  try {
    await chrome.tabs.sendMessage(tabId, { type: "scanTurnstileNow" });
  } catch (_err) {
    // Tab may not have injected script yet.
  }
}

async function loadCaptures() {
  const tab = await getActiveTab();
  if (!tab?.id) {
    statusEl.textContent = "Unable to detect active tab.";
    return;
  }

  activeTabId = tab.id;
  await requestTabScan(activeTabId);

  const response = await chrome.runtime.sendMessage({
    type: "getCapturesForTab",
    tabId: activeTabId
  });

  renderCaptures(response?.captures || []);
}

async function copyLatest() {
  if (!currentCaptures.length) {
    statusEl.textContent = "Nothing to copy yet.";
    return;
  }

  const latest = currentCaptures[0];
  const payload = {
    url: latest.url || "",
    sitekey: latest.sitekey || "",
    action: latest.action || "",
    cdata: latest.cdata || "",
    source: latest.source || ""
  };

  await navigator.clipboard.writeText(JSON.stringify(payload, null, 2));
  statusEl.textContent = "Latest capture copied to clipboard.";
}

refreshBtn.addEventListener("click", () => {
  loadCaptures();
});

copyBtn.addEventListener("click", () => {
  copyLatest();
});

loadCaptures();
