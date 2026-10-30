//! A bounded, streaming multipart/form-data reader with the behaviour of Go's
//! mime/multipart (internal/api/handlers_add.go reads uploads with it), for
//! bodies that are hostile input: an NZB of up to 64 MiB plus a few small
//! fields.
//!
//! What it takes from Go, quirks included:
//!   - lines before the first boundary (a preamble) are skipped;
//!   - the first boundary line decides between CRLF and bare LF line ends;
//!   - a boundary line may end in spaces and tabs; "--boundary--" ends the form
//!     even without a final line end;
//!   - a body that ends early (no final boundary) or headers cut short end the
//!     list of parts like a normal end (Go's errors wrap io.EOF and the handler
//!     treats any of those as the end); a body that ends inside a part is an
//!     error;
//!   - part headers follow textproto.ReadMIMEHeader: folded lines, a space or
//!     tab on the first line is an error, keys are tokens (or contain a space
//!     and then match nothing);
//!   - the file name is passed through filepath.Base.
//! What it does not take: Content-Transfer-Encoding: quoted-printable is not
//! decoded (Go decodes it); the part headers are capped at 16 KiB and 64 lines
//! (Go: 10 MiB and 10000), the boundary at 200 bytes.
//!
//! Memory: the reader is one struct with an 8 KiB window; part names and the
//! Content-Disposition value live in the arena the caller passes to `next`.
//! The window is not a read-ahead of the whole body, so a part is streamed with
//! `read` or collected with `readUpTo`.
const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const mime = @import("add_mime.zig");

pub const window_size = 8192;
pub const max_boundary_bytes = 200;
pub const max_header_bytes = 16 << 10;
pub const max_header_lines = 64;

pub const Error = error{
    /// Not a multipart body we accept (Go: the "invalid_multipart" family).
    Malformed,
    /// The body is over the limit given to `init` (Go: http.MaxBytesError).
    PayloadTooLarge,
    OutOfMemory,
};

/// One part's identity (Go: Part.FormName and Part.FileName).
pub const Part = struct {
    /// The name of a "form-data" part, else "".
    name: []const u8,
    /// The file name passed through filepath.Base; "" when none.
    file_name: []const u8,
};

/// How a part's body (or the source) ended.
const End = enum {
    /// The boundary was found: a normal end.
    clean,
    /// The source ended inside the part.
    unexpected_eof,
    too_large,
    failed,
};

const SrcEnd = enum { eof, too_large, failed };

