function b64utf8(s) {
  const bytes = new TextEncoder().encode(s);
  let bin = "";
  const chunk = 0x8000;
  for (let i = 0; i < bytes.length; i += chunk) {
    bin += String.fromCharCode(...bytes.subarray(i, i + chunk));
  }
  return btoa(bin);
}

chrome.action.onClicked.addListener(async (tab) => {
  try {
    const [{ result }] = await chrome.scripting.executeScript({
      target: { tabId: tab.id },
      world: "MAIN",
      func: () => {
        // Grab known page-JS data stores (jqGrid feeds etc.) that the DOM may not fully render.
        const jsData = {};
        for (const name of ["data_", "CHART_FULL_DATA"]) {
          try {
            const v = window[name];
            if (v !== undefined && v !== null && v !== "") {
              jsData[name] = JSON.parse(JSON.stringify(v));
            }
          } catch (e) {}
        }
        return {
          url: location.href,
          title: document.title,
          html: document.documentElement.outerHTML,
          jsData,
        };
      },
    });
    const stamp = new Date().toISOString().replace(/[:.]/g, "-").slice(0, 19);
    const slug = (result.title || "page")
      .replace(/[^\wㄱ-힝-]+/g, "_")
      .replace(/^_+|_+$/g, "")
      .slice(0, 60) || "page";
    const meta = `<!-- GiveItToClaude | ${result.url} | saved ${stamp} -->\n`;
    const jsDump = Object.keys(result.jsData || {}).length
      ? `<script type="application/x-giveittoclaude-jsdata">${JSON.stringify(result.jsData)}</script>\n`
      : "";
    await chrome.downloads.download({
      url: "data:text/html;charset=utf-8;base64," + b64utf8(meta + jsDump + result.html),
      filename: `GiveItToClaude/${stamp}_${slug}.html`,
      saveAs: false,
      conflictAction: "uniquify",
    });
    chrome.action.setBadgeBackgroundColor({ color: "#2e7d32", tabId: tab.id });
    chrome.action.setBadgeText({ text: "OK", tabId: tab.id });
  } catch (e) {
    chrome.action.setBadgeBackgroundColor({ color: "#c62828", tabId: tab.id });
    chrome.action.setBadgeText({ text: "ERR", tabId: tab.id });
  }
  setTimeout(() => chrome.action.setBadgeText({ text: "", tabId: tab.id }), 3000);
});
