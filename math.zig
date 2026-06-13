const std = @import("std");

// WASM linear memory is 16 pages (1MB) declared via export.
// We use the first 256KB as a bump arena for per-call allocs.
// This is safe because all allocs are freed within the same RPC call.
const ARENA_SIZE: usize = 256 * 1024;
var arena_buf: [ARENA_SIZE]u8 align(16) = undefined;
var arena_offset: usize = 0;

fn arena_alloc(len: usize, comptime T: type) ?[]T {
    const byte_len = len * @sizeOf(T);
    const aligned = (arena_offset + @alignOf(T) - 1) & ~(@alignOf(T) - 1);
    if (aligned + byte_len > ARENA_SIZE) return null;
    const ptr: [*]T = @ptrCast(@alignCast(&arena_buf[aligned]));
    arena_offset = aligned + byte_len;
    return ptr[0..len];
}

fn arena_reset() void {
    arena_offset = 0;
}

export fn alloc(len: usize) [*]f32 {
    const slice = arena_alloc(len, f32) orelse return @ptrFromInt(0);
    return slice.ptr;
}

export fn free(_: [*]f32, _: usize) void {
    // Batch-freed via arena_reset after cosine_similarity returns.
    // Individual free is a no-op in the arena model.
}

export fn cosine_similarity(a_ptr: [*]const f32, b_ptr: [*]const f32, len: usize) f32 {
    var dot_product: f32 = 0.0;
    var norm_a: f32 = 0.0;
    var norm_b: f32 = 0.0;
    var i: usize = 0;
    while (i < len) : (i += 1) {
        const va = a_ptr[i];
        const vb = b_ptr[i];
        dot_product += va * vb;
        norm_a += va * va;
        norm_b += vb * vb;
    }
    // Reset arena after computation — both alloc'd buffers are no longer needed.
    arena_reset();
    if (norm_a == 0.0 or norm_b == 0.0) return 0.0;
    return dot_product / (@sqrt(norm_a) * @sqrt(norm_b));
}