pub const Reader = struct {
    src: *Io.Reader,
    /// Bytes the source may give in all; one more is `PayloadTooLarge`.
    limit: u64,
    consumed: u64 = 0,
    data: [window_size]u8 = undefined,
    r: usize = 0,
    w: usize = 0,
    /// Sticky: once the source failed it is not asked again.
    src_end: ?SrcEnd = null,

    /// "\r\n--boundary--" (arena); the others are slices of it.
    delim: []const u8,
    lf_mode: bool = false,
    parts_read: usize = 0,

    in_part: bool = false,
    /// Body bytes known to be data, waiting in the window.
    part_n: usize = 0,
    part_total: u64 = 0,
    part_end: ?End = null,
    part_read_err: ?End = null,

    /// `src` must have a buffer of at least one byte. `boundary` is copied.
    pub fn init(arena: Allocator, src: *Io.Reader, boundary: []const u8, limit: u64) Error!*Reader {
        // Go reports an empty boundary at the first NextPart.
        if (boundary.len == 0 or boundary.len > max_boundary_bytes) return error.Malformed;
        const delim = try std.mem.concat(arena, u8, &.{ "\r\n--", boundary, "--" });
        const self = try arena.create(Reader);
        self.* = .{ .src = src, .limit = limit, .delim = delim };
        return self;
    }

    fn nl(self: *const Reader) []const u8 {
        return if (self.lf_mode) self.delim[1..2] else self.delim[0..2];
    }
    /// "\r\n--boundary" or "\n--boundary".
    fn nlDashBoundary(self: *const Reader) []const u8 {
        return self.delim[(if (self.lf_mode) @as(usize, 1) else 0)..(self.delim.len - 2)];
    }
    /// "--boundary--"
    fn dashBoundaryDash(self: *const Reader) []const u8 {
        return self.delim[2..];
    }
    /// "--boundary"
    fn dashBoundary(self: *const Reader) []const u8 {
        return self.delim[2 .. self.delim.len - 2];
    }

    // ---- the window ----

    fn buffered(self: *const Reader) []const u8 {
        return self.data[self.r..self.w];
    }

    /// Reads at least one more byte into the window, or records why not. The
    /// window must have room (`compact` makes it).
    fn fill(self: *Reader) void {
        if (self.src_end != null) return;
        if (self.r > 0 and self.w == window_size) {
            std.mem.copyForwards(u8, self.data[0 .. self.w - self.r], self.data[self.r..self.w]);
            self.w -= self.r;
            self.r = 0;
        }
        if (self.w == window_size) return;
        const room = window_size - self.w;
        const avail = self.src.peekGreedy(1) catch |err| {
            self.src_end = switch (err) {
                error.EndOfStream => .eof,
                error.ReadFailed => .failed,
            };
            return;
        };
        // MaxBytesReader: n bytes are fine, one more is an error.
        if (self.consumed >= self.limit) {
            self.src_end = .too_large;
            return;
        }
        const allowed: usize = @intCast(@min(self.limit - self.consumed, @as(u64, room)));
        const n = @min(avail.len, allowed);
        @memcpy(self.data[self.w..][0..n], avail[0..n]);
        self.src.toss(n);
        self.w += n;
        self.consumed += n;
    }

    /// Go's bufio Peek(buffered+1): true when there is one more byte; false
    /// with `src_end` set when there never will be.
    fn more(self: *Reader) ?End {
        const have = self.w - self.r;
        while (self.w - self.r <= have and self.src_end == null and have < window_size) self.fill();
        if (self.w - self.r > have) return null;
        return switch (self.src_end orelse return null) {
            .eof => .unexpected_eof, // inside a part, running out is an error
            .too_large => .too_large,
            .failed => .failed,
        };
    }

    // ---- lines ----

    const Line = union(enum) {
        /// Ends in '\n'; valid until the next fill.
        full: []const u8,
        /// The source ended: whatever was left (maybe empty).
        eof: []const u8,
        buffer_full,
        too_large,
        failed,
    };

    /// Go's ReadSlice('\n').
    fn readSlice(self: *Reader) Line {
        while (true) {
            const buf = self.buffered();
            if (std.mem.findScalar(u8, buf, '\n')) |i| {
                self.r += i + 1;
                return .{ .full = buf[0 .. i + 1] };
            }
            if (buf.len >= window_size) return .buffer_full;
            if (self.src_end) |e| {
                self.r = self.w;
                return switch (e) {
                    .eof => .{ .eof = buf },
                    .too_large => .too_large,
                    .failed => .failed,
                };
            }
            self.fill();
        }
    }

    // ---- parts ----

    /// The next part, or null at the end of the form. A part that is not read
    /// to its end is skipped. Strings of the result live in `arena`.
    pub fn next(self: *Reader, arena: Allocator) Error!?Part {
        if (self.in_part) {
            self.drain();
            self.in_part = false;
        }
        var expect_new_part = false;
        while (true) {
            switch (self.readSlice()) {
                .eof => return null,
                .buffer_full, .failed => return error.Malformed,
                .too_large => return error.PayloadTooLarge,
                .full => |line| {
                    if (self.isBoundaryDelimiterLine(line)) {
                        self.parts_read += 1;
                        return self.newPart(arena);
                    }
                    if (self.isFinalBoundary(line)) return null;
                    if (expect_new_part) return error.Malformed;
                    if (self.parts_read == 0) continue; // preamble
                    // The separator between the end of the last part's body
                    // and the boundary line that must follow.
                    if (std.mem.eql(u8, line, self.nl())) {
                        expect_new_part = true;
                        continue;
                    }
                    return error.Malformed;
                },
            }
        }
    }

    /// `^--boundary--[ \t]*(\r\n)?$`
    fn isFinalBoundary(self: *const Reader, line: []const u8) bool {
        if (!std.mem.startsWith(u8, line, self.dashBoundaryDash())) return false;
        const rest = skipLwsp(line[self.dashBoundaryDash().len..]);
        return rest.len == 0 or std.mem.eql(u8, rest, self.nl());
    }

    fn isBoundaryDelimiterLine(self: *Reader, line: []const u8) bool {
        if (!std.mem.startsWith(u8, line, self.dashBoundary())) return false;
        const rest = skipLwsp(line[self.dashBoundary().len..]);
        // On the first part, a line end of "\n" switches to LF mode: against
        // the spec but seen in practice.
        if (self.parts_read == 0 and rest.len == 1 and rest[0] == '\n') self.lf_mode = true;
        return std.mem.eql(u8, rest, self.nl());
    }

    fn newPart(self: *Reader, arena: Allocator) Error!?Part {
        const disposition = self.readHeaders(arena) catch |err| switch (err) {
            // Go's errors here wrap io.EOF: the end of the form.
            error.EndOfForm => return null,
            else => |e| return e,
        };
        self.in_part = true;
        self.part_n = 0;
        self.part_total = 0;
        self.part_end = null;
        self.part_read_err = null;

        const media = mime.parse(arena, disposition) catch |err| switch (err) {
            error.OutOfMemory => return error.OutOfMemory,
            error.InvalidMediaType => return .{ .name = "", .file_name = "" },
        };
        const name = if (std.mem.eql(u8, media.type, "form-data")) media.get("name") orelse "" else "";
        const filename = media.get("filename") orelse "";
        return .{ .name = name, .file_name = if (filename.len == 0) "" else goBase(filename) };
    }

    // ---- headers (net/textproto ReadMIMEHeader) ----

    const HeaderError = error{EndOfForm} || Error;

    /// Appends the next line without its line end. EndOfForm when the source
    /// ended before any byte of it.
    fn readLine(self: *Reader, arena: Allocator, out: *std.ArrayList(u8), budget: *usize) HeaderError!void {
        const start = out.items.len;
        while (true) {
            const buf = self.buffered();
            if (std.mem.findScalar(u8, buf, '\n')) |i| {
                try take(arena, out, budget, buf[0..i]);
                self.r += i + 1;
                if (out.items.len > start and out.items[out.items.len - 1] == '\r') out.items.len -= 1;
                return;
            }
            try take(arena, out, budget, buf);
            self.r = self.w;
            if (self.src_end) |e| switch (e) {
                // A last line without a line end is a line.
                .eof => if (out.items.len > start) return else return error.EndOfForm,
                .too_large => return error.PayloadTooLarge,
                .failed => return error.Malformed,
            };
            self.fill();
        }
    }

    fn take(arena: Allocator, out: *std.ArrayList(u8), budget: *usize, bytes: []const u8) HeaderError!void {
        if (bytes.len > budget.*) return error.Malformed; // Go: message too large
        budget.* -= bytes.len;
        try out.appendSlice(arena, bytes);
    }

    /// Skips spaces and tabs; how many.
    fn skipSpace(self: *Reader) usize {
        var n: usize = 0;
        while (true) {
            if (self.r == self.w) {
                self.fill();
                if (self.r == self.w) return n;
            }
            const c = self.data[self.r];
            if (c != ' ' and c != '\t') return n;
            self.r += 1;
            n += 1;
        }
    }

    /// Reads the header block of a part and returns its (first)
    /// Content-Disposition value, "" when there is none.
    fn readHeaders(self: *Reader, arena: Allocator) HeaderError![]const u8 {
        var budget: usize = max_header_bytes;
        if (self.r == self.w) self.fill();
        if (self.r < self.w and (self.data[self.r] == ' ' or self.data[self.r] == '\t')) return error.Malformed;

        var disposition: ?[]const u8 = null;
        var lines: usize = 0;
        while (true) {
            var first: std.ArrayList(u8) = .empty;
            try self.readLine(arena, &first, &budget);
            if (first.items.len == 0) break; // the blank line
            if (std.mem.findScalar(u8, first.items, ':') == null) return error.Malformed;

            // Folded lines are joined with one space.
            var kv: std.ArrayList(u8) = .empty;
            try kv.appendSlice(arena, trimSpaceTab(first.items));
            while (self.skipSpace() > 0) {
                try kv.append(arena, ' ');
                var cont: std.ArrayList(u8) = .empty;
                try self.readLine(arena, &cont, &budget);
                try kv.appendSlice(arena, trimSpaceTab(cont.items));
                if (kv.items.len > max_header_bytes) return error.Malformed;
            }

            const colon = std.mem.findScalar(u8, kv.items, ':') orelse return error.Malformed;
            const key = kv.items[0..colon];
            const value = kv.items[colon + 1 ..];
            if (!validKey(key)) return error.Malformed;
            for (value) |c| if (!validValueByte(c)) return error.Malformed;
            lines += 1;
            if (lines > max_header_lines) return error.Malformed;
            if (disposition == null and std.ascii.eqlIgnoreCase(key, "Content-Disposition")) {
                disposition = std.mem.trimStart(u8, value, " \t");
            }
        }
        return disposition orelse "";
    }

    // ---- bodies ----

    /// Reads up to `dest.len` bytes of the current part's body; 0 is the end
    /// of the body. A body that is cut short is `Malformed`.
    pub fn read(self: *Reader, dest: []u8) Error!usize {
        std.debug.assert(self.in_part);
        if (dest.len == 0) return 0;
        // Find out how much of the window is data.
        while (self.part_n == 0 and self.part_end == null) {
            const res = scan(self.buffered(), self.dashBoundary(), self.nlDashBoundary(), self.part_total, self.part_read_err);
            self.part_n = res.n;
            self.part_end = res.end;
            if (self.part_n == 0 and self.part_end == null) self.part_read_err = self.more();
        }
        if (self.part_n == 0) return switch (self.part_end.?) {
            .clean => 0,
            .too_large => error.PayloadTooLarge,
            .unexpected_eof, .failed => error.Malformed,
        };
        const n = @min(dest.len, self.part_n);
        @memcpy(dest[0..n], self.data[self.r..][0..n]);
        self.r += n;
        self.part_total += n;
        self.part_n -= n;
        return n;
    }

    /// The rest of the body, as long as it is at most `max` bytes; one more
    /// byte than `max` is returned when the body is longer (so the caller can
    /// tell), and no more are read. Allocated in `gpa`; free with `gpa.free`.
    pub fn readUpTo(self: *Reader, gpa: Allocator, max: usize) Error![]u8 {
        var out: std.ArrayList(u8) = .empty;
        errdefer out.deinit(gpa);
        var chunk: [4096]u8 = undefined;
        while (out.items.len <= max) {
            const want = @min(chunk.len, max + 1 - out.items.len);
            const n = try self.read(chunk[0..want]);
            if (n == 0) break;
            try out.appendSlice(gpa, chunk[0..n]);
        }
        return out.toOwnedSlice(gpa);
    }

    /// Goes through the rest of the body without keeping it (Go: Part.Close);
    /// errors are left for the next line read to find again.
    fn drain(self: *Reader) void {
        var chunk: [1024]u8 = undefined;
        while (true) {
            const n = self.read(&chunk) catch return;
            if (n == 0) return;
        }
    }
};

