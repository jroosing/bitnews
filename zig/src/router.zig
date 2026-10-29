//! Request routing with the observable behaviour of Go's net/http ServeMux
//! (patterns like "GET /api/v1/downloads/{id}"), as the Go server uses it:
//!
//! - a pattern is an optional method, a path of literal and "{name}" segments,
//!   and a trailing "/" for a subtree ("/api/v2/" matches everything below);
//! - a GET pattern also answers HEAD;
//! - the path is cleaned like Go's cleanPath (no "//", "." or ".."), and a
//!   request for an unclean path gets a 301 to the clean one;
//! - "/tree" with only "/tree/" registered gets a 301 to "/tree/";
//! - a path that exists for other methods only is a 405 with an Allow header;
//! - segments are percent-decoded before they are compared or handed out as
//!   parameters, so "%68ealth" is "health" and "%2F" stays inside one segment.
//!
//! The router is pure: it never touches the network, so it is fuzzed on its
//! own. Everything a lookup allocates comes from the caller's (per request)
//! arena.
const std = @import("std");
const Allocator = std.mem.Allocator;
const Method = std.http.Method;

pub const Param = struct { name: []const u8, value: []const u8 };

/// Longest path a lookup works on. The server's header limit is lower; this
/// only bounds the router when it is used on its own.
pub const max_path_len = 1 << 16;

pub const AddError = error{ InvalidPattern, DuplicatePattern, OutOfMemory };

