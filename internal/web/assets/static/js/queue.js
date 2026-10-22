// The queue page's client code. A plain script that loads before Alpine (like
// app.js) and registers its components in "alpine:init". Strict CSP: no
// inline script, so templates only reference what is registered here.
(function () {
  "use strict";

  // The live stream (/ui/events) does not replay events, so whenever it
  // opens (the first time, after a reconnect, after a resync) the queue
  // lists are refetched. The lists element listens for "sse:refresh".
  document.addEventListener("htmx:sseOpen", function () {
    var lists = document.getElementById("queue-lists");
    if (lists && window.htmx) {
      window.htmx.trigger(lists, "sse:refresh");
    }
  });

  // The server ends a stream that missed events with a "resync" event. The
  // browser then reconnects by itself, and the open handler above refetches.

  document.addEventListener("alpine:init", function () {
    var Alpine = window.Alpine;

    // A popover with a form (the speed limit). Escape and a click outside
    // close it; opening focuses the first field.
    Alpine.data("popover", function () {
      return {
        open: false,
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
            var field = self.$refs.panel.querySelector("input, select, button");
            if (field) {
              field.focus();
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
      };
    });
  });
})();
