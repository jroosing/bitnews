//! A request-body decoder with the behaviour of Go's encoding/json as the
//! API uses it (`DisallowUnknownFields`), over std.json's scanner. std.json's
//! own parser differs in ways clients can see: it reads an array of small
//! integers into a string, takes "563", 563.0 and 5e2 for an integer, and
//! matches keys case-sensitively.
//!
//! What it does, like Go:
//!   - an object's keys match the field names exactly or, failing that,
//!     ignoring ASCII case; an unknown key is an error; a repeated key takes
//!     the last value;
//!   - `null` into anything that is not an optional or a `Nullable` leaves the
//!     field at its default, and a top-level `null` is an empty request;
//!   - a string field takes only a JSON string; an integer field only an
//!     integer written as one (range checked); a bool only true or false;
//!   - one value only: anything after it is an error.
//! Request types are structs of such fields and EVERY field needs a default.
const std = @import("std");
const Allocator = std.mem.Allocator;
const Scanner = std.json.Scanner;

pub const Error = error{
    /// The details are in `Diag.message`.
    Invalid,
    OutOfMemory,
};

pub const Diag = struct {
    /// Safe to show to the client; allocated in the arena of the parse.
    message: []const u8 = "malformed JSON",
};

/// A request field that tells "not given" from "given as null": the PATCH
/// semantics of internal/api's nullableString. `set` is true when the key was
/// present; `value` is null when it was null.
pub fn Nullable(comptime T: type) type {
    return struct {
        set: bool = false,
        value: ?T = null,

        pub const accepts_null = true;
        const Self = @This();

        pub fn jsonParse(allocator: Allocator, source: *Scanner, diag: *Diag) Error!Self {
            if (try peek(source) == .null) {
                _ = try next(source);
                return .{ .set = true, .value = null };
            }
            return .{ .set = true, .value = try parseValue(T, allocator, source, diag) };
        }
    };
}

/// An integer request field; with the strict decoder every integer field is
/// strict, so this is only kept for the code that already names it.
pub const Int = struct {
    value: i64 = 0,

    pub fn jsonParse(allocator: Allocator, source: *Scanner, diag: *Diag) Error!Int {
        return .{ .value = try parseValue(i64, allocator, source, diag) };
    }
};

/// Parses `body` as one value of type T. Strings of the result are copies in
/// `arena`.
pub fn parse(comptime T: type, arena: Allocator, body: []const u8, diag: *Diag) Error!T {
    var scanner = Scanner.initCompleteInput(arena, body);
    defer scanner.deinit();
    const value: T = value: {
        if (try peek(&scanner) == .null) {
            _ = try next(&scanner);
            break :value .{};
        }
        break :value try parseValue(T, arena, &scanner, diag);
    };
    // Anything but the end after the value, even a second value the scanner
    // refuses outright, is trailing data.
    const after = peek(&scanner) catch |err| switch (err) {
        error.OutOfMemory => return error.OutOfMemory,
        error.Invalid => .object_begin,
    };
    if (after != .end_of_document) {
        diag.message = "unexpected data after the JSON value";
        return error.Invalid;
    }
    return value;
}

// std.json's scanner errors (syntax, truncated input) all mean "malformed".
fn peek(sc: *Scanner) Error!std.json.TokenType {
    return sc.peekNextTokenType() catch error.Invalid;
}

fn next(sc: *Scanner) Error!std.json.Token {
    return sc.next() catch error.Invalid;
}

fn nextAlloc(sc: *Scanner, a: Allocator) Error!std.json.Token {
    const t = sc.nextAlloc(a, .alloc_always) catch |err| switch (err) {
        error.OutOfMemory => return error.OutOfMemory,
        else => return error.Invalid,
    };
    return switch (t) {
        .allocated_string => |s| .{ .string = s },
        .allocated_number => |s| .{ .number = s },
        else => t,
    };
}

fn wrongType(diag: *Diag, comptime T: type) Error {
    diag.message = comptime "a field has the wrong type or value (expected " ++ typeLabel(T) ++ ")";
    return error.Invalid;
}

fn typeLabel(comptime T: type) []const u8 {
    return switch (@typeInfo(T)) {
        .bool => "a boolean",
        .int, .float => "a number",
        .@"enum" => "one of a fixed set of strings",
        .pointer => |p| if (p.child == u8) "a string" else "an array",
        .@"struct" => "an object",
        .optional => |o| typeLabel(o.child),
        else => "another type",
    };
}

fn isOptional(comptime T: type) bool {
    return @typeInfo(T) == .optional or (@typeInfo(T) == .@"struct" and @hasDecl(T, "accepts_null"));
}

