//! Checks and repairs a Download with the par2 program (par2cmdline-turbo
//! 1.5.0, ADR-0007; internal/postproc/par2). par2 does all the work, we wrap
//! it: build the command line, guard against hostile names, read its English
//! report, map its exit code, clean up what it leaves behind.
//!
//! One `par2 r` (through the `tool_types.Runner` seam, the only way a process
//! starts) in the Download's directory verifies, renames files that match a
//! target by content, rebuilds damaged and missing files and checks the
//! result, so there is no separate verify run:
//!
//!     par2 r -B<dir> -t<threads> -m<MB> -- <main.par2> @   (extra files on stdin)
//!
//!  - -B<dir> is the absolute Download directory: par2 only reads and writes
//!    below it ("Ignoring out of basepath source file").
//!  - -t is the CPU count (at most 16) and -m is 512 MB by default. par2's own
//!    default memory is half of the physical RAM, which a container limit does
//!    not change, so we set it; par2 works with -m1, more memory only means
//!    fewer passes over a big set. Unlike Go's GOMAXPROCS, std offers no
//!    cgroup CPU quota: the count is `std.Thread.getCpuCount` (the affinity
//!    mask on Linux, which a cpuset does shrink), so a CFS-quota-limited
//!    container runs more par2 threads than its quota. Set `threads` to pin it.
//!  - The main file is the smallest .par2 file of the directory (the index
//!    without recovery blocks). Every other .par2 file and every regular file
//!    in the top level of the directory that is not already a target name goes
//!    to par2 as an "extra file" through stdin (`@`), so a directory with many
//!    files cannot overflow the argument list. par2 only loads sibling volumes
//!    that share the main file's prefix; extras are how it also finds volumes
//!    with other names, and files whose names were obfuscated: it scans each
//!    extra file and matches it to a target by hash.
//!  - A PAR2 file is known by its content as well as its name: before par2
//!    runs, a top-level file that starts with the "PAR2\0PKT" magic but has no
//!    .par2 name (a fully obfuscated post) is renamed to <name>.par2, so the
//!    set is used and the cleanup stage removes it afterwards.
//!  - Never used: -p (purge deletes the .par2 files, so a crash between the
//!    repair and Ready would leave no set to check against), -N and -S (block
//!    skipping), -q (we read the report).
//!
//! Exit codes (par2cmdline-turbo 1.5.0; `par2 -h` does not list them):
//!
//!     0  nothing to repair, or repair complete -> has_par2, repaired_slices
//!     2  repair required but not possible      -> InsufficientRecovery (needed/available)
//!     4  insufficient critical data ("Main packet not found") -> has_par2 false
//!     5  repair failed                         -> RepairFailed
//!     1, 3, 6, 7, 8 and a kill                 -> Failed with the output tail
//!
//! The English text on stdout (LC_ALL=C is set by the runner) only gives the
//! counts (par2_output.zig). Every pattern is a whole line starting with a
//! fixed word, never with a file name.
//!
//! Backups: when par2 replaces a damaged target it renames the old file to
//! name.1 (name.2 if that exists). Left there they would be moved into the
//! library, because the cleanup stage only deletes .par2 and .nzb files and
//! archives. So after every successful run the regular files called
//! <target>.<digits> next to a target are deleted, unless that name is a
//! target itself. A repair that is interrupted leaves such a file and no
//! target or a partial one: the next repair hands the .N file to par2 as an
//! extra file, which uses it as a data source, so the repair can be run again.
//!
//! An obfuscated and damaged file is repaired into the target name and the
//! damaged copy stays under its odd name. par2 reports it (`File: "x" - found
//! 9 of 10 data blocks from "a.bin".`), and after a successful repair that
//! copy is deleted, provided its name is exactly one of the extra files we
//! listed.
//!
//! Hostile input: the names inside a PAR2 file are chosen by the uploader and
//! par2 writes files under them. Some par2 versions neutralise some of them,
//! not all, and they differ. So before par2 starts, par2_names.zig reads the
//! File Description packets of every .par2 file and refuses the whole set
//! (PathViolation) when any name is unsafe. That guard is the only place that
//! reads PAR2 data here; it is isolated so it can be dropped.
//!
//! Errors: `Diag.message` is Go's err.Error() text. Everything that is a plain
//! error in Go (a missing directory, par2 not startable, a failed rename) is
//! `error.Failed`. A cancelled repair may leave name.N files and half-written
//! targets; the next repair picks them up as data sources.
const std = @import("std");
const Allocator = std.mem.Allocator;
const Io = std.Io;
const ports = @import("postprocess_ports.zig");
const tool = @import("tool_types.zig");
const fsutil = @import("fsutil.zig");
const names = @import("par2_names.zig");
const output = @import("par2_output.zig");

