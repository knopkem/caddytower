(() => {
  const body = document.body;
  if (!body) {
    return;
  }

  const setLoader = (active) => {
    document.getElementById("page-loader")?.classList.toggle("is-active", active);
  };

  const setSectionState = (section, active) => {
    section.classList.toggle("hidden", !active);
    section.querySelectorAll("input, select, textarea").forEach((field) => {
      field.disabled = !active;
    });
  };

  const initProjectTypeForms = (root) => {
    root.querySelectorAll("[data-project-type-form]").forEach((form) => {
      const select = form.querySelector("[data-project-type]");
      const webSection = form.querySelector("[data-web-fields]");
      const networkSection = form.querySelector("[data-network-fields]");
      if (!select || !webSection || !networkSection) {
        return;
      }

      const sync = () => {
        const isWeb = select.value === "web";
        setSectionState(webSection, isWeb);
        setSectionState(networkSection, !isWeb);
      };

      select.addEventListener("change", sync);
      sync();
    });
  };

  const initDialogs = (root) => {
    root.querySelectorAll("[data-dialog-open]").forEach((button) => {
      button.addEventListener("click", () => {
        const dialog = document.getElementById(button.getAttribute("data-dialog-open"));
        if (dialog && typeof dialog.showModal === "function") {
          dialog.showModal();
        }
      });
    });

    root.querySelectorAll("dialog").forEach((dialog) => {
      dialog.querySelectorAll("[data-dialog-close]").forEach((button) => {
        button.addEventListener("click", () => dialog.close());
      });
      dialog.addEventListener("click", (event) => {
        if (event.target === dialog) {
          dialog.close();
        }
      });
    });
  };

  const disconnectLogStream = (target) => {
    if (!target?._eventSource) {
      return;
    }
    target._eventSource.close();
    delete target._eventSource;
    delete target.dataset.streamConnected;
  };

  const connectLogStream = (target) => {
    if (!target || target.dataset.streamConnected === "true") {
      return;
    }

    const url = target.getAttribute("data-logs-url");
    if (!url) {
      return;
    }

    target.textContent = "";
    target.dataset.streamConnected = "true";

    const source = new EventSource(url);
    target._eventSource = source;

    source.onmessage = (event) => {
      target.textContent += `${target.textContent ? "\n" : ""}${event.data}`;
      target.scrollTop = target.scrollHeight;
    };

    source.onerror = () => {
      if (target.textContent.length === 0) {
        target.textContent = "Log stream unavailable.";
      }
      disconnectLogStream(target);
    };
  };

  const initLogStreams = (root) => {
    root.querySelectorAll("[data-log-stream]").forEach(connectLogStream);
  };

  const disconnectLogStreams = (root) => {
    root.querySelectorAll("[data-log-stream]").forEach(disconnectLogStream);
  };

  const init = (root) => {
    initDialogs(root);
    initProjectTypeForms(root);
    initLogStreams(root);
  };

  document.addEventListener("DOMContentLoaded", () => init(document));

  body.addEventListener("htmx:beforeRequest", () => setLoader(true));
  body.addEventListener("htmx:afterRequest", () => setLoader(false));
  body.addEventListener("htmx:responseError", () => setLoader(false));
  body.addEventListener("htmx:beforeSwap", () => disconnectLogStreams(document));
  body.addEventListener("htmx:load", (event) => init(event.detail.elt || document));
})();
