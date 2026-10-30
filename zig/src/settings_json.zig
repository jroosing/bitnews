//! Settings JSON the way Go's encoding/json does it, so that a PUT body, the
//! stored row and the GET answer behave like the Go daemon's:
//!
//!  * decoding is onto an existing value (the defaults when a row is loaded,
//!    the zero value for a PUT), keys match case-insensitively, `null`
//!    leaves a value alone (and empties a list), a number must be an integer
//!    for an integer field, and a value of the wrong type is reported with
//!    Go's wording. A request refuses unknown fields; a stored row ignores
//!    them. Syntax errors are all reported as "malformed JSON".
//!  * encoding writes the fields in struct order, strings with Go's HTML-safe
//!    escaping (`<`, `>`, `&`, U+2028 and U+2029 as \u escapes) and numbers in
//!    Go's float format.
//!
//! The decoder reads the body with the streaming scanner and never builds a
//! tree: the nesting it follows is that of the types, and a value it skips is
//! skipped iteratively, so a deeply nested hostile body costs no stack.
const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const Scanner = std.json.Scanner;
const types = @import("settings_types.zig");
const Settings = types.Settings;

// ---- encoding ----

/// Writes `s` as JSON, without a trailing newline.
pub fn write(w: *Io.Writer, s: Settings) Io.Writer.Error!void {
    try writeValue(w, Settings, s);
}

fn writeValue(w: *Io.Writer, comptime T: type, v: T) Io.Writer.Error!void {
    switch (T) {
        bool => return w.writeAll(if (v) "true" else "false"),
        i64 => return w.print("{d}", .{v}),
        f64 => return writeFloat(w, v),
        []const u8 => return writeString(w, v),
        []const []const u8 => {
            try w.writeByte('[');
            for (v, 0..) |item, i| {
                if (i > 0) try w.writeByte(',');
                try writeString(w, item);
            }
            return w.writeByte(']');
        },
        else => {},
    }
    try w.writeByte('{');
    const info = @typeInfo(T).@"struct";
    inline for (info.field_names, info.field_types, 0..) |name, ftype, i| {
        if (i > 0) try w.writeByte(',');
        try w.writeAll("\"" ++ name ++ "\":");
        try writeValue(w, ftype, @field(v, name));
    }
    try w.writeByte('}');
}

/// strconv.FormatFloat(v, 'f' or 'e', -1, 64) as encoding/json picks it: the
/// shortest digits that read back the same, in exponent form below 1e-6 and
/// from 1e21.
fn writeFloat(w: *Io.Writer, v: f64) Io.Writer.Error!void {
    // Validation keeps NaN and infinities out; never write invalid JSON.
    if (!std.math.isFinite(v)) return w.writeAll("0");
    if (v == 0) return w.writeAll(if (std.math.signbit(v)) "-0" else "0");
    const a = @abs(v);
    if (a >= 1e-6 and a < 1e21) return w.print("{d}", .{v});
    var buf: [40]u8 = undefined;
    const s = std.fmt.bufPrint(&buf, "{e}", .{v}) catch unreachable;
    const e = std.mem.indexOfScalar(u8, s, 'e').?;
    try w.writeAll(s[0 .. e + 1]);
    if (s[e + 1] != '-') try w.writeByte('+');
    try w.writeAll(s[e + 1 ..]);
}

fn writeString(w: *Io.Writer, s: []const u8) Io.Writer.Error!void {
    try w.writeByte('"');
    var i: usize = 0;
    while (i < s.len) {
        const c = s[i];
        if (c < 0x80) {
            switch (c) {
                '"' => try w.writeAll("\\\""),
                '\\' => try w.writeAll("\\\\"),
                '\n' => try w.writeAll("\\n"),
                '\r' => try w.writeAll("\\r"),
                '\t' => try w.writeAll("\\t"),
                0x08 => try w.writeAll("\\b"),
                0x0c => try w.writeAll("\\f"),
                '<', '>', '&' => try w.print("\\u{x:0>4}", .{c}),
                0...0x07, 0x0b, 0x0e...0x1f => try w.print("\\u{x:0>4}", .{c}),
                else => try w.writeByte(c),
            }
            i += 1;
            continue;
        }
        const n = std.unicode.utf8ByteSequenceLength(c) catch 0;
        const cp: ?u21 = if (n != 0 and i + n <= s.len) std.unicode.utf8Decode(s[i..][0..n]) catch null else null;
        if (cp) |u| {
            if (u == 0x2028 or u == 0x2029) {
                try w.print("\\u{x:0>4}", .{u});
            } else {
                try w.writeAll(s[i..][0..n]);
            }
            i += n;
        } else {
            // Go writes the replacement character for each invalid byte.
            try w.writeAll("\\ufffd");
            i += 1;
        }
    }
    try w.writeByte('"');
}

