//go:build arm64 && !purego

package main

// f32SIMD selects the NEON kernels, which every arm64 core has. A variable so
// the tests can compare both paths.
var f32SIMD = true

// f32Lanes is the element count the kernels work in (one 128-bit register).
const f32Lanes = 4

//go:noescape
func f32AxpyAsm(dst, src *float32, n int, a float32)

//go:noescape
func f32Axpy4Asm(dst, s0, s1, s2, s3 *float32, n int, w *[4]float32)

//go:noescape
func f32DotAsm(a, b *float32, n int, part *[4]float32)