const ScanResult = struct { n: usize, end: ?End };

/// How much of `buf` is body data (Go: scanUntilBoundary). `dash` is
/// "--boundary", `nl_dash` "\r\n--boundary" or "\n--boundary". At the very
/// start of a body (total 0) a boundary with no line end before it counts.
fn scan(buf: []const u8, dash: []const u8, nl_dash: []const u8, total: u64, read_err: ?End) ScanResult {
    if (total == 0) {
        if (std.mem.startsWith(u8, buf, dash)) {
            switch (matchAfterPrefix(buf, dash, read_err)) {
                -1 => return .{ .n = dash.len, .end = null },
                0 => return .{ .n = 0, .end = null },
                else => return .{ .n = 0, .end = .clean },
            }
        }
        if (std.mem.startsWith(u8, dash, buf)) return .{ .n = 0, .end = read_err };
    }
    if (std.mem.find(u8, buf, nl_dash)) |i| {
        switch (matchAfterPrefix(buf[i..], nl_dash, read_err)) {
            -1 => return .{ .n = i + nl_dash.len, .end = null },
            0 => return .{ .n = i, .end = null },
            else => return .{ .n = i, .end = .clean },
        }
    }
    if (std.mem.startsWith(u8, nl_dash, buf)) return .{ .n = 0, .end = read_err };
    // Anything up to the last line end is body; so is the tail if it is not
    // the start of a boundary.
    if (std.mem.findScalarLast(u8, buf, nl_dash[0])) |i| {
        if (std.mem.startsWith(u8, nl_dash, buf[i..])) return .{ .n = i, .end = null };
    }
    return .{ .n = buf.len, .end = read_err };
}