/// Parses the next JSON value as T.
pub fn parseValue(comptime T: type, a: Allocator, sc: *Scanner, diag: *Diag) Error!T {
    if (T == std.json.Value) {
        return std.json.innerParse(std.json.Value, a, sc, .{}) catch |err| switch (err) {
            error.OutOfMemory => error.OutOfMemory,
            else => error.Invalid,
        };
    }
    if (comptime @typeInfo(T) == .@"struct" and @hasDecl(T, "jsonParse")) return T.jsonParse(a, sc, diag);

    switch (@typeInfo(T)) {
        .optional => |o| {
            if (try peek(sc) == .null) {
                _ = try next(sc);
                return null;
            }
            return try parseValue(o.child, a, sc, diag);
        },
        .bool => return switch (try peek(sc)) {
            .true => blk: {
                _ = try next(sc);
                break :blk true;
            },
            .false => blk: {
                _ = try next(sc);
                break :blk false;
            },
            else => wrongType(diag, T),
        },
        .int => {
            if (try peek(sc) != .number) return wrongType(diag, T);
            const text = (try nextAlloc(sc, a)).number;
            if (std.mem.indexOfAny(u8, text, ".eE") != null) return wrongType(diag, T);
            return std.fmt.parseInt(T, text, 10) catch wrongType(diag, T);
        },
        .float => {
            if (try peek(sc) != .number) return wrongType(diag, T);
            const text = (try nextAlloc(sc, a)).number;
            return std.fmt.parseFloat(T, text) catch wrongType(diag, T);
        },
        .@"enum" => {
            if (try peek(sc) != .string) return wrongType(diag, T);
            const text = (try nextAlloc(sc, a)).string;
            return std.meta.stringToEnum(T, text) orelse wrongType(diag, T);
        },
        .pointer => |p| {
            if (p.size != .slice) @compileError("unsupported request field type " ++ @typeName(T));
            if (p.child == u8) {
                if (try peek(sc) != .string) return wrongType(diag, T);
                return (try nextAlloc(sc, a)).string;
            }
            if (try peek(sc) != .array_begin) return wrongType(diag, T);
            _ = try next(sc);
            var items: std.ArrayList(p.child) = .empty;
            while (try peek(sc) != .array_end) {
                try items.append(a, try parseValue(p.child, a, sc, diag));
            }
            _ = try next(sc); // array_end
            return items.items;
        },
        .@"struct" => |info| {
            if (try peek(sc) != .object_begin) return wrongType(diag, T);
            _ = try next(sc);
            var result: T = .{};
            while (true) {
                const key = switch (try nextAlloc(sc, a)) {
                    .object_end => break,
                    .string => |s| s,
                    else => return error.Invalid,
                };
                const idx = fieldIndex(info, key) orelse {
                    diag.message = try std.fmt.allocPrint(a, "json: unknown field \"{s}\"", .{key});
                    return error.Invalid;
                };
                inline for (info.field_names, info.field_types, 0..) |name, FieldT, i| {
                    if (i == idx) {
                        if (try peek(sc) == .null and !isOptional(FieldT)) {
                            _ = try next(sc); // null into a plain field changes nothing
                        } else {
                            @field(result, name) = try parseValue(FieldT, a, sc, diag);
                        }
                    }
                }
            }
            return result;
        },
        else => @compileError("unsupported request field type " ++ @typeName(T)),
    }
}

/// The field named `key`: an exact match first, else the first that matches
/// ignoring ASCII case (Go's decoder is case-insensitive).
fn fieldIndex(comptime info: std.builtin.Type.Struct, key: []const u8) ?usize {
    inline for (info.field_names, 0..) |name, i| {
        if (std.mem.eql(u8, name, key)) return i;
    }
    inline for (info.field_names, 0..) |name, i| {
        if (std.ascii.eqlIgnoreCase(name, key)) return i;
    }
    return null;
}

const testing = std.testing;

const Inner = struct { x: i32 = 0 };
const Req = struct {
    name: []const u8 = "",
    count: i32 = 7,
    small: u8 = 0,
    ratio: f64 = 0,
    on: bool = false,
    dir: Nullable([]const u8) = .{},
    maybe: ?i64 = null,
    tags: []const []const u8 = &.{},
    inner: Inner = .{},
    mode: enum { fast, slow } = .fast,
};

fn run(body: []const u8) !union(enum) { ok: Req, bad: []const u8 } {
    var diag: Diag = .{};
    const v = parse(Req, test_arena.allocator(), body, &diag) catch |err| switch (err) {
        error.Invalid => return .{ .bad = diag.message },
        error.OutOfMemory => return error.OutOfMemory,
    };
    return .{ .ok = v };
}

var test_arena: std.heap.ArenaAllocator = undefined;

test "a well-formed request parses, with defaults for what is absent" {
    test_arena = .init(testing.allocator);
    defer test_arena.deinit();
    const r = (try run("{\"name\":\"tv\",\"on\":true,\"tags\":[\"a\",\"b\"],\"inner\":{\"x\":3},\"mode\":\"slow\",\"ratio\":1.5}")).ok;
    try testing.expectEqualStrings("tv", r.name);
    try testing.expect(r.on);
    try testing.expectEqual(@as(i32, 7), r.count);
    try testing.expectEqual(@as(usize, 2), r.tags.len);
    try testing.expectEqualStrings("b", r.tags[1]);
    try testing.expectEqual(@as(i32, 3), r.inner.x);
    try testing.expect(r.mode == .slow);
    try testing.expectEqual(@as(f64, 1.5), r.ratio);
}