const PortError = ports.PortError;
const Diag = ports.Diag;

/// What par2 may use for repair buffers (see the top of the file).
pub const default_memory_mb: u32 = 512;
/// Caps the default thread count.
pub const max_threads: u32 = 16;
/// How much of par2's output an error message may carry.
pub const output_tail = 400;
/// The longest ".N" backup suffix, in digits.
pub const max_backup_suffix_digits = 4;
/// Hard cap on the files looked at in the top level of one directory.
pub const max_entries = 100_000;

// par2 exit codes.
const exit_ok = 0;
const exit_repair_required = 1; // verify only: damaged, and repairable
const exit_repair_impossible = 2; // not enough recovery blocks
const exit_invalid_args = 3;
const exit_insufficient_crit = 4; // no usable main packet / critical packets
const exit_repair_failed = 5;
const exit_file_io = 6;
const exit_logic = 7;
const exit_memory = 8;
const exit_signal = -1; // killed (the runner reports -1)

/// Implements `ports.Repairer` with one run of `par2 r`. Lives as long as the
/// port is used; nothing in it changes after construction.
pub const Repairer = struct {
    runner: tool.Runner,
    /// The par2 program, from the runner's `locate`.
    path: []const u8,
    /// par2's -t. Zero means the CPU count, at most 16.
    threads: u32 = 0,
    /// par2's -m. Zero means `default_memory_mb`.
    memory_mb: u32 = 0,

    pub fn port(self: *Repairer) ports.Repairer {
        return .{ .ctx = self, .repair_fn = repairPort };
    }

    fn repairPort(ctx: *anyopaque, arena: Allocator, io: Io, dir: []const u8, diag: *Diag) PortError!ports.RepairOutcome {
        const self: *Repairer = @ptrCast(@alignCast(ctx));
        return self.repair(arena, io, dir, diag);
    }

    fn threadCount(self: Repairer) u32 {
        if (self.threads > 0) return self.threads;
        const n = std.Thread.getCpuCount() catch 1;
        return @intCast(@min(@max(n, 1), max_threads));
    }

    fn memoryMb(self: Repairer) u32 {
        return if (self.memory_mb > 0) self.memory_mb else default_memory_mb;
    }

    /// Checks the files in `dir` (top level, plus the subdirectories the set
    /// names) against its PAR2 set and repairs them in one run of `par2 r`.
    /// The damaged originals par2 keeps as name.1, name.2, ... and the damaged
    /// copies under obfuscated names are removed afterwards, also when nothing
    /// needed repairing (the leftovers of an interrupted repair).
    ///
    /// A directory without a usable PAR2 set (no PAR2 file, or none with a
    /// main packet) is not an error: it yields `has_par2` false. Strings in
    /// `diag` live in `arena`, as do the results of the runner and every
    /// temporary of this call: pass an arena per post-processing step.
    pub fn repair(self: *const Repairer, arena: Allocator, io: Io, dir: []const u8, diag: *Diag) PortError!ports.RepairOutcome {
        try io.checkCancel();
        const abs = try absolute(arena, io, dir, diag);
        var d = Io.Dir.cwd().openDir(io, abs, .{ .iterate = true }) catch |e| return ioFail(arena, diag, "open", abs, e);
        defer d.close(io);

        try adoptPar2Files(arena, io, d, diag);
        const si = (try inspect(arena, io, d, abs, diag)) orelse return .{};

        const res = try self.run(arena, io, si, diag);
        const out = try output.parse(arena, res.stdout);
        switch (res.exit_code) {
            exit_ok => {
                try removeBackups(arena, io, si, diag);
                try removePartials(arena, io, si, out.partials, diag);
                return .{ .has_par2 = true, .repaired_slices = out.will_use };
            },
            exit_repair_impossible => {
                var needed = out.damaged();
                if (!out.have_avail) needed = out.recoveryBlocks() + out.need_more;
                diag.needed = needed;
                diag.available = out.recoveryBlocks();
                diag.message = try print(arena, "not enough recovery data: need {d} blocks, have {d}", .{ diag.needed, diag.available });
                return error.InsufficientRecovery;
            },
            exit_repair_failed => {
                diag.message = try print(arena, "repair failed: {s}", .{try exitError(arena, "repair", res)});
                return error.RepairFailed;
            },
            exit_insufficient_crit => return .{}, // "Main packet not found": no usable set
            else => {
                diag.message = try exitError(arena, "repair", res);
                return error.Failed;
            },
        }
    }

    /// Starts `par2 r`.
    fn run(self: *const Repairer, arena: Allocator, io: Io, si: SetInfo, diag: *Diag) PortError!tool.Result {
        // -B keeps par2 to dir: it ignores files outside the basepath. Never
        // used: -N (block skipping), -p (purge: it would delete the .par2
        // files) or -S. The extra files go through stdin (`@`), so a
        // directory with many files cannot overflow the argument list.
        const args = try arena.dupe([]const u8, &.{
            "r",
            try print(arena, "-B{s}", .{si.dir}),
            try print(arena, "-t{d}", .{self.threadCount()}),
            try print(arena, "-m{d}", .{self.memoryMb()}),
            "--",
            si.main,
            "@",
        });
        var stdin: std.ArrayList(u8) = .empty;
        for (si.extra) |p| {
            try stdin.appendSlice(arena, p);
            try stdin.append(arena, '\n');
        }
        if (si.extra.len == 0) try stdin.append(arena, '\n');
        return self.runner.run(arena, io, .{
            .path = self.path,
            .args = args,
            .dir = si.dir,
            .stdin = stdin.items,
        }) catch |e| switch (e) {
            error.Canceled => return error.Canceled,
            error.OutOfMemory => return error.OutOfMemory,
            error.StartFailed => {
                diag.message = try print(arena, "tool: start {s}", .{fsutil.base(self.path)});
                return error.Failed;
            },
            error.Timeout => {
                diag.message = try print(arena, "tool: timed out: {s}", .{fsutil.base(self.path)});
                return error.Failed;
            },
        };
    }
};