// ---- decoding ----

pub const Result = union(enum) {
    ok,
    /// The message of a 400 `invalid_json` response, or of a corrupt row.
    invalid: []const u8,
};

const request_prefix = "invalid request body: ";

/// Decodes `body` onto `dst`, which holds the values for the keys that are
/// absent. New strings and lists live in `arena`. With `strict`, a key that is
/// not a setting is an error (the API); without, it is ignored (a stored row).
pub fn decode(arena: Allocator, body: []const u8, dst: *Settings, strict: bool) Allocator.Error!Result {
    if (std.mem.trim(u8, body, " \t\r\n").len == 0) return .{ .invalid = "the request body is empty" };

    // Like Go, check the whole body before using any of it, so that a syntax
    // error wins over a wrong type earlier in the text.
    var pre = Scanner.initCompleteInput(arena, body);
    defer pre.deinit();
    var first_value_done = false;
    while (true) {
        const tok = pre.next() catch |err| switch (err) {
            error.OutOfMemory => return error.OutOfMemory,
            error.UnexpectedEndOfInput => return .{ .invalid = request_prefix ++ "unexpected EOF" },
            else => return .{ .invalid = if (first_value_done)
                request_prefix ++ "unexpected data after the JSON value"
            else
                request_prefix ++ "malformed JSON" },
        };
        if (tok == .end_of_document) break;
        if (pre.stackHeight() == 0) first_value_done = true;
    }

    var d: Decoder = .{ .arena = arena, .scan = Scanner.initCompleteInput(arena, body), .strict = strict };
    defer d.scan.deinit();
    d.decodeValue(Settings, dst, Settings.go_name) catch |err| switch (err) {
        error.OutOfMemory => return error.OutOfMemory,
        error.Invalid => return .{ .invalid = d.msg },
    };
    return .ok;
}

const DecodeError = error{ Invalid, OutOfMemory };