/// Router of routes that carry a `T` (the server's handler and limits). Not
/// thread-safe to modify; lookups after the last `add` are read-only and safe
/// from many threads.
pub fn Router(comptime T: type) type {
    return struct {
        const Self = @This();

        const Seg = union(enum) { literal: []const u8, wildcard: []const u8 };

        const Entry = struct {
            method: ?Method,
            segs: []const Seg,
            /// The pattern ended in "/": it also matches everything below.
            subtree: bool,
            value: T,
        };

        pub const Found = struct { value: T, params: []const Param };

        pub const Match = union(enum) {
            found: Found,
            /// Answer 301 with this location (a cleaned path, without the query).
            redirect: []const u8,
            /// Answer 405 with this Allow header.
            method_not_allowed: []const u8,
            not_found,
            /// The path is not a valid request path (not rooted, bad escape).
            bad_path,
        };

        gpa: Allocator,
        entries: std.ArrayList(Entry) = .empty,

        pub fn init(gpa: Allocator) Self {
            return .{ .gpa = gpa };
        }

        pub fn deinit(self: *Self) void {
            for (self.entries.items) |e| self.freeSegs(e.segs);
            self.entries.deinit(self.gpa);
        }

        fn freeSegs(self: *Self, segs: []const Seg) void {
            for (segs) |s| switch (s) {
                .literal, .wildcard => |text| self.gpa.free(text),
            };
            self.gpa.free(segs);
        }

        /// Registers `pattern`. Patterns that differ only in wildcard names are
        /// duplicates.
        pub fn add(self: *Self, pattern: []const u8, value: T) AddError!void {
            var rest = pattern;
            var method: ?Method = null;
            if (std.mem.findScalar(u8, rest, ' ')) |sp| {
                method = std.meta.stringToEnum(Method, rest[0..sp]) orelse return error.InvalidPattern;
                rest = std.mem.trimStart(u8, rest[sp + 1 ..], " ");
            }
            if (rest.len == 0 or rest[0] != '/') return error.InvalidPattern;
            rest = rest[1..];
            var subtree = false;
            if (rest.len == 0) {
                subtree = true;
            } else if (rest[rest.len - 1] == '/') {
                subtree = true;
                rest = rest[0 .. rest.len - 1];
            }

            var segs: std.ArrayList(Seg) = .empty;
            errdefer {
                for (segs.items) |s| switch (s) {
                    .literal, .wildcard => |text| self.gpa.free(text),
                };
                segs.deinit(self.gpa);
            }
            if (rest.len > 0) {
                var it = std.mem.splitScalar(u8, rest, '/');
                while (it.next()) |piece| {
                    if (piece.len == 0) return error.InvalidPattern;
                    if (piece[0] == '{') {
                        if (piece[piece.len - 1] != '}' or piece.len < 3) return error.InvalidPattern;
                        const name = piece[1 .. piece.len - 1];
                        for (name) |c| if (!std.ascii.isAlphanumeric(c) and c != '_') return error.InvalidPattern;
                        try segs.append(self.gpa, .{ .wildcard = try self.gpa.dupe(u8, name) });
                    } else {
                        if (std.mem.findAny(u8, piece, "{}") != null) return error.InvalidPattern;
                        try segs.append(self.gpa, .{ .literal = try self.gpa.dupe(u8, piece) });
                    }
                }
            }

            for (self.entries.items) |e| {
                if (e.method == method and e.subtree == subtree and sameShape(e.segs, segs.items)) return error.DuplicatePattern;
            }
            const owned = try segs.toOwnedSlice(self.gpa);
            errdefer self.freeSegs(owned);
            try self.entries.append(self.gpa, .{ .method = method, .segs = owned, .subtree = subtree, .value = value });
        }

        fn sameShape(a: []const Seg, b: []const Seg) bool {
            if (a.len != b.len) return false;
            for (a, b) |x, y| {
                if (std.meta.activeTag(x) != std.meta.activeTag(y)) return false;
                switch (x) {
                    .literal => |l| if (!std.mem.eql(u8, l, y.literal)) return false,
                    .wildcard => {},
                }
            }
            return true;
        }

        /// Looks up `raw_path`, the path of the request target as received
        /// (still percent-encoded, without the query).
        pub fn lookup(self: *const Self, arena: Allocator, method: Method, raw_path: []const u8) Allocator.Error!Match {
            if (raw_path.len == 0 or raw_path[0] != '/' or raw_path.len > max_path_len) return .bad_path;
            if (!validEscapes(raw_path)) return .bad_path;

            const cleaned = try cleanPath(arena, raw_path);
            const segs = try splitDecoded(arena, cleaned);
            const found_idx = self.bestEntry(method, segs);

            // "/tree" with only "/tree/" registered: redirect to the subtree.
            if (cleaned[cleaned.len - 1] != '/' and (found_idx == null or self.entries.items[found_idx.?].subtree)) {
                const with_slash = try arena.alloc([]const u8, segs.len + 1);
                @memcpy(with_slash[0..segs.len], segs);
                with_slash[segs.len] = "";
                if (self.bestEntry(method, with_slash)) |i| {
                    const e = self.entries.items[i];
                    if (e.subtree and e.segs.len == segs.len) return .{ .redirect = try std.mem.concat(arena, u8, &.{ cleaned, "/" }) };
                }
            }

            if (!std.mem.eql(u8, cleaned, raw_path)) return .{ .redirect = cleaned };

            const i = found_idx orelse {
                if (try self.allowed(arena, segs)) |allow| return .{ .method_not_allowed = allow };
                return .not_found;
            };
            const e = self.entries.items[i];
            var params: std.ArrayList(Param) = .empty;
            for (e.segs, 0..) |s, n| switch (s) {
                .wildcard => |name| try params.append(arena, .{ .name = name, .value = segs[n] }),
                .literal => {},
            };
            return .{ .found = .{ .value = e.value, .params = params.items } };
        }

        fn matches(e: Entry, segs: []const []const u8, method: ?Method) bool {
            if (method) |m| {
                const em = e.method orelse m;
                if (em != m and !(em == .GET and m == .HEAD)) return false;
            }
            if (e.subtree) {
                if (segs.len < e.segs.len + 1) return false;
            } else if (segs.len != e.segs.len) return false;
            for (e.segs, 0..) |s, i| switch (s) {
                .literal => |l| if (!std.mem.eql(u8, l, segs[i])) return false,
                .wildcard => if (segs[i].len == 0) return false,
            };
            return true;
        }

        /// Index of the most specific entry that matches, like Go's precedence:
        /// literals beat wildcards, an exact pattern beats a subtree, a longer
        /// subtree beats a shorter, a method beats none.
        fn bestEntry(self: *const Self, method: Method, segs: []const []const u8) ?usize {
            var found: ?usize = null;
            for (self.entries.items, 0..) |e, i| {
                if (!matches(e, segs, method)) continue;
                if (found == null or moreSpecific(e, self.entries.items[found.?])) found = i;
            }
            return found;
        }

        fn moreSpecific(a: Entry, b: Entry) bool {
            for (a.segs[0..@min(a.segs.len, b.segs.len)], b.segs[0..@min(a.segs.len, b.segs.len)]) |x, y| {
                const xl = std.meta.activeTag(x) == .literal;
                const yl = std.meta.activeTag(y) == .literal;
                if (xl != yl) return xl;
            }
            if (a.subtree != b.subtree) return !a.subtree;
            if (a.segs.len != b.segs.len) return a.segs.len > b.segs.len;
            return a.method != null and b.method == null;
        }

        /// The Allow header for a path that no entry serves for this method, or
        /// null when no entry knows the path at all.
        fn allowed(self: *const Self, arena: Allocator, segs: []const []const u8) Allocator.Error!?[]const u8 {
            var set: std.EnumSet(Method) = .{};
            for (self.entries.items) |e| {
                if (!matches(e, segs, null)) continue;
                const m = e.method orelse continue;
                set.insert(m);
                if (m == .GET) set.insert(.HEAD);
            }
            if (set.count() == 0) return null;
            var names: std.ArrayList([]const u8) = .empty;
            var it = set.iterator();
            while (it.next()) |m| try names.append(arena, @tagName(m));
            std.mem.sort([]const u8, names.items, {}, struct {
                fn lt(_: void, x: []const u8, y: []const u8) bool {
                    return std.mem.lessThan(u8, x, y);
                }
            }.lt);
            return try std.mem.join(arena, ", ", names.items);
        }
    };
}