// ---- helpers for errors and text ----

fn print(arena: Allocator, comptime fmt: []const u8, args: anytype) Allocator.Error![]u8 {
    return std.fmt.allocPrint(arena, fmt, args);
}

/// A plain error (Go: fmt.Errorf of an os error): cancellation and memory keep
/// their class, the rest is `Failed` with "par2: <what> <path>: <error>".
fn ioFail(arena: Allocator, diag: *Diag, what: []const u8, path: []const u8, err: anyerror) PortError {
    switch (err) {
        error.Canceled => return error.Canceled,
        error.OutOfMemory => return error.OutOfMemory,
        else => {},
    }
    diag.message = print(arena, "par2: {s} {s}: {s}", .{ what, path, @errorName(err) }) catch return error.OutOfMemory;
    return error.Failed;
}

/// The class of a name-guard error (its text is in the Diag already).
fn lift(e: names.Error) PortError {
    return switch (e) {
        error.UnsafeSet => error.PathViolation,
        error.Failed => error.Failed,
        error.Canceled => error.Canceled,
        error.OutOfMemory => error.OutOfMemory,
    };
}

/// Go's filepath.Abs.
fn absolute(arena: Allocator, io: Io, dir: []const u8, diag: *Diag) PortError![]const u8 {
    if (fsutil.isAbs(dir)) return fsutil.clean(arena, dir);
    const cwd = std.process.currentPathAlloc(io, arena) catch |e| return ioFail(arena, diag, "cwd for", dir, e);
    return fsutil.join(arena, &.{ cwd, dir });
}

/// How the cases of an unexpected par2 exit are named in an error.
fn exitName(code: i32) []const u8 {
    return switch (code) {
        exit_repair_required => "repair required",
        exit_repair_impossible => "repair not possible",
        exit_invalid_args => "invalid arguments",
        exit_insufficient_crit => "insufficient critical data",
        exit_repair_failed => "repair failed",
        exit_file_io => "file I/O error",
        exit_logic => "logic error",
        exit_memory => "out of memory",
        exit_signal => "killed",
        else => "unknown",
    };
}

/// Describes an unexpected par2 exit with the tail of its output.
fn exitError(arena: Allocator, what: []const u8, res: tool.Result) Allocator.Error![]u8 {
    var tail = try std.mem.concat(arena, u8, &.{ res.stdout, res.stderr });
    if (tail.len > output_tail) tail = tail[tail.len - output_tail ..];
    return print(arena, "par2: {s}: exit status {d} ({s}): {s}", .{ what, res.exit_code, exitName(res.exit_code), try printable(arena, tail) });
}