const Decoder = struct {
    arena: Allocator,
    scan: Scanner,
    strict: bool,
    /// The JSON path of the value being decoded, ".queues.usenet", for messages.
    path: std.ArrayList(u8) = .empty,
    msg: []const u8 = "",

    fn fail(d: *Decoder, comptime fmt: []const u8, args: anytype) DecodeError {
        d.msg = std.fmt.allocPrint(d.arena, request_prefix ++ fmt, args) catch return error.OutOfMemory;
        return error.Invalid;
    }

    fn malformed(d: *Decoder) DecodeError {
        d.msg = request_prefix ++ "malformed JSON";
        return error.Invalid;
    }

    fn next(d: *Decoder) DecodeError!Scanner.Token {
        return d.scan.nextAlloc(d.arena, .alloc_always) catch |err| switch (err) {
            error.OutOfMemory => error.OutOfMemory,
            else => d.malformed(),
        };
    }

    fn peek(d: *Decoder) DecodeError!Scanner.TokenType {
        return d.scan.peekNextTokenType() catch d.malformed();
    }

    fn skip(d: *Decoder) DecodeError!void {
        d.scan.skipValue() catch |err| switch (err) {
            error.OutOfMemory => return error.OutOfMemory,
            else => return d.malformed(),
        };
    }

    fn kindName(kind: Scanner.TokenType) []const u8 {
        return switch (kind) {
            .object_begin => "object",
            .array_begin => "array",
            .string => "string",
            .number => "number",
            .true, .false => "bool",
            else => "value",
        };
    }

    fn typeError(d: *Decoder, kind: Scanner.TokenType, type_name: []const u8) DecodeError {
        if (d.path.items.len == 0) {
            return d.fail("json: cannot unmarshal {s} into Go value of type {s}", .{ kindName(kind), type_name });
        }
        return d.fail("json: cannot unmarshal {s} into Go struct field Settings{s} of type {s}", .{ kindName(kind), d.path.items, type_name });
    }

    fn numberError(d: *Decoder, number: []const u8, type_name: []const u8) DecodeError {
        return d.fail("json: cannot unmarshal number {s} into Go struct field Settings{s} of type {s}", .{ number, d.path.items, type_name });
    }

    fn numberText(d: *Decoder) DecodeError![]const u8 {
        return switch (try d.next()) {
            .number => |t| t,
            .allocated_number => |t| t,
            else => d.malformed(),
        };
    }

    fn stringValue(d: *Decoder) DecodeError![]const u8 {
        return switch (try d.next()) {
            .string => |t| try d.arena.dupe(u8, t),
            .allocated_string => |t| t,
            else => d.malformed(),
        };
    }

    fn decodeValue(d: *Decoder, comptime T: type, dst: *T, comptime type_name: []const u8) DecodeError!void {
        const kind = try d.peek();
        if (kind == .null) {
            // null leaves a value alone, except that it empties a list.
            _ = try d.next();
            if (T == []const []const u8) dst.* = &.{};
            return;
        }
        switch (T) {
            bool => {
                dst.* = switch (kind) {
                    .true => true,
                    .false => false,
                    else => return d.typeError(kind, type_name),
                };
                _ = try d.next();
            },
            i64 => {
                if (kind != .number) return d.typeError(kind, type_name);
                const t = try d.numberText();
                if (std.mem.findAny(u8, t, ".eE") != null) return d.numberError(t, type_name);
                dst.* = std.fmt.parseInt(i64, t, 10) catch return d.numberError(t, type_name);
            },
            f64 => {
                if (kind != .number) return d.typeError(kind, type_name);
                const t = try d.numberText();
                const v = std.fmt.parseFloat(f64, t) catch return d.numberError(t, type_name);
                if (!std.math.isFinite(v)) return d.numberError(t, type_name);
                dst.* = v;
            },
            []const u8 => {
                if (kind != .string) return d.typeError(kind, type_name);
                dst.* = try d.stringValue();
            },
            []const []const u8 => {
                if (kind != .array_begin) return d.typeError(kind, type_name);
                _ = try d.next();
                var list: std.ArrayList([]const u8) = .empty;
                while (true) {
                    const k = try d.peek();
                    switch (k) {
                        .array_end => {
                            _ = try d.next();
                            break;
                        },
                        .null => {
                            _ = try d.next();
                            try list.append(d.arena, "");
                        },
                        .string => try list.append(d.arena, try d.stringValue()),
                        else => return d.fail("json: cannot unmarshal {s} into Settings{s}.{d} of type string", .{ kindName(k), d.path.items, list.items.len }),
                    }
                }
                dst.* = list.items;
            },
            else => try d.decodeStruct(T, dst, type_name),
        }
    }

    fn decodeStruct(d: *Decoder, comptime T: type, dst: *T, comptime type_name: []const u8) DecodeError!void {
        const kind = try d.peek();
        if (kind != .object_begin) return d.typeError(kind, type_name);
        _ = try d.next();
        const info = @typeInfo(T).@"struct";
        while (true) {
            const key = switch (try d.next()) {
                .object_end => return,
                .string => |k| k,
                .allocated_string => |k| k,
                else => return d.malformed(),
            };
            var matched = false;
            inline for (info.field_names, info.field_types) |name, ftype| {
                if (!matched and std.mem.eql(u8, key, name)) {
                    matched = true;
                    try d.decodeField(T, name, ftype, dst);
                }
            }
            inline for (info.field_names, info.field_types) |name, ftype| {
                if (!matched and std.ascii.eqlIgnoreCase(key, name)) {
                    matched = true;
                    try d.decodeField(T, name, ftype, dst);
                }
            }
            if (!matched) {
                if (d.strict) return d.fail("json: unknown field \"{s}\"", .{key});
                try d.skip();
            }
        }
    }

    fn decodeField(d: *Decoder, comptime T: type, comptime name: []const u8, comptime ftype: type, dst: *T) DecodeError!void {
        const mark = d.path.items.len;
        try d.path.append(d.arena, '.');
        try d.path.appendSlice(d.arena, name);
        try d.decodeValue(ftype, &@field(dst, name), goTypeName(T, name, ftype));
        d.path.shrinkRetainingCapacity(mark);
    }
};

/// How Go names the type of a field in its error messages.
fn goTypeName(comptime Parent: type, comptime name: []const u8, comptime ftype: type) []const u8 {
    if (@hasDecl(Parent, "go_types") and @hasField(@TypeOf(Parent.go_types), name)) return @field(Parent.go_types, name);
    return switch (ftype) {
        bool => "bool",
        i64 => "int",
        f64 => "float64",
        []const u8 => "string",
        []const []const u8 => "[]string",
        else => ftype.go_name,
    };
}

// ---- tests ----

const testing = std.testing;
const golden = @import("settings_golden.zig");
const validate = @import("settings_validate.zig").validate;

fn render(a: Allocator, s: Settings) ![]u8 {
    var aw: Io.Writer.Allocating = .init(a);
    try std.json.Stringify.value(s, .{}, &aw.writer);
    try aw.writer.writeByte('\n');
    return aw.written();
}

test "the default settings are written like Go writes them" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    try testing.expectEqualStrings(golden.default_json, try render(arena.allocator(), Settings.defaults()));
}

