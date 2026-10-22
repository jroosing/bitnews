// bitnews web UI: Alpine components of the Settings pages. Loaded after
// app.js and before Alpine, like app.js; see the CSP rules in README.md.
(function () {
  "use strict";

  // A server switch that could not be saved goes back to where it was.
  document.addEventListener("htmx:responseError", function (evt) {
    var elt = evt.detail && evt.detail.elt;
    if (elt && elt.hasAttribute && elt.hasAttribute("data-revert-on-error")) {
      elt.checked = !elt.checked;
    }
  });
  document.addEventListener("htmx:sendError", function (evt) {
    var elt = evt.detail && evt.detail.elt;
    if (elt && elt.hasAttribute && elt.hasAttribute("data-revert-on-error")) {
      elt.checked = !elt.checked;
    }
  });

  document.addEventListener("alpine:init", function () {
    var Alpine = window.Alpine;

    // The section navigation is a scrolling pill bar on phones: bring the
    // current section into view.
    Alpine.data("settingsNav", function () {
      return {
        init: function () {
          var nav = this.$el;
          var cur = nav.querySelector('[aria-current="page"]');
          if (cur && nav.scrollWidth > nav.clientWidth) {
            nav.scrollLeft = cur.offsetLeft - (nav.clientWidth - cur.offsetWidth) / 2;
          }
        },
      };
    });

    // The settings form pattern: Save stays disabled until a field changes.
    // A form re-rendered after a failed save starts dirty (data-dirty).
    // The form is swapped out by htmx after a save, which resets the state.
    Alpine.data("settingsForm", function () {
      return {
        dirty: false,
        init: function () {
          this.dirty = this.$el.dataset.dirty === "true";
        },
        touch: function () {
          this.dirty = true;
        },
      };
    });

    // The "add category" form: a live hint for the name while typing.
    var CATEGORY_NAME = /^[a-z0-9._-]{1,64}$/;
    Alpine.data("categoryForm", function () {
      return {
        name: "",
        checked: false,
        init: function () {
          // A name that came back from the server has its own error text, so
          // the live hint waits until the user types.
          this.name = this.$el.querySelector("#category-name").value;
        },
        check: function () {
          this.checked = true;
        },
        get invalid() {
          if (!this.checked || this.name === "") {
            return false;
          }
          return !CATEGORY_NAME.test(this.name) || this.name === "." || this.name === "..";
        },
        get hint() {
          if (this.name.length > 64) {
            return "At most 64 characters.";
          }
          if (this.name === "." || this.name === "..") {
            return "A name of only dots is not allowed.";
          }
          return "Only lowercase letters, digits, dots, underscores and hyphens.";
        },
      };
    });

    // The add / edit server dialog. htmx loads the form into the body; the
    // dialog opens when it arrives and closes on the "dialog-close" event
    // that the server sends after a successful save.
    Alpine.data("serverDialog", function () {
      return {
        show: function () {
          var dlg = this.$refs.dlg;
          if (!dlg.open) {
            dlg.showModal();
          }
        },
        hide: function () {
          var dlg = this.$refs.dlg;
          if (dlg.open) {
            dlg.close();
          }
        },
        onSwap: function (evt) {
          var target = evt.detail && evt.detail.target;
          if (target && target.id === "server-dialog-body") {
            this.show();
          }
        },
        onClose: function () {
          this.$refs.body.replaceChildren();
        },
        onBackdrop: function (evt) {
          if (evt.target === this.$refs.dlg) {
            this.hide();
          }
        },
      };
    });

    // The form inside the dialog: an old test result is wrong as soon as a
    // field changes.
    Alpine.data("serverForm", function () {
      return {
        clearResult: function () {
          this.$refs.result.replaceChildren();
        },
      };
    });
  });
})();