test "a string field takes only a string, not an array of integers" {
    test_arena = .init(testing.allocator);
    defer test_arena.deinit();
    for ([_][]const u8{ "{\"name\":[]}", "{\"name\":[104,105]}", "{\"name\":5}", "{\"name\":true}", "{\"name\":{}}" }) |bad| {
        try testing.expect((try run(bad)) == .bad);
    }
}

test "integer fields take only integers in range" {
    test_arena = .init(testing.allocator);
    defer test_arena.deinit();
    try testing.expectEqual(@as(i32, -4), (try run("{\"count\":-4}")).ok.count);
    for ([_][]const u8{
        "{\"count\":\"5\"}", "{\"count\":5.0}", "{\"count\":5e2}", "{\"count\":99999999999}", "{\"count\":true}",
        "{\"small\":256}",   "{\"small\":-1}",  "{\"count\":[1]}",
    }) |bad| {
        try testing.expect((try run(bad)) == .bad);
    }
    try testing.expectEqual(@as(?i64, 9), (try run("{\"maybe\":9}")).ok.maybe);
    try testing.expectEqual(@as(?i64, null), (try run("{\"maybe\":null}")).ok.maybe);
}

test "keys match exactly or ignoring ASCII case, unknown keys are errors, the last repeat wins" {
    test_arena = .init(testing.allocator);
    defer test_arena.deinit();
    try testing.expectEqualStrings("a", (try run("{\"NAME\":\"a\"}")).ok.name);
    try testing.expectEqualStrings("b", (try run("{\"name\":\"a\",\"Name\":\"b\"}")).ok.name);
    try testing.expectEqualStrings("json: unknown field \"id\"", (try run("{\"id\":1}")).bad);
    try testing.expectEqualStrings("second", (try run("{\"name\":\"first\",\"name\":\"second\"}")).ok.name);
}

test "null leaves a plain field alone, sets an optional to null, and null as the whole body is empty" {
    test_arena = .init(testing.allocator);
    defer test_arena.deinit();
    const r = (try run("{\"name\":null,\"count\":null,\"on\":null,\"inner\":null,\"tags\":null,\"maybe\":null}")).ok;
    try testing.expectEqualStrings("", r.name);
    try testing.expectEqual(@as(i32, 7), r.count);
    try testing.expect(!r.on);
    try testing.expectEqual(@as(usize, 0), r.tags.len);
    const whole = (try run("null")).ok;
    try testing.expectEqual(@as(i32, 7), whole.count);
    try testing.expectEqualStrings("", whole.name);
}

test "Nullable tells absent, null and a value apart" {
    test_arena = .init(testing.allocator);
    defer test_arena.deinit();
    try testing.expect(!(try run("{}")).ok.dir.set);
    const n = (try run("{\"dir\":null}")).ok.dir;
    try testing.expect(n.set and n.value == null);
    const v = (try run("{\"dir\":\"/x\"}")).ok.dir;
    try testing.expect(v.set);
    try testing.expectEqualStrings("/x", v.value.?);
    try testing.expect((try run("{\"dir\":[]}")) == .bad);
    try testing.expect((try run("{\"dir\":5}")) == .bad);
}

test "one value only, and malformed input is refused" {
    test_arena = .init(testing.allocator);
    defer test_arena.deinit();
    for ([_][]const u8{ "", "   ", "{", "{\"name\":\"a\"", "{\"name\":\"a\"}{}", "{\"name\":\"a\"} x", "[]", "\"s\"", "5", "{\"name\"}", "{,}", "nul" }) |bad| {
        try testing.expect((try run(bad)) == .bad);
    }
    try testing.expectEqualStrings("unexpected data after the JSON value", (try run("{} {}")).bad);
}

test "an enum field takes only its names" {
    test_arena = .init(testing.allocator);
    defer test_arena.deinit();
    try testing.expect((try run("{\"mode\":\"medium\"}")) == .bad);
    try testing.expect((try run("{\"mode\":1}")) == .bad);
}

test "fuzz: parse never crashes or leaks on any bytes" {
    try testing.fuzz({}, struct {
        fn one(_: void, smith: *testing.Smith) anyerror!void {
            var buf: [256]u8 = undefined;
            const n = smith.slice(&buf);
            var arena: std.heap.ArenaAllocator = .init(testing.allocator);
            defer arena.deinit();
            var diag: Diag = .{};
            _ = parse(Req, arena.allocator(), buf[0..n], &diag) catch |err| switch (err) {
                error.Invalid => {},
                error.OutOfMemory => return error.OutOfMemory,
            };
        }
    }.one, .{ .corpus = &.{
        "\x24\x00\x00\x00{\"name\":\"a\",\"count\":3,\"tags\":[\"x\"],\"dir\":null}",
        "\x04\x00\x00\x00null",
    } });
}