/// True when every '%' is followed by two hex digits.
pub fn validEscapes(s: []const u8) bool {
    var i: usize = 0;
    while (i < s.len) {
        if (s[i] != '%') {
            i += 1;
            continue;
        }
        if (i + 2 >= s.len) return false;
        if (!std.ascii.isHex(s[i + 1]) or !std.ascii.isHex(s[i + 2])) return false;
        i += 3;
    }
    return true;
}

/// Percent-decodes `s`, which must have valid escapes (see `validEscapes`). '+'
/// is not a space in a path. Returns `s` itself when there is nothing to decode.
pub fn percentDecode(arena: Allocator, s: []const u8) Allocator.Error![]const u8 {
    if (std.mem.findScalar(u8, s, '%') == null) return s;
    const out = try arena.alloc(u8, s.len);
    var n: usize = 0;
    var i: usize = 0;
    while (i < s.len) : (n += 1) {
        if (s[i] == '%' and i + 2 < s.len) {
            out[n] = (hexVal(s[i + 1]) << 4) | hexVal(s[i + 2]);
            i += 3;
        } else {
            out[n] = s[i];
            i += 1;
        }
    }
    return out[0..n];
}

fn hexVal(c: u8) u8 {
    return std.fmt.charToDigit(c, 16) catch 0;
}

fn splitDecoded(arena: Allocator, cleaned: []const u8) Allocator.Error![]const []const u8 {
    var list: std.ArrayList([]const u8) = .empty;
    var it = std.mem.splitScalar(u8, cleaned[1..], '/');
    while (it.next()) |seg| try list.append(arena, try percentDecode(arena, seg));
    return list.items;
}

/// Go's cleanPath: the lexically shortest rooted path ("//", "." and ".."
/// removed, ".." at the root dropped), keeping a trailing slash.
pub fn cleanPath(arena: Allocator, p: []const u8) Allocator.Error![]u8 {
    var stack: std.ArrayList([]const u8) = .empty;
    var it = std.mem.splitScalar(u8, p, '/');
    while (it.next()) |seg| {
        if (seg.len == 0 or std.mem.eql(u8, seg, ".")) continue;
        if (std.mem.eql(u8, seg, "..")) {
            _ = stack.pop();
            continue;
        }
        try stack.append(arena, seg);
    }
    var out: std.ArrayList(u8) = .empty;
    for (stack.items) |seg| {
        try out.append(arena, '/');
        try out.appendSlice(arena, seg);
    }
    if (out.items.len == 0 or (p.len > 0 and p[p.len - 1] == '/')) try out.append(arena, '/');
    return out.items;
}

const testing = std.testing;

fn testRouter() !Router(u32) {
    var r: Router(u32) = .init(testing.allocator);
    errdefer r.deinit();
    try r.add("GET /api/v1/health", 1);
    try r.add("GET /api/v1/downloads", 2);
    try r.add("POST /api/v1/downloads", 3);
    try r.add("GET /api/v1/downloads/{id}", 4);
    try r.add("POST /api/v1/downloads/{id}/pause", 5);
    try r.add("GET /api/v1/downloads/new", 6);
    try r.add("/sabnzbd/api", 7);
    try r.add("/sabnzbd/api/", 8);
    try r.add("/api/v2/", 9);
    return r;
}

