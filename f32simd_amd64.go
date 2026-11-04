//go:build amd64 && !purego

package main

// f32SIMD selects the AVX2+FMA kernels. A variable so the tests can compare
// both paths on the same machine.
var f32SIMD = f32HasAVX2FMA()

// f32Lanes is the element count the kernels work in; they handle n rounded
// down to a multiple of it and the Go wrappers do the rest.
const f32Lanes = 8

func f32HasAVX2FMA() bool

//go:noescape
func f32AxpyAsm(dst, src *float32, n int, a float32)

//go:noescape
func f32Axpy4Asm(dst, s0, s1, s2, s3 *float32, n int, w *[4]float32)

//go:noescape
func f32DotAsm(a, b *float32, n int, part *[4]float32)