/// Keeps `b` as one line of text: progress lines (\r) and line breaks become
/// " | ", other control bytes are dropped.
fn printable(arena: Allocator, b: []const u8) Allocator.Error![]const u8 {
    var sb: std.ArrayList(u8) = .empty;
    for (b) |c| switch (c) {
        '\r', '\n' => try sb.appendSlice(arena, " | "),
        0...9, 0x0b, 0x0c, 0x0e...0x1f, 0x7f => {},
        else => try sb.append(arena, c),
    };
    return trimSpace(sb.items);
}

// ---- Unicode-aware trimming (Go's strings.TrimSpace) ----

fn isSpaceCp(cp: u21) bool {
    return switch (cp) {
        '\t', '\n', 0x0b, 0x0c, '\r', ' ', 0x85, 0xa0, 0x1680, 0x2000...0x200a, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000 => true,
        else => false,
    };
}

/// The code point at the start of `s` and its length, or null for invalid UTF-8.
fn firstRune(s: []const u8) ?struct { u21, usize } {
    if (s.len == 0) return null;
    const n = std.unicode.utf8ByteSequenceLength(s[0]) catch return null;
    if (n > s.len) return null;
    const cp = std.unicode.utf8Decode(s[0..n]) catch return null;
    return .{ cp, n };
}

fn trimSpace(s_in: []const u8) []const u8 {
    var s = s_in;
    while (firstRune(s)) |r| {
        if (!isSpaceCp(r[0])) break;
        s = s[r[1]..];
    }
    while (s.len > 0) {
        // The last rune starts at most 3 bytes back, on a non-continuation byte.
        var start = s.len - 1;
        while (start > 0 and s.len - start < 4 and s[start] & 0xc0 == 0x80) start -= 1;
        const r = firstRune(s[start..]) orelse break;
        if (r[1] != s.len - start or !isSpaceCp(r[0])) break;
        s = s[0..start];
    }
    return s;
}

/// Go's filepath.Ext(name) is ".par2", ignoring case.
fn hasPar2Ext(name: []const u8) bool {
    const dot = std.mem.lastIndexOfScalar(u8, name, '.') orelse return false;
    return std.ascii.eqlIgnoreCase(name[dot..], ".par2");
}

// ---- looking at the directory ----

const ParFile = struct { name: []const u8, size: u64 };

const Listing = struct {
    pars: []ParFile,
    others: []const []const u8,
};

/// Whether the entry is a regular file (not a symlink, directory or special
/// file), asking the file system when the listing does not say.
fn isRegular(io: Io, d: Io.Dir, e: Io.Dir.Entry) bool {
    return switch (e.kind) {
        .file => true,
        .unknown => blk: {
            const st = d.statFile(io, e.name, .{ .follow_symlinks = false }) catch break :blk false;
            break :blk st.kind == .file;
        },
        else => false,
    };
}

/// The regular files of the top level of `d`, split into PAR2 files (by name)
/// and the rest. Symlinks, directories and special files are never given to
/// par2.
fn list(arena: Allocator, io: Io, d: Io.Dir, diag: *Diag) PortError!Listing {
    var pars: std.ArrayList(ParFile) = .empty;
    var others: std.ArrayList([]const u8) = .empty;
    var it = d.iterate();
    var count: usize = 0;
    while (it.next(io) catch |err| return ioFail(arena, diag, "list", "directory", err)) |e| {
        if (!isRegular(io, d, e)) continue;
        count += 1;
        if (count > max_entries) {
            diag.message = try print(arena, "par2: more than {d} files in the directory", .{max_entries});
            return error.Failed;
        }
        const name = try arena.dupe(u8, e.name);
        if (hasPar2Ext(name)) {
            const st = d.statFile(io, name, .{ .follow_symlinks = false }) catch |err| return ioFail(arena, diag, "stat", name, err);
            try pars.append(arena, .{ .name = name, .size = st.size });
        } else {
            try others.append(arena, name);
        }
    }
    // Go's os.ReadDir sorts by name; keep that order so the extras list (and
    // so par2's work) does not depend on the file system.
    std.mem.sort([]const u8, others.items, {}, lessString);
    return .{ .pars = pars.items, .others = others.items };
}