fn expectFound(want: u32, m: anytype) !void {
    switch (m) {
        .found => |f| try testing.expectEqual(want, f.value),
        else => |other| {
            std.debug.print("expected found {d}, got {any}\n", .{ want, other });
            return error.TestExpectedEqual;
        },
    }
}

test "exact paths match their method" {
    var r = try testRouter();
    defer r.deinit();
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    try expectFound(1, try r.lookup(arena.allocator(), .GET, "/api/v1/health"));
    try expectFound(2, try r.lookup(arena.allocator(), .GET, "/api/v1/downloads"));
    try expectFound(3, try r.lookup(arena.allocator(), .POST, "/api/v1/downloads"));
}

test "a GET route also answers HEAD" {
    var r = try testRouter();
    defer r.deinit();
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    try expectFound(1, try r.lookup(arena.allocator(), .HEAD, "/api/v1/health"));
}

test "wildcards become decoded parameters and literals win over them" {
    var r = try testRouter();
    defer r.deinit();
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const m = try r.lookup(arena.allocator(), .GET, "/api/v1/downloads/a%20b");
    try expectFound(4, m);
    try testing.expectEqualStrings("id", m.found.params[0].name);
    try testing.expectEqualStrings("a b", m.found.params[0].value);
    try expectFound(6, try r.lookup(arena.allocator(), .GET, "/api/v1/downloads/new"));
    const p = try r.lookup(arena.allocator(), .POST, "/api/v1/downloads/x/pause");
    try expectFound(5, p);
    try testing.expectEqualStrings("x", p.found.params[0].value);
}

test "an encoded %2F stays inside one segment" {
    var r = try testRouter();
    defer r.deinit();
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const m = try r.lookup(arena.allocator(), .GET, "/api/v1/downloads/a%2Fb");
    try testing.expectEqualStrings("a/b", m.found.params[0].value);
}

test "encoded literal segments match" {
    var r = try testRouter();
    defer r.deinit();
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    try expectFound(1, try r.lookup(arena.allocator(), .GET, "/api/v1/%68ealth"));
}

test "unknown paths are not found, trailing slashes included" {
    var r = try testRouter();
    defer r.deinit();
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    try testing.expectEqual(.not_found, try r.lookup(arena.allocator(), .GET, "/api/v1/nope"));
    try testing.expectEqual(.not_found, try r.lookup(arena.allocator(), .GET, "/api/v1/health/"));
    try testing.expectEqual(.not_found, try r.lookup(arena.allocator(), .GET, "/"));
    try testing.expectEqual(.not_found, try r.lookup(arena.allocator(), .GET, "/api/v1/downloads/"));
}

test "a wrong method gets 405 with a sorted Allow list" {
    var r = try testRouter();
    defer r.deinit();
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const m = try r.lookup(arena.allocator(), .DELETE, "/api/v1/downloads");
    try testing.expectEqualStrings("GET, HEAD, POST", m.method_not_allowed);
    const h = try r.lookup(arena.allocator(), .POST, "/api/v1/health");
    try testing.expectEqualStrings("GET, HEAD", h.method_not_allowed);
}

test "unclean paths redirect to the clean path" {
    var r = try testRouter();
    defer r.deinit();
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = try r.lookup(arena.allocator(), .GET, "/api//v1/./health");
    try testing.expectEqualStrings("/api/v1/health", a.redirect);
    const b = try r.lookup(arena.allocator(), .GET, "/api/v1/downloads/../health");
    try testing.expectEqualStrings("/api/v1/health", b.redirect);
    const c = try r.lookup(arena.allocator(), .GET, "/nope/../../x/");
    try testing.expectEqualStrings("/x/", c.redirect);
}

test "a subtree matches below it and /tree redirects to /tree/" {
    var r = try testRouter();
    defer r.deinit();
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    try expectFound(9, try r.lookup(arena.allocator(), .PUT, "/api/v2/torrents/add"));
    try expectFound(9, try r.lookup(arena.allocator(), .GET, "/api/v2/"));
    const m = try r.lookup(arena.allocator(), .GET, "/api/v2");
    try testing.expectEqualStrings("/api/v2/", m.redirect);
    // Both forms registered: neither redirects.
    try expectFound(7, try r.lookup(arena.allocator(), .GET, "/sabnzbd/api"));
    try expectFound(8, try r.lookup(arena.allocator(), .GET, "/sabnzbd/api/x"));
}

