(() => {
  const body = document.body;
  if (!body) {
    return;
  }

  const setLoader = (active) => {
    document.getElementById("page-loader")?.classList.toggle("is-active", active);
  };

  const queryAll = (root, selector) => {
    const matches = [];
    if (root instanceof Element && root.matches(selector)) {
      matches.push(root);
    }
    return matches.concat(Array.from(root.querySelectorAll(selector)));
  };

  const setSectionState = (section, active) => {
    section.classList.toggle("hidden", !active);
    section.querySelectorAll("input, select, textarea").forEach((field) => {
      field.disabled = !active;
    });
  };

  const initProjectTypeForms = (root) => {
    queryAll(root, "[data-project-type-form]").forEach((form) => {
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

  const initSetupPreview = (root) => {
    queryAll(root, "[data-setup-preview-form]").forEach((form) => {
      if (form.dataset.setupPreviewBound === "true") {
        return;
      }
      form.dataset.setupPreviewBound = "true";

      const emailInput = form.querySelector("[data-setup-email]");
      const secretInput = form.querySelector("[data-setup-secret]");
      const previewURL = form.getAttribute("data-setup-preview-url");
      const scope = form.closest(".auth-setup-grid") || root;
      const qrImage = scope.querySelector("[data-setup-qr]");
      const otpAuthValue = scope.querySelector("[data-setup-otpauth]");
      if (!emailInput || !secretInput || !previewURL || !qrImage || !otpAuthValue) {
        return;
      }

      const originalEmail = emailInput.value.trim().toLowerCase();
      const originalQR = qrImage.getAttribute("src") || "";
      const originalOTPAuth = otpAuthValue.textContent || "";
      let controller;
      let debounceTimer;
      let lastRequestedEmail = originalEmail;

      const restoreOriginal = () => {
        qrImage.setAttribute("src", originalQR);
        otpAuthValue.textContent = originalOTPAuth;
        lastRequestedEmail = originalEmail;
      };

      const runPreview = () => {
        const email = emailInput.value.trim().toLowerCase();
        const secret = secretInput.value.trim();
        if (!secret) {
          return;
        }
        if (!email || email === originalEmail) {
          controller?.abort();
          restoreOriginal();
          return;
        }
        if (email === lastRequestedEmail) {
          return;
        }

        controller?.abort();
        controller = new AbortController();
        lastRequestedEmail = email;

        fetch(`${previewURL}?email=${encodeURIComponent(email)}&secret=${encodeURIComponent(secret)}`, {
          method: "GET",
          headers: { Accept: "application/json" },
          credentials: "same-origin",
          signal: controller.signal,
        })
          .then(async (response) => {
            if (!response.ok) {
              throw new Error("preview request failed");
            }
            const payload = await response.json();
            if (emailInput.value.trim().toLowerCase() !== email) {
              return;
            }
            if (payload.qr_code_data_url) {
              qrImage.setAttribute("src", payload.qr_code_data_url);
            }
            if (payload.otp_auth_url) {
              otpAuthValue.textContent = payload.otp_auth_url;
            }
          })
          .catch((error) => {
            if (error?.name === "AbortError") {
              return;
            }
            lastRequestedEmail = "";
          });
      };

      const queuePreview = () => {
        clearTimeout(debounceTimer);
        debounceTimer = window.setTimeout(runPreview, 180);
      };

      emailInput.addEventListener("input", queuePreview);
      emailInput.addEventListener("change", runPreview);
    });
  };

  const initDialogs = (root) => {
    queryAll(root, "[data-dialog-open]").forEach((button) => {
      button.addEventListener("click", () => {
        const dialog = document.getElementById(button.getAttribute("data-dialog-open"));
        if (dialog && typeof dialog.showModal === "function") {
          dialog.showModal();
        }
      });
    });

    queryAll(root, "dialog").forEach((dialog) => {
      dialog.querySelectorAll("[data-dialog-close]").forEach((button) => {
        button.addEventListener("click", () => dialog.close());
      });
      dialog.addEventListener("click", (event) => {
        if (event.target === dialog) {
          dialog.close();
        }
      });
      if (dialog.dataset.dialogAutoOpen === "true" && dialog.dataset.autoOpened !== "true" && typeof dialog.showModal === "function") {
        dialog.dataset.autoOpened = "true";
        dialog.showModal();
      }
    });
  };

  const initConfirmDialogs = (root) => {
    queryAll(root, "form[data-confirm-dialog]").forEach((form) => {
      if (form.dataset.confirmBound === "true") {
        return;
      }
      form.dataset.confirmBound = "true";

      const dialogId = form.getAttribute("data-confirm-dialog");
      const dialog = dialogId ? document.getElementById(dialogId) : null;
      if (!dialog || typeof dialog.showModal !== "function") {
        return;
      }

      if (dialog.dataset.confirmDialogBound !== "true") {
        dialog.dataset.confirmDialogBound = "true";
        const title = dialog.querySelector("[data-confirm-title]");
        const bodyText = dialog.querySelector("[data-confirm-body]");
        const confirmButton = dialog.querySelector("[data-confirm-submit]");
        const cancelButtons = dialog.querySelectorAll("[data-confirm-cancel]");
        const defaultConfirmLabel = confirmButton?.textContent || "Continue";

        const reset = () => {
          dialog._confirmForm = null;
          if (confirmButton) {
            confirmButton.disabled = false;
            confirmButton.textContent = defaultConfirmLabel;
          }
        };

        cancelButtons.forEach((button) => {
          button.addEventListener("click", () => {
            dialog.dataset.keepState = "";
            dialog.close();
          });
        });
        dialog.addEventListener("click", (event) => {
          if (event.target === dialog) {
            dialog.dataset.keepState = "";
            dialog.close();
          }
        });
        dialog.addEventListener("close", () => {
          if (dialog.dataset.keepState === "true") {
            dialog.dataset.keepState = "";
            return;
          }
          reset();
        });
        confirmButton?.addEventListener("click", () => {
          const activeForm = dialog._confirmForm;
          if (!activeForm) {
            dialog.close();
            return;
          }
          const progressLabel = activeForm.getAttribute("data-confirm-progress-label") || defaultConfirmLabel;
          dialog.dataset.keepState = "true";
          confirmButton.disabled = true;
          confirmButton.textContent = progressLabel;
          setLoader(true);
          dialog.close();
          activeForm.dataset.confirmArmed = "true";
          activeForm.requestSubmit();
        });

        dialog._confirmPopulate = (activeForm) => {
          dialog._confirmForm = activeForm;
          if (title) {
            title.textContent = activeForm.getAttribute("data-confirm-title") || "Are you sure?";
          }
          if (bodyText) {
            bodyText.textContent = activeForm.getAttribute("data-confirm-body") || "This action cannot be undone.";
          }
          if (confirmButton) {
            confirmButton.disabled = false;
            confirmButton.textContent = activeForm.getAttribute("data-confirm-confirm-label") || defaultConfirmLabel;
          }
        };
      }

      form.addEventListener("submit", (event) => {
        if (form.dataset.confirmArmed === "true") {
          delete form.dataset.confirmArmed;
          return;
        }
        event.preventDefault();
        dialog._confirmPopulate?.(form);
        dialog.showModal();
      });
    });
  };

  const disconnectStream = (target) => {
    if (!target?._eventSource) {
      return;
    }
    target._eventSource.close();
    delete target._eventSource;
    delete target.dataset.streamConnected;
  };

  const connectStream = (target, urlAttribute, unavailableMessage) => {
    if (!target || target.dataset.streamConnected === "true") {
      return;
    }

    const url = target.getAttribute(urlAttribute);
    if (!url) {
      return;
    }

    target.textContent = target.getAttribute("data-empty-message") || "";
    target.dataset.streamConnected = "true";
    let hasRealMessage = false;

    const source = new EventSource(url);
    target._eventSource = source;

    source.onmessage = (event) => {
      let line = event.data;
      try {
        const payload = JSON.parse(event.data);
        if (payload?.message) {
          line = payload.stage ? `[${payload.stage}] ${payload.message}` : payload.message;
        }
      } catch {}
      if (!hasRealMessage) {
        target.textContent = "";
        hasRealMessage = true;
      }
      target.textContent += `${target.textContent ? "\n" : ""}${line}`;
      target.scrollTop = target.scrollHeight;
    };

    source.onerror = () => {
      if (target.textContent.length === 0) {
        target.textContent = unavailableMessage;
      }
      disconnectStream(target);
    };
  };

  const initLogStreams = (root) => {
    queryAll(root, "[data-log-stream]").forEach((target) => connectStream(target, "data-logs-url", "Log stream unavailable."));
    queryAll(root, "[data-event-stream]").forEach((target) => connectStream(target, "data-events-url", "Deploy event stream unavailable."));
  };

  const disconnectLogStreams = (root) => {
    queryAll(root, "[data-log-stream]").forEach(disconnectStream);
    queryAll(root, "[data-event-stream]").forEach(disconnectStream);
  };

  const initWizards = (root) => {
    queryAll(root, "[data-wizard]").forEach((wizard) => {
      if (wizard.dataset.wizardBound === "true") {
        return;
      }
      wizard.dataset.wizardBound = "true";

      const steps = Array.from(wizard.querySelectorAll("[data-wizard-step]"));
      const indicators = Array.from(wizard.querySelectorAll("[data-wizard-indicator]"));
      if (steps.length === 0) {
        return;
      }

      let index = 0;
      const sync = () => {
        steps.forEach((step, stepIndex) => {
          step.classList.toggle("hidden", stepIndex !== index);
        });
        indicators.forEach((indicator, indicatorIndex) => {
          indicator.classList.toggle("is-active", indicatorIndex === index);
        });
      };

      wizard.querySelectorAll("[data-wizard-next]").forEach((button) => {
        button.addEventListener("click", () => {
          index = Math.min(index + 1, steps.length - 1);
          sync();
        });
      });
      wizard.querySelectorAll("[data-wizard-back]").forEach((button) => {
        button.addEventListener("click", () => {
          index = Math.max(index - 1, 0);
          sync();
        });
      });
      sync();
    });
  };

  const initImageChecks = (root) => {
    queryAll(root, "[data-image-check-form]").forEach((form) => {
      if (form.dataset.imageCheckBound === "true") {
        return;
      }
      form.dataset.imageCheckBound = "true";

      const input = form.querySelector('input[name="image_ref"]');
      const status = form.querySelector("[data-image-check-status]");
      if (!input || !status) {
        return;
      }

      let controller;
      const setStatus = (message, state) => {
        status.textContent = message || "";
        status.classList.remove("is-ok", "is-error", "is-loading");
        if (state) {
          status.classList.add(`is-${state}`);
        }
      };

      const runCheck = () => {
        const value = input.value.trim();
        if (!value) {
          setStatus("", "");
          return;
        }

        controller?.abort();
        controller = new AbortController();
        setStatus("Checking image access…", "loading");

        fetch(`/api/image-check?image_ref=${encodeURIComponent(value)}`, {
          method: "GET",
          headers: { Accept: "application/json" },
          credentials: "same-origin",
          signal: controller.signal,
        })
          .then(async (response) => {
            let payload = {};
            try {
              payload = await response.json();
            } catch {
              payload = {};
            }
            if (response.status === 401) {
              setStatus("Sign in again to verify image access.", "error");
              return;
            }
            setStatus(payload.message || "Unable to verify image access.", payload.ok ? "ok" : "error");
          })
          .catch((error) => {
            if (error?.name === "AbortError") {
              return;
            }
            setStatus("Unable to verify image access right now.", "error");
          });
      };

      input.addEventListener("blur", runCheck);
      input.addEventListener("change", runCheck);
      if (input.value.trim()) {
        runCheck();
      }
    });
  };

  const parseEnvLines = (raw) => {
    const rows = [];
    raw.split(/\r?\n/).forEach((line) => {
      const trimmed = line.trim();
      if (!trimmed || trimmed.startsWith("#")) {
        return;
      }
      const separator = trimmed.indexOf("=");
      if (separator === -1) {
        rows.push({ key: trimmed, value: "" });
        return;
      }
      rows.push({
        key: trimmed.slice(0, separator).trim(),
        value: trimmed.slice(separator + 1),
      });
    });
    return rows;
  };

  const serializeEnvRows = (rows) => rows
    .map(({ key, value }) => `${key.trim()}=${value}`)
    .filter((entry) => entry.trim() !== "=" && !entry.startsWith("="))
    .join("\n");

  const initEnvEditors = (root) => {
    queryAll(root, "[data-env-editor]").forEach((editor) => {
      if (editor.dataset.envEditorBound === "true") {
        return;
      }
      editor.dataset.envEditorBound = "true";

      const textarea = editor.querySelector("[data-env-editor-text]");
      const rowsHost = editor.querySelector("[data-env-editor-rows]");
      const addButton = editor.querySelector("[data-env-editor-add]");
      if (!textarea || !rowsHost || !addButton) {
        return;
      }

      const buildRow = ({ key = "", value = "" } = {}) => {
        const row = document.createElement("div");
        row.className = "env-editor-row";
        row.innerHTML = `
          <label class="field">
            <span>Key</span>
            <input type="text" data-env-key>
          </label>
          <label class="field">
            <span>Value</span>
            <input type="text" data-env-value>
          </label>
          <button type="button" class="secondary env-editor-remove" data-env-remove>Remove</button>
        `;
        row.querySelector("[data-env-key]").value = key;
        row.querySelector("[data-env-value]").value = value;
        row.querySelectorAll("input").forEach((input) => {
          input.addEventListener("input", syncTextareaFromRows);
        });
        row.querySelector("[data-env-remove]").addEventListener("click", () => {
          row.remove();
          ensureBlankRow();
          syncTextareaFromRows();
        });
        rowsHost.appendChild(row);
      };

      const ensureBlankRow = () => {
        if (rowsHost.children.length === 0) {
          buildRow();
        }
      };

      const syncTextareaFromRows = () => {
        const rows = Array.from(rowsHost.querySelectorAll(".env-editor-row")).map((row) => ({
          key: row.querySelector("[data-env-key]")?.value || "",
          value: row.querySelector("[data-env-value]")?.value || "",
        }));
        textarea.value = serializeEnvRows(rows);
      };

      const syncRowsFromTextarea = () => {
        rowsHost.innerHTML = "";
        const rows = parseEnvLines(textarea.value);
        if (rows.length === 0) {
          buildRow();
          return;
        }
        rows.forEach(buildRow);
      };

      addButton.addEventListener("click", () => {
        buildRow();
      });
      textarea.addEventListener("input", syncRowsFromTextarea);

      syncRowsFromTextarea();
    });
  };

  const init = (root) => {
    initDialogs(root);
    initConfirmDialogs(root);
    initProjectTypeForms(root);
    initSetupPreview(root);
    initLogStreams(root);
    initWizards(root);
    initImageChecks(root);
    initEnvEditors(root);
  };

  document.addEventListener("DOMContentLoaded", () => init(document));

  body.addEventListener("htmx:beforeRequest", () => setLoader(true));
  body.addEventListener("htmx:afterRequest", () => setLoader(false));
  body.addEventListener("htmx:responseError", () => setLoader(false));
  body.addEventListener("htmx:beforeSwap", () => disconnectLogStreams(document));
  body.addEventListener("htmx:load", (event) => init(event.detail.elt || document));
})();