/// Gives a .par2 name to every file directly in the directory that is a PAR2
/// file by content but not by name. Fully obfuscated posts name the PAR2
/// files like the data (random hex, no extension); without the extension
/// neither inspect nor par2 would know them, the data would keep its random
/// names and the PAR2 files would be left in the output. The new name is the
/// old one plus ".par2", made safe and unique; nothing is overwritten.
fn adoptPar2Files(arena: Allocator, io: Io, d: Io.Dir, diag: *Diag) PortError!void {
    const l = try list(arena, io, d, diag);
    for (l.others) |name| {
        if (!try startsWithPar2(io, d, name)) continue;
        const safe = fsutil.safeName(arena, try print(arena, "{s}.par2", .{name})) catch |e| switch (e) {
            error.InvalidName => continue,
            error.OutOfMemory => return error.OutOfMemory,
        };
        const uniq = try fsutil.unique(arena, io, d, safe);
        d.rename(name, d, uniq, io) catch |e| {
            if (e == error.Canceled) return error.Canceled;
            diag.message = try print(arena, "par2: name PAR2 file {f}: {s}", .{ quoted(name), @errorName(e) });
            return error.Failed;
        };
    }
}

fn quoted(s: []const u8) std.fmt.Alt([]const u8, formatQuoted) {
    return .{ .data = s };
}

fn formatQuoted(s: []const u8, w: *Io.Writer) Io.Writer.Error!void {
    try @import("log_text.zig").writeQuoted(w, s);
}

/// Whether the file begins with a PAR2 packet. A file that cannot be read is
/// not one; cancellation is passed on.
fn startsWithPar2(io: Io, d: Io.Dir, name: []const u8) PortError!bool {
    const f = d.openFile(io, name, .{ .allow_directory = false }) catch |e| {
        if (e == error.Canceled) return error.Canceled;
        return false;
    };
    defer f.close(io);
    var head: [names.magic.len]u8 = undefined;
    const n = f.readPositionalAll(io, &head, 0) catch |e| {
        if (e == error.Canceled) return error.Canceled;
        return false;
    };
    return n == head.len and std.mem.eql(u8, &head, names.magic);
}

/// What we learn about a directory before par2 is started.
const SetInfo = struct {
    dir: []const u8, // absolute
    main: []const u8, // the .par2 file given to par2
    extra: []const []const u8, // everything else par2 may read: other .par2 files and non-target files
    names: []const []const u8, // target names from the File Description packets, sorted
};

fn lessString(_: void, a: []const u8, b: []const u8) bool {
    return std.mem.lessThan(u8, a, b);
}

fn lessPar(_: void, a: ParFile, b: ParFile) bool {
    if (a.size != b.size) return a.size < b.size;
    return std.mem.lessThan(u8, a.name, b.name);
}

/// Lists the directory (not its subdirectories), reads the file names out of
/// every .par2 file and checks them. Null when there is no set.
fn inspect(arena: Allocator, io: Io, d: Io.Dir, abs: []const u8, diag: *Diag) PortError!?SetInfo {
    const l = try list(arena, io, d, diag);
    if (l.pars.len == 0) return null;

    var sc = try names.Scanner.init(arena);
    var set: names.NameSet = .{};
    for (l.pars) |p| names.readNamesFile(arena, io, &sc, d, p.name, &set, diag) catch |e| return lift(e);
    if (set.names().len == 0) return null;
    const sorted = try arena.dupe([]const u8, set.names());
    std.mem.sort([]const u8, sorted, {}, lessString);
    names.checkNames(arena, io, abs, sorted, diag) catch |e| return lift(e);

    // The main file is the smallest one: the index without recovery data.
    std.mem.sort(ParFile, l.pars, {}, lessPar);
    var extra: std.ArrayList([]const u8) = .empty;
    for (l.pars[1..]) |p| try extra.append(arena, try fsutil.join(arena, &.{ abs, p.name }));
    for (l.others) |n| {
        // A file that already has a PAR2 name is checked as that target. The
        // rest may be an obfuscated copy of one: par2 finds it by hash. The
        // list is read line by line and trimmed, so names it cannot carry are
        // left out.
        if (!set.map.contains(n) and listable(n)) try extra.append(arena, try fsutil.join(arena, &.{ abs, n }));
    }
    return .{
        .dir = abs,
        .main = try fsutil.join(arena, &.{ abs, l.pars[0].name }),
        .extra = extra.items,
        .names = sorted,
    };
}

/// Whether a name survives a line-based, trimmed file list.
fn listable(name: []const u8) bool {
    if (name.len == 0 or trimSpace(name).len != name.len) return false;
    for (name) |c| if (c < 0x20 or c == 0x7f) return false;
    return true;
}

// ---- cleanup after a successful run ----

