(() => {
  const target = document.querySelector("[data-log-stream]");
  if (!target) {
    return;
  }

  const url = target.getAttribute("data-logs-url");
  if (!url) {
    return;
  }

  target.textContent = "";
  const source = new EventSource(url);

  source.onmessage = (event) => {
    if (target.textContent.length > 0) {
      target.textContent += "\n";
    }
    target.textContent += event.data;
    target.scrollTop = target.scrollHeight;
  };

  source.onerror = () => {
    if (target.textContent.length === 0) {
      target.textContent = "Log stream unavailable.";
    }
    source.close();
  };
})();