/// +1 when `buf` (which starts with `prefix`) is a boundary: the prefix is
/// followed by "--", a space, tab, CR or LF, or by the end of input. -1 when
/// it is not ("--foobar" does not match "--foo"). 0 when more input decides.
fn matchAfterPrefix(buf: []const u8, prefix: []const u8, read_err: ?End) i2 {
    if (buf.len == prefix.len) return if (read_err != null) 1 else 0;
    const c = buf[prefix.len];
    if (c == ' ' or c == '\t' or c == '\r' or c == '\n') return 1;
    if (c == '-') {
        if (buf.len == prefix.len + 1) return if (read_err != null) -1 else 0;
        if (buf[prefix.len + 1] == '-') return 1;
    }
    return -1;
}

fn skipLwsp(b: []const u8) []const u8 {
    return std.mem.trimStart(u8, b, " \t");
}

fn trimSpaceTab(s: []const u8) []const u8 {
    return std.mem.trim(u8, s, " \t");
}

fn tokenByte(c: u8) bool {
    return switch (c) {
        '0'...'9', 'a'...'z', 'A'...'Z', '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~' => true,
        else => false,
    };
}

/// A header key: token bytes (and spaces, which Go accepts but never matches).
fn validKey(k: []const u8) bool {
    if (k.len == 0) return false;
    for (k) |c| if (!tokenByte(c) and c != ' ') return false;
    return true;
}

