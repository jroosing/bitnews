// bitnews web UI: the only first-party script besides theme.js. It is a
// plain script (no build step) and must load BEFORE Alpine, because it
// registers everything in the "alpine:init" listener.
//
// Rules, because of the strict CSP (no unsafe-eval, no inline script):
//   * Alpine's CSP build only evaluates simple expressions, so templates
//     reference properties and methods of the components registered here.
//   * Data fetching and swapping stay with htmx; Alpine holds small client
//     state only (menus, dialogs, toasts, theme, tabs, clipboard).
(function () {
  "use strict";

  // ---- htmx wiring (works without Alpine) -------------------------------

  // Every htmx request carries the CSRF token from the <meta> tag.
  document.addEventListener("htmx:configRequest", function (evt) {
    var meta = document.querySelector('meta[name="csrf-token"]');
    if (meta && meta.content) {
      evt.detail.headers["X-CSRF-Token"] = meta.content;
    }
  });

  // htmx: a failed request without a server-provided toast gets a generic one.
  document.addEventListener("htmx:sendError", function () {
    toast({ level: "danger", message: "Could not reach the server. Check your connection." });
  });
  document.addEventListener("htmx:timeout", function () {
    toast({ level: "danger", message: "The request timed out." });
  });

  // HX-Trigger: {"toast": {"level": "success", "message": "Saved"}}
  document.addEventListener("toast", function (evt) {
    toast(evt.detail || {});
  });

  // hx-confirm opens the styled confirm dialog instead of window.confirm.
  // The trigger element may set data-confirm-label and
  // data-confirm-tone="danger".
  document.addEventListener("htmx:confirm", function (evt) {
    if (!evt.detail.question) {
      return;
    }
    var dlg = window.bitnewsConfirm;
    if (!dlg) {
      return; // no dialog on this page: fall back to window.confirm
    }
    evt.preventDefault();
    var elt = evt.detail.elt || {};
    var data = elt.dataset || {};
    dlg
      .ask(evt.detail.question, data.confirmLabel || "Confirm", data.confirmTone || "primary")
      .then(function (ok) {
        if (ok) {
          evt.detail.issueRequest(true);
        }
      });
  });

  var pendingToasts = [];

  function toast(t) {
    if (window.Alpine && window.Alpine.store("toasts")) {
      window.Alpine.store("toasts").add(t);
    } else {
      pendingToasts.push(t);
    }
  }

  // ---- Alpine components -------------------------------------------------

  document.addEventListener("alpine:init", function () {
    var Alpine = window.Alpine;

    // Theme: "system" | "light" | "dark", persisted by theme.js.
    Alpine.store("theme", {
      mode: window.bitnewsTheme ? window.bitnewsTheme.get() : "system",
      set: function (mode) {
        this.mode = mode;
        if (window.bitnewsTheme) {
          window.bitnewsTheme.set(mode);
        }
      },
    });

    // Toasts: an aria-live region renders this list.
    var nextToast = 1;
    var LEVELS = {
      success: { icon: "text-ok-fg", role: "status" },
      info: { icon: "text-info-fg", role: "status" },
      warn: { icon: "text-warn-fg", role: "alert" },
      danger: { icon: "text-bad-fg", role: "alert" },
    };
    Alpine.store("toasts", {
      items: [],
      add: function (t) {
        var level = LEVELS[t.level] ? t.level : "info";
        var id = nextToast++;
        var item = {
          id: id,
          level: level,
          message: String(t.message || ""),
          role: LEVELS[level].role,
          isSuccess: level === "success",
          isInfo: level === "info",
          isWarn: level === "warn",
          isDanger: level === "danger",
        };
        this.items.push(item);
        var self = this;
        setTimeout(function () {
          self.remove(id);
        }, level === "danger" ? 9000 : 5000);
      },
      remove: function (id) {
        this.items = this.items.filter(function (i) {
          return i.id !== id;
        });
      },
    });
    pendingToasts.splice(0).forEach(function (t) {
      Alpine.store("toasts").add(t);
    });

    // The app shell: the mobile slide-over navigation.
    Alpine.data("shell", function () {
      return {
        navOpen: false,
        openNav: function () {
          this.navOpen = true;
          var self = this;
          this.$nextTick(function () {
            var close = self.$refs.navClose;
            if (close) {
              close.focus();
            }
          });
        },
        closeNav: function () {
          this.navOpen = false;
        },
      };
    });

    // A dropdown menu (the user menu). Arrow keys move between the items.
    Alpine.data("menu", function () {
      return {
        open: false,
        items: function () {
          return Array.prototype.slice.call(this.$refs.panel.querySelectorAll('[role^="menuitem"]'));
        },
        toggle: function () {
          if (this.open) {
            this.close();
          } else {
            this.show();
          }
        },
        show: function () {
          this.open = true;
          var self = this;
          this.$nextTick(function () {
            var first = self.items()[0];
            if (first) {
              first.focus();
            }
          });
        },
        close: function () {
          if (!this.open) {
            return;
          }
          this.open = false;
          this.$refs.button.focus();
        },
        closeQuiet: function () {
          this.open = false;
        },
        onPanelKey: function (evt) {
          var items = this.items();
          var i = items.indexOf(document.activeElement);
          if (evt.key === "ArrowDown") {
            items[(i + 1) % items.length].focus();
          } else if (evt.key === "ArrowUp") {
            items[(i - 1 + items.length) % items.length].focus();
          } else if (evt.key === "Home") {
            items[0].focus();
          } else if (evt.key === "End") {
            items[items.length - 1].focus();
          } else if (evt.key === "Tab") {
            this.open = false;
            return;
          } else {
            return;
          }
          evt.preventDefault();
        },
        onButtonKey: function (evt) {
          if (evt.key === "ArrowDown") {
            evt.preventDefault();
            this.show();
          }
        },
      };
    });

    // Theme choice rows inside the user menu.
    Alpine.data("themeChoice", function () {
      return {
        isLight: function () {
          return Alpine.store("theme").mode === "light";
        },
        isDark: function () {
          return Alpine.store("theme").mode === "dark";
        },
        isSystem: function () {
          return Alpine.store("theme").mode === "system";
        },
        setLight: function () {
          Alpine.store("theme").set("light");
        },
        setDark: function () {
          Alpine.store("theme").set("dark");
        },
        setSystem: function () {
          Alpine.store("theme").set("system");
        },
      };
    });

    // A <dialog> that opens from a button: x-data="modal", x-ref="dlg".
    Alpine.data("modal", function () {
      return {
        show: function () {
          this.$refs.dlg.showModal();
        },
        hide: function () {
          this.$refs.dlg.close();
        },
        onBackdrop: function (evt) {
          if (evt.target === this.$refs.dlg) {
            this.$refs.dlg.close();
          }
        },
      };
    });

    // The global confirm dialog behind hx-confirm.
    Alpine.data("confirmDialog", function () {
      return {
        message: "",
        label: "Confirm",
        danger: false,
        resolver: null,
        get acceptClass() {
          return this.danger ? "btn-danger" : "btn-primary";
        },
        init: function () {
          var self = this;
          window.bitnewsConfirm = {
            ask: function (message, label, tone) {
              self.message = message;
              self.label = label;
              self.danger = tone === "danger";
              self.finish(false);
              return new Promise(function (resolve) {
                self.resolver = resolve;
                self.$refs.dlg.showModal();
              });
            },
          };
        },
        finish: function (ok) {
          var r = this.resolver;
          this.resolver = null;
          if (r) {
            r(ok);
          }
        },
        accept: function () {
          this.finish(true);
          this.$refs.dlg.close();
        },
        cancel: function () {
          this.$refs.dlg.close();
        },
        onClose: function () {
          this.finish(false);
        },
        onBackdrop: function (evt) {
          if (evt.target === this.$refs.dlg) {
            this.$refs.dlg.close();
          }
        },
      };
    });

    // Show/hide for a password input.
    Alpine.data("reveal", function () {
      return {
        shown: false,
        toggle: function () {
          this.shown = !this.shown;
        },
        get inputType() {
          return this.shown ? "text" : "password";
        },
        get label() {
          return this.shown ? "Hide password" : "Show password";
        },
      };
    });

    // Copy-to-clipboard: the button carries data-copy="text to copy".
    Alpine.data("clipboard", function () {
      return {
        copied: false,
        copy: function (evt) {
          var text = evt.currentTarget.dataset.copy || "";
          var self = this;
          var done = function () {
            self.copied = true;
            setTimeout(function () {
              self.copied = false;
            }, 2000);
          };
          if (navigator.clipboard && navigator.clipboard.writeText) {
            navigator.clipboard.writeText(text).then(done, function () {});
          }
        },
      };
    });

    // Tabs: tab buttons carry data-tab="id", panels use isTab* checks via
    // the panel's own data-panel attribute.
    Alpine.data("tabs", function () {
      return {
        active: "",
        init: function () {
          var first = this.$el.querySelector("[data-tab]");
          if (first && !this.active) {
            this.active = first.dataset.tab;
          }
        },
        select: function (evt) {
          this.active = evt.currentTarget.dataset.tab;
        },
        isActive: function (id) {
          return this.active === id;
        },
      };
    });

    // The style guide's buttons that raise a toast: data-level="success".
    Alpine.data("toastDemo", function () {
      return {
        fire: function (evt) {
          var level = evt.currentTarget.dataset.level || "info";
          Alpine.store("toasts").add({ level: level, message: "This is a " + level + " toast." });
        },
      };
    });

    // The toast list in the layout.
    Alpine.data("toastRegion", function () {
      return {
        get items() {
          return Alpine.store("toasts").items;
        },
        dismiss: function (evt) {
          var id = Number(evt.currentTarget.dataset.id);
          Alpine.store("toasts").remove(id);
        },
      };
    });
  });
})();