test "a PUT body gives the same answer as Go: decode error, validation message or the settings" {
    for (golden.cases, 0..) |c, i| {
        var arena: std.heap.ArenaAllocator = .init(testing.allocator);
        defer arena.deinit();
        const a = arena.allocator();
        errdefer std.debug.print("golden case {d}: {s}\n", .{ i, c.in });

        var s: Settings = .{};
        switch (try decode(a, c.in, &s, true)) {
            .invalid => |msg| {
                try testing.expectEqual(golden.Kind.decode, c.kind);
                try testing.expectEqualStrings(c.want, msg);
            },
            .ok => {
                if (try validate(a, s)) |msg| {
                    try testing.expectEqual(golden.Kind.invalid, c.kind);
                    try testing.expectEqualStrings(c.want, msg);
                } else {
                    try testing.expectEqual(golden.Kind.ok, c.kind);
                    try testing.expectEqualStrings(c.want, try render(a, s));
                }
            },
        }
    }
}

test "a stored row is the defaults with the stored keys on top" {
    for (golden.loads) |c| {
        var arena: std.heap.ArenaAllocator = .init(testing.allocator);
        defer arena.deinit();
        var s = Settings.defaults();
        try testing.expectEqual(Result.ok, try decode(arena.allocator(), c.in, &s, false));
        try testing.expectEqualStrings(c.want, try render(arena.allocator(), s));
    }
}

test "rows written by Go load, and what we write loads again" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();

    var def = Settings.defaults();
    try testing.expectEqual(Result.ok, try decode(a, golden.stored_default, &def, false));
    try testing.expectEqualStrings(golden.default_json, try render(a, def));

    var changed = Settings.defaults();
    try testing.expectEqual(Result.ok, try decode(a, golden.stored_changed, &changed, false));
    try testing.expectEqual(3, changed.queues.usenet);
    try testing.expectEqual(1.5, changed.seeding.ratio_limit);
    try testing.expectEqual(1, changed.auth.allowed_hosts.len);
    try testing.expectEqualStrings("h<&>", changed.auth.allowed_hosts[0]);

    var out: Io.Writer.Allocating = .init(a);
    try write(&out.writer, changed);
    var again: Settings = .{};
    try testing.expectEqual(Result.ok, try decode(a, out.written(), &again, true));
    var out2: Io.Writer.Allocating = .init(a);
    try write(&out2.writer, again);
    try testing.expectEqualStrings(out.written(), out2.written());
}

test "a body nested a megabyte deep costs no stack" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const depth = 1 << 20;
    const body = try arena.allocator().alloc(u8, depth + 2);
    @memset(body, '[');
    body[0] = '{';
    body[1] = '"';
    var s: Settings = .{};
    // Unbalanced, so malformed; the point is that it returns.
    try testing.expect((try decode(arena.allocator(), body, &s, true)) == .invalid);
    var open: std.ArrayList(u8) = .empty;
    try open.appendSlice(arena.allocator(), "{\"x\":");
    try open.appendNTimes(arena.allocator(), '[', 100_000);
    try open.appendNTimes(arena.allocator(), ']', 100_000);
    try open.append(arena.allocator(), '}');
    try testing.expectEqualStrings("invalid request body: json: unknown field \"x\"", (try decode(arena.allocator(), open.items, &s, true)).invalid);
    try testing.expectEqual(Result.ok, try decode(arena.allocator(), open.items, &s, false));
}

fn fuzzDecode(_: void, smith: *testing.Smith) !void {
    var buf: [512]u8 = undefined;
    const n = smith.slice(&buf);
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    var s: Settings = .{};
    switch (try decode(a, buf[0..n], &s, smith.value(bool))) {
        .invalid => return,
        .ok => {},
    }
    _ = try validate(a, s);
    // Whatever was accepted is written as JSON that reads back to the same text.
    var w1: Io.Writer.Allocating = .init(a);
    try write(&w1.writer, s);
    var s2: Settings = .{};
    try testing.expectEqual(Result.ok, try decode(a, w1.written(), &s2, true));
    var w2: Io.Writer.Allocating = .init(a);
    try write(&w2.writer, s2);
    try testing.expectEqualStrings(w1.written(), w2.written());
}

test "fuzz: any request body is decoded or refused, and what is accepted round-trips" {
    try testing.fuzz({}, fuzzDecode, .{ .corpus = &.{
        "\x23\x00\x00\x00{\"queues\":{\"usenet\":2},\"auth\":null}\x01",
        "\x1b\x00\x00\x00{\"seeding\":{\"ratio_limit\":1e5}}\x00",
        "\x17\x00\x00\x00{\"auth\":{\"allowed_hosts\":[\"<\"]}}\x01",
        "\x04\x00\x00\x00null\x00",
        "\x02\x00\x00\x00[]\x00",
    } });
}