fn validValueByte(c: u8) bool {
    return c == '\t' or (c >= 0x20 and c < 0x7f) or c >= 0x80;
}

/// Go's filepath.Base on Unix for a non-empty path.
pub fn goBase(path: []const u8) []const u8 {
    var p = path;
    while (p.len > 0 and p[p.len - 1] == '/') p = p[0 .. p.len - 1];
    if (p.len == 0) return "/";
    if (std.mem.findScalarLast(u8, p, '/')) |i| p = p[i + 1 ..];
    return p;
}

// ---- tests ----

const testing = std.testing;

/// A source that gives at most `chunk` bytes per fill, to meet every boundary
/// split.
const Chunked = struct {
    data: []const u8,
    pos: usize = 0,
    chunk: usize,
    fail_at_end: bool = false,
    buf: [16]u8 = undefined,
    interface: Io.Reader,

    fn init(c: *Chunked, data: []const u8, chunk: usize) void {
        c.* = .{ .data = data, .chunk = chunk, .interface = .{ .vtable = &.{ .stream = stream }, .buffer = &c.buf, .seek = 0, .end = 0 } };
        c.interface.buffer = &c.buf;
    }

    fn stream(r: *Io.Reader, w: *Io.Writer, limit: Io.Limit) Io.Reader.StreamError!usize {
        const c: *Chunked = @alignCast(@fieldParentPtr("interface", r));
        if (c.pos >= c.data.len) return if (c.fail_at_end) error.ReadFailed else error.EndOfStream;
        const n = @min(c.chunk, c.data.len - c.pos, @backingInt(limit));
        try w.writeAll(c.data[c.pos..][0..n]);
        c.pos += n;
        return n;
    }
};

const Got = struct { name: []const u8, file_name: []const u8, body: []const u8 };

