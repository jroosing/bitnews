// Scripts of the History, Download detail and "New download" pieces of the
// UI. Like app.js it is a plain script that must load BEFORE Alpine, and it
// only registers components in "alpine:init". Templates reference the
// properties and methods below; no data is ever put into an Alpine
// expression.
(function () {
  "use strict";

  // The top bar button opens the "New download" dialog: a window event that
  // both the dialog (Alpine) and its form loader (htmx) listen for.
  document.addEventListener("click", function (evt) {
    var target = evt.target;
    var btn = target && target.closest ? target.closest("[data-add-open]") : null;
    if (btn) {
      evt.preventDefault();
      window.dispatchEvent(new CustomEvent("add-open"));
    }
  });

  // The server answers a refused add with a status of 400 or more and the
  // form with its problems as the body: let htmx swap it in. Other errors
  // (an empty body) keep htmx's default of showing a toast.
  document.addEventListener("htmx:beforeSwap", function (evt) {
    var d = evt.detail;
    if (!d || !d.target || d.target.id !== "add-dialog-body" || !d.xhr) {
      return;
    }
    var status = d.xhr.status;
    if (status >= 400 && status < 600 && d.xhr.responseText) {
      d.shouldSwap = true;
      d.isError = false;
    }
  });

  document.addEventListener("alpine:init", function () {
    var Alpine = window.Alpine;

    // The dialog around the form.
    Alpine.data("addDialog", function () {
      return {
        open: function () {
          var dlg = this.$refs.dlg;
          if (!dlg.open) {
            dlg.showModal();
          }
        },
        close: function () {
          var dlg = this.$refs.dlg;
          if (dlg.open) {
            dlg.close();
          }
        },
        onBackdrop: function (evt) {
          if (evt.target === this.$refs.dlg) {
            this.close();
          }
        },
      };
    });

    // The form: the file/link tabs, the drop zone and the upload progress.
    Alpine.data("addForm", function () {
      return {
        mode: "file",
        dragging: false,
        fileName: "",
        fileError: "",
        hint: "",
        maxBytes: 0,
        maxTorrentBytes: 0,
        uploading: false,
        progress: 0,
        init: function () {
          var data = this.$el.dataset;
          this.mode = data.mode === "link" ? "link" : "file";
          this.maxBytes = Number(data.maxBytes) || 0;
          this.maxTorrentBytes = Number(data.maxTorrentBytes) || 0;
          this.hint = data.hint || "";
          var self = this;
          this.$nextTick(function () {
            var target = null;
            if (self.$el.dataset.hasError) {
              target = document.getElementById("add-error");
              if (!target) {
                target = self.mode === "link" ? document.getElementById("add-link") : self.$refs.tabFile;
              }
            } else {
              target = self.mode === "link" ? self.$refs.tabLink : self.$refs.tabFile;
            }
            if (target && target.focus) {
              target.focus();
            }
          });
        },
        get isFile() {
          return this.mode === "file";
        },
        get isLink() {
          return this.mode === "link";
        },
        get fileTabIndex() {
          return this.mode === "file" ? 0 : -1;
        },
        get linkTabIndex() {
          return this.mode === "link" ? 0 : -1;
        },
        get hasFile() {
          return this.fileName !== "";
        },
        get zoneClass() {
          return this.dragging ? "dropzone-active" : "";
        },
        get progressText() {
          return this.progress + "%";
        },
        showFile: function () {
          this.mode = "file";
        },
        showLink: function () {
          this.mode = "link";
        },
        onTabKey: function (evt) {
          var key = evt.key;
          if (key !== "ArrowLeft" && key !== "ArrowRight" && key !== "Home" && key !== "End") {
            return;
          }
          evt.preventDefault();
          var toLink = key === "ArrowRight" || key === "End";
          this.mode = toLink ? "link" : "file";
          var self = this;
          this.$nextTick(function () {
            (toLink ? self.$refs.tabLink : self.$refs.tabFile).focus();
          });
        },
        dragOver: function (evt) {
          this.dragging = true;
          if (evt.dataTransfer) {
            evt.dataTransfer.dropEffect = "copy";
          }
        },
        dragLeave: function (evt) {
          var zone = this.$refs.zone;
          if (evt.relatedTarget && zone && zone.contains(evt.relatedTarget)) {
            return;
          }
          this.dragging = false;
        },
        // A file dropped on the zone goes into the file input, so the form
        // posts it like a chosen one.
        dropped: function (evt) {
          this.dragging = false;
          var files = evt.dataTransfer && evt.dataTransfer.files;
          if (!files || files.length === 0) {
            return;
          }
          var input = this.$refs.file;
          try {
            var transfer = new DataTransfer();
            transfer.items.add(files[0]);
            input.files = transfer.files;
          } catch (e) {
            this.fileError = "Your browser cannot take a dropped file. Use “choose one” instead.";
            return;
          }
          this.check(input.files);
        },
        picked: function (evt) {
          this.check(evt.target.files);
        },
        check: function (files) {
          this.fileError = "";
          this.fileName = "";
          if (!files || files.length === 0) {
            return;
          }
          var file = files[0];
          var name = file.name || "";
          var lower = name.toLowerCase();
          var limit = lower.slice(-8) === ".torrent" ? this.maxTorrentBytes : this.maxBytes;
          if (limit > 0 && file.size > limit) {
            this.fileError = "That file is too large: " + megabytes(file.size) + ". The limit is " + megabytes(limit) + ".";
            this.$refs.file.value = "";
            return;
          }
          if (file.size === 0) {
            this.fileError = "That file is empty.";
            this.$refs.file.value = "";
            return;
          }
          this.fileName = name;
        },
        started: function () {
          this.uploading = true;
          this.progress = 0;
        },
        onProgress: function (evt) {
          var d = evt.detail || {};
          if (d.lengthComputable && d.total > 0) {
            this.progress = Math.min(100, Math.round((d.loaded / d.total) * 100));
          }
        },
        finished: function () {
          this.uploading = false;
        },
        closeDialog: function () {
          var dlg = document.getElementById("add-dialog");
          if (dlg && dlg.open) {
            dlg.close();
          }
        },
      };
    });

    // The share limit form: the inputs are only live for a custom limit.
    Alpine.data("shareLimit", function () {
      return {
        custom: false,
        init: function () {
          this.custom = !!this.$el.dataset.custom;
        },
        get isDefault() {
          return !this.custom;
        },
        useDefault: function () {
          this.custom = false;
        },
        useCustom: function () {
          this.custom = true;
        },
      };
    });
  });

  function megabytes(n) {
    return (n / (1024 * 1024)).toFixed(1) + " MB";
  }
})();
