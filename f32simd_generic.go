//go:build !(amd64 || arm64) || purego

package main

// No vector kernels on this platform (or -tags purego): the Go loops in
// f32simd.go do all the work.
var f32SIMD = false

const f32Lanes = 8

func f32AxpyAsm(dst, src *float32, n int, a float32)                 { panic("unreachable") }
func f32Axpy4Asm(dst, s0, s1, s2, s3 *float32, n int, w *[4]float32) { panic("unreachable") }
func f32DotAsm(a, b *float32, n int, part *[4]float32)               { panic("unreachable") }