test "bad paths" {
    var r = try testRouter();
    defer r.deinit();
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    try testing.expectEqual(.bad_path, try r.lookup(arena.allocator(), .GET, ""));
    try testing.expectEqual(.bad_path, try r.lookup(arena.allocator(), .GET, "api/v1/health"));
    try testing.expectEqual(.bad_path, try r.lookup(arena.allocator(), .GET, "/api/%zz"));
    try testing.expectEqual(.bad_path, try r.lookup(arena.allocator(), .GET, "/api/%4"));
    try testing.expectEqual(.bad_path, try r.lookup(arena.allocator(), .GET, "/api/%"));
}

test "patterns are validated and duplicates refused" {
    var r: Router(u32) = .init(testing.allocator);
    defer r.deinit();
    try testing.expectError(error.InvalidPattern, r.add("api", 1));
    try testing.expectError(error.InvalidPattern, r.add("FETCH /x", 1));
    try testing.expectError(error.InvalidPattern, r.add("GET /a//b", 1));
    try testing.expectError(error.InvalidPattern, r.add("GET /a/{}", 1));
    try testing.expectError(error.InvalidPattern, r.add("GET /a/{x-y}", 1));
    try testing.expectError(error.InvalidPattern, r.add("GET /a/b{x}", 1));
    try r.add("GET /a/{x}", 1);
    try testing.expectError(error.DuplicatePattern, r.add("GET /a/{y}", 2));
    try r.add("POST /a/{y}", 2);
}

test "a method-specific pattern beats a method-less one" {
    var r: Router(u32) = .init(testing.allocator);
    defer r.deinit();
    try r.add("/x", 1);
    try r.add("GET /x", 2);
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    try expectFound(2, try r.lookup(arena.allocator(), .GET, "/x"));
    try expectFound(1, try r.lookup(arena.allocator(), .POST, "/x"));
}

fn fuzzLookup(r: *const Router(u32), smith: *testing.Smith) !void {
    var buf: [256]u8 = undefined;
    const n = smith.slice(&buf);
    const path = buf[0..n];
    const method = smith.value(Method);
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();

    const m = try r.lookup(a, method, path);
    switch (m) {
        .redirect => |loc| {
            // The target of a redirect is clean: it never redirects again
            // (except /tree -> /tree/, which is then found or not found).
            try testing.expect(loc.len > 0 and loc[0] == '/');
            const again = try r.lookup(a, method, loc);
            switch (again) {
                .redirect => return error.RedirectLoop,
                else => {},
            }
        },
        .found => |f| for (f.params) |p| try testing.expect(p.name.len > 0),
        else => {},
    }

    // cleanPath is idempotent and never leaves a dot segment or a double slash.
    if (path.len > 0) {
        const c1 = try cleanPath(a, path);
        const c2 = try cleanPath(a, c1);
        try testing.expectEqualStrings(c1, c2);
        try testing.expect(c1[0] == '/');
        try testing.expect(std.mem.find(u8, c1, "//") == null);
        try testing.expect(std.mem.find(u8, c1, "/./") == null);
        try testing.expect(!std.mem.endsWith(u8, c1, "/.."));
        try testing.expect(std.mem.find(u8, c1, "/../") == null);
    }
}

fn fuzzSeed(comptime s: []const u8, comptime method: u8) []const u8 {
    return &(std.mem.toBytes(@as(u32, s.len)) ++ s.* ++ [_]u8{method});
}

test "fuzz: router lookup and path cleaning" {
    var r = try testRouter();
    defer r.deinit();
    try testing.fuzz(&r, fuzzLookup, .{ .corpus = &.{
        fuzzSeed("/api/v1/health", 0),
        fuzzSeed("/api//v1/./health", 0),
        fuzzSeed("/api/v1/downloads/%2e%2e/x", 0),
        fuzzSeed("/api/v2", 1),
        fuzzSeed("/a/b/../../../c/", 2),
        fuzzSeed("/api/v1/%zz", 0),
    } });
}