/// Deletes the damaged copies of repaired targets that par2 found under other
/// names (an obfuscated and damaged file: the repair writes the target under
/// its set name and leaves the copy). Only names from par2's report that are
/// exactly files we gave it as extras are touched, and only regular files.
fn removePartials(arena: Allocator, io: Io, si: SetInfo, partials: []const []const u8, diag: *Diag) PortError!void {
    var extra: std.StringHashMapUnmanaged(void) = .empty;
    for (si.extra) |p| try extra.put(arena, p, {});
    const cwd = Io.Dir.cwd();
    for (partials) |n| {
        const p = try fsutil.join(arena, &.{ si.dir, n });
        if (!extra.contains(p) or hasPar2Ext(n)) continue;
        const st = cwd.statFile(io, p, .{ .follow_symlinks = false }) catch |e| {
            if (e == error.Canceled) return error.Canceled;
            continue;
        };
        if (st.kind != .file) continue;
        cwd.deleteFile(io, p) catch |e| switch (e) {
            error.FileNotFound => {},
            else => return ioFail(arena, diag, "remove", p, e),
        };
    }
}

/// The directory of a slash-separated relative name (Go: path.Dir).
fn dirOf(n: []const u8) []const u8 {
    const i = std.mem.lastIndexOfScalar(u8, n, '/') orelse return ".";
    return n[0..i];
}

/// Go's path.Join(rel, name) for a rel from `dirOf` and a plain name.
fn joinRel(arena: Allocator, rel: []const u8, name: []const u8) Allocator.Error![]const u8 {
    if (std.mem.eql(u8, rel, ".")) return name;
    return print(arena, "{s}/{s}", .{ rel, name });
}

/// The part before the ".N" of a backup name (`^(.+)\.(\d{1,4})$`: the dot is
/// the last one, and `.` does not match a line break), or null.
fn backupBase(name: []const u8) ?[]const u8 {
    const dot = std.mem.lastIndexOfScalar(u8, name, '.') orelse return null;
    const digits = name[dot + 1 ..];
    if (dot == 0 or digits.len < 1 or digits.len > max_backup_suffix_digits) return null;
    for (digits) |c| if (c < '0' or c > '9') return null;
    if (std.mem.indexOfScalar(u8, name[0..dot], '\n') != null) return null;
    return name[0..dot];
}

/// Deletes the name.N files par2 leaves when it replaces a damaged target:
/// regular files next to a target whose name is a target name plus ".N" and
/// that are not targets themselves. They are old copies of data that has been
/// repaired; left in place they would be moved into the library.
fn removeBackups(arena: Allocator, io: Io, si: SetInfo, diag: *Diag) PortError!void {
    var targets: std.StringHashMapUnmanaged(void) = .empty;
    var dirs: std.StringArrayHashMapUnmanaged(void) = .empty;
    for (si.names) |n| {
        try targets.put(arena, n, {});
        try dirs.put(arena, dirOf(n), {});
    }
    var buf: [64 << 10]u8 = undefined;
    var fba: std.heap.FixedBufferAllocator = .init(&buf);
    for (dirs.keys()) |rel| {
        fba.reset();
        const p = fsutil.safeJoin(fba.allocator(), io, si.dir, rel) catch |e| switch (e) {
            error.Canceled => return error.Canceled,
            else => {
                diag.message = try print(arena, "par2: unsafe PAR2 set: path violation: {s}", .{@errorName(e)});
                return error.PathViolation;
            },
        };
        var sub = Io.Dir.cwd().openDir(io, p, .{ .iterate = true }) catch |e| switch (e) {
            error.FileNotFound => continue,
            else => return ioFail(arena, diag, "list", p, e),
        };
        defer sub.close(io);
        // List first, delete after: removing while reading a directory may
        // make a file system skip entries.
        var victims: std.ArrayList([]const u8) = .empty;
        var it = sub.iterate();
        while (it.next(io) catch |e| return ioFail(arena, diag, "list", p, e)) |ent| {
            const base = backupBase(ent.name) orelse continue;
            if (!isRegular(io, sub, ent)) continue;
            const full = try joinRel(arena, rel, ent.name);
            if (targets.contains(full) or !targets.contains(try joinRel(arena, rel, base))) continue;
            try victims.append(arena, try arena.dupe(u8, ent.name));
        }
        for (victims.items) |name| {
            sub.deleteFile(io, name) catch |err| switch (err) {
                error.FileNotFound => {},
                else => return ioFail(arena, diag, "remove", name, err),
            };
        }
    }
}

test {
    _ = output;
    _ = names;
}