/// Parses everything; the bodies are allocated in `arena`.
fn parseAll(arena: Allocator, data: []const u8, boundary: []const u8, chunk: usize, limit: u64) Error![]const Got {
    var c: Chunked = undefined;
    c.init(data, chunk);
    const rd = try Reader.init(arena, &c.interface, boundary, limit);
    var out: std.ArrayList(Got) = .empty;
    while (try rd.next(arena)) |p| {
        const body = try rd.readUpTo(arena, 1 << 20);
        try out.append(arena, .{ .name = p.name, .file_name = p.file_name, .body = body });
    }
    return out.items;
}

fn form(arena: Allocator, parts: []const [3][]const u8, nl_: []const u8) ![]const u8 {
    var out: std.ArrayList(u8) = .empty;
    for (parts) |p| {
        try out.print(arena, "--XB{s}", .{nl_});
        try out.print(arena, "Content-Disposition: form-data; name=\"{s}\"", .{p[0]});
        if (p[1].len > 0) try out.print(arena, "; filename=\"{s}\"", .{p[1]});
        try out.print(arena, "{s}{s}{s}", .{ nl_, nl_, p[2] });
        try out.appendSlice(arena, nl_);
    }
    try out.print(arena, "--XB--{s}", .{nl_});
    return out.items;
}

test "a form with fields and a file, in every chunk size" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const body = try form(a, &.{ .{ "name", "", "My Release" }, .{ "nzb", "dir/My.Release.nzb", "<nzb>\r\n--XBX not a boundary\r\n</nzb>" }, .{ "paused", "", "" } }, "\r\n");
    for ([_]usize{ 1, 2, 3, 7, 16, 100, 1 << 16 }) |chunk| {
        const got = try parseAll(a, body, "XB", chunk, 1 << 20);
        try testing.expectEqual(@as(usize, 3), got.len);
        try testing.expectEqualStrings("name", got[0].name);
        try testing.expectEqualStrings("My Release", got[0].body);
        try testing.expectEqualStrings("nzb", got[1].name);
        try testing.expectEqualStrings("My.Release.nzb", got[1].file_name);
        try testing.expectEqualStrings("<nzb>\r\n--XBX not a boundary\r\n</nzb>", got[1].body);
        try testing.expectEqualStrings("", got[2].body);
    }
}

test "bare LF line ends work when the first boundary line uses them" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const body = try form(a, &.{ .{ "a", "", "one\ntwo" }, .{ "b", "", "x" } }, "\n");
    const got = try parseAll(a, body, "XB", 5, 1 << 20);
    try testing.expectEqual(@as(usize, 2), got.len);
    try testing.expectEqualStrings("one\ntwo", got[0].body);
    try testing.expectEqualStrings("x", got[1].body);
}

test "a preamble, an epilogue, trailing blanks on boundary lines and a final boundary without line end" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const body = "this is a preamble\r\nof two lines\r\n--XB  \t\r\nContent-Disposition: form-data; name=\"a\"\r\n\r\nval\r\n--XB--";
    const got = try parseAll(a, body, "XB", 4, 1 << 20);
    try testing.expectEqual(@as(usize, 1), got.len);
    try testing.expectEqualStrings("val", got[0].body);
    const with_epilogue = "--XB\r\nContent-Disposition: form-data; name=\"a\"\r\n\r\nval\r\n--XB--  \r\nignored\r\n--XB\r\nnot read";
    try testing.expectEqual(@as(usize, 1), (try parseAll(a, with_epilogue, "XB", 3, 1 << 20)).len);
}

test "a body that ends early ends the form, one cut inside a part is an error" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    try testing.expectEqual(@as(usize, 0), (try parseAll(a, "", "XB", 4, 100)).len);
    try testing.expectEqual(@as(usize, 0), (try parseAll(a, "junk without boundary", "XB", 4, 100)).len);
    // Headers cut short: the end of the form.
    try testing.expectEqual(@as(usize, 0), (try parseAll(a, "--XB\r\nContent-Disposition: form-data; na", "XB", 4, 1000)).len);
    try testing.expectEqual(@as(usize, 0), (try parseAll(a, "--XB\r\n", "XB", 4, 1000)).len);
    // Cut inside the body.
    try testing.expectError(error.Malformed, parseAll(a, "--XB\r\nContent-Disposition: form-data; name=\"a\"\r\n\r\nval", "XB", 4, 1000));
}

test "bad framing is malformed" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    // Junk on a boundary line.
    try testing.expectError(error.Malformed, parseAll(a, "--XB\r\nContent-Disposition: form-data; name=\"a\"\r\n\r\nv\r\n--XB junk\r\nmore\r\n--XB--", "XB", 8, 1000));
    try testing.expectError(error.Malformed, parseAll(a, "--XB\r\n Folded-first: x\r\n\r\n", "XB", 8, 1000));
    try testing.expectError(error.Malformed, parseAll(a, "--XB\r\nno colon here\r\n\r\n", "XB", 8, 1000));
    try testing.expectError(error.Malformed, parseAll(a, "--XB\r\nBad Key\x01: v\r\n\r\n", "XB", 8, 1000));
    try testing.expectError(error.Malformed, parseAll(a, "--XB\r\nKey: v\x01\r\n\r\n", "XB", 8, 1000));
    var c: Chunked = undefined;
    c.init("", 1);
    try testing.expectError(error.Malformed, Reader.init(a, &c.interface, "", 10));
    const long: [max_boundary_bytes + 1]u8 = @splat('x');
    try testing.expectError(error.Malformed, Reader.init(a, &c.interface, &long, 10));
}

test "header lines fold, and only a Content-Disposition of form-data names the part" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const folded = "--XB\r\ncontent-disposition: form-data;\r\n\tname=\"nzb\";\r\n filename=\"a.nzb\"\r\nContent-Type: application/x-nzb\r\n\r\nbody\r\n--XB--\r\n";
    const got = try parseAll(a, folded, "XB", 5, 1000);
    try testing.expectEqualStrings("nzb", got[0].name);
    try testing.expectEqualStrings("a.nzb", got[0].file_name);

    const attachment = "--XB\r\nContent-Disposition: attachment; name=\"x\"; filename=\"f.nzb\"\r\n\r\nb\r\n--XB--\r\n";
    const g2 = try parseAll(a, attachment, "XB", 100, 1000);
    try testing.expectEqualStrings("", g2[0].name);
    try testing.expectEqualStrings("f.nzb", g2[0].file_name);

    const none = "--XB\r\nContent-Type: text/plain\r\n\r\nb\r\n--XB--\r\n";
    try testing.expectEqualStrings("", (try parseAll(a, none, "XB", 100, 1000))[0].name);

    // A space before the colon is accepted by Go but is another header.
    const spaced = "--XB\r\nContent-Disposition : form-data; name=\"a\"\r\n\r\nb\r\n--XB--\r\n";
    try testing.expectEqualStrings("", (try parseAll(a, spaced, "XB", 100, 1000))[0].name);

    // A broken disposition gives no name.
    const broken = "--XB\r\nContent-Disposition: form-data; name=\"a\"; name=\"b\"\r\n\r\nb\r\n--XB--\r\n";
    try testing.expectEqualStrings("", (try parseAll(a, broken, "XB", 100, 1000))[0].name);
}

test "file names go through filepath.Base" {
    try testing.expectEqualStrings("Unix.Name.nzb", goBase("some/dir/Unix.Name.nzb"));
    try testing.expectEqualStrings("x.nzb", goBase("x.nzb"));
    try testing.expectEqualStrings("dir", goBase("dir/"));
    try testing.expectEqualStrings("/", goBase("///"));
    try testing.expectEqualStrings("..", goBase(".."));
    try testing.expectEqualStrings("C:\\dev\\x.nzb", goBase("C:\\dev\\x.nzb"));
}

test "the body limit is a hard stop, exactly at the limit is fine" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const body = try form(a, &.{.{ "a", "", "0123456789" }}, "\r\n");
    _ = try parseAll(a, body, "XB", 7, body.len);
    try testing.expectError(error.PayloadTooLarge, parseAll(a, body, "XB", 7, body.len - 1));
    try testing.expectError(error.PayloadTooLarge, parseAll(a, body, "XB", 1000, 20));
}

test "a part is cut off when the caller stops reading it, and the form goes on" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const body = try form(a, &.{ .{ "big", "", &@as([20000]u8, @splat('x')) }, .{ "next", "", "ok" } }, "\r\n");
    var c: Chunked = undefined;
    c.init(body, 4096);
    const rd = try Reader.init(a, &c.interface, "XB", 1 << 20);
    const first = (try rd.next(a)).?;
    try testing.expectEqualStrings("big", first.name);
    const head = try rd.readUpTo(testing.allocator, 10);
    defer testing.allocator.free(head);
    try testing.expectEqual(@as(usize, 11), head.len); // one more than asked: "longer"
    const second = (try rd.next(a)).?;
    try testing.expectEqualStrings("next", second.name);
    const v = try rd.readUpTo(testing.allocator, 100);
    defer testing.allocator.free(v);
    try testing.expectEqualStrings("ok", v);
    try testing.expect((try rd.next(a)) == null);
}

test "a source that fails is malformed" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    var c: Chunked = undefined;
    c.init("--XB\r\nContent-Disposition: form-data; name=\"a\"\r\n\r\nabc", 5);
    c.fail_at_end = true;
    const rd = try Reader.init(a, &c.interface, "XB", 1 << 20);
    _ = (try rd.next(a)).?;
    try testing.expectError(error.Malformed, rd.readUpTo(a, 100));
}

test "too many or too long header lines are malformed" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    var many: std.ArrayList(u8) = .empty;
    try many.appendSlice(a, "--XB\r\n");
    for (0..max_header_lines + 1) |i| try many.print(a, "X-{d}: v\r\n", .{i});
    try many.appendSlice(a, "\r\nbody\r\n--XB--\r\n");
    try testing.expectError(error.Malformed, parseAll(a, many.items, "XB", 100, 1 << 20));

    const filler: [max_header_bytes + 10]u8 = @splat('a');
    const long = try std.mem.concat(a, u8, &.{ "--XB\r\nX: ", &filler, "\r\n\r\nbody\r\n--XB--\r\n" });
    try testing.expectError(error.Malformed, parseAll(a, long, "XB", 100, 1 << 20));
}

test "a long boundary line without a line end is malformed" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const junk = try a.alloc(u8, window_size + 10);
    @memset(junk, 'j');
    try testing.expectError(error.Malformed, parseAll(a, junk, "XB", 1000, 1 << 20));
}

/// A fuzz seed: the Smith stream for one slice (u32 length, then the bytes).
fn seed(comptime s: []const u8) []const u8 {
    return &(std.mem.toBytes(@as(u32, s.len)) ++ s.*);
}

test "fuzz: any bytes parse or fail, never crash, leak or read past the limit" {
    try testing.fuzz({}, struct {
        fn one(_: void, smith: *testing.Smith) anyerror!void {
            var buf: [600]u8 = undefined;
            const n = smith.slice(&buf);
            const boundaries = [_][]const u8{ "XB", "----WebKitFormBoundaryAbc", "a" };
            const boundary = boundaries[smith.value(u8) % boundaries.len];
            const chunk = @as(usize, smith.value(u8) % 40) + 1;
            var arena: std.heap.ArenaAllocator = .init(testing.allocator);
            defer arena.deinit();
            const a = arena.allocator();
            var c: Chunked = undefined;
            c.init(buf[0..n], chunk);
            const rd = Reader.init(a, &c.interface, boundary, 1000) catch return;
            var total: usize = 0;
            while (true) {
                const part = rd.next(a) catch return;
                if (part == null) break;
                const body = rd.readUpTo(testing.allocator, 64) catch return;
                total += body.len;
                testing.allocator.free(body);
            }
            try testing.expect(total <= n);
        }
    }.one, .{ .corpus = &.{
        seed("--XB\r\nContent-Disposition: form-data; name=\"nzb\"; filename=\"a.nzb\"\r\n\r\n<nzb/>\r\n--XB--\r\n"),
        seed("--XB\nContent-Disposition: form-data; name=\"a\"\n\nvalue\n--XB--"),
        seed("preamble\r\n--XB \t\r\nA: b\r\n c\r\n\r\n\r\n--XB\r\n\r\n\r\n--XB--  \r\n"),
        seed("--XB\r\nContent-Disposition: form-data; name*=utf-8''n%41\r\n\r\n--XBX\r\n--XB-\r\n--XB--"),
    } });
}
