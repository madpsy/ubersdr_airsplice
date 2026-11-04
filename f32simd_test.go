package main

import (
	"math"
	"math/rand"
	"testing"
)

func withF32SIMD(on bool, fn func()) {
	saved := f32SIMD
	f32SIMD = on
	defer func() { f32SIMD = saved }()
	fn()
}

func randF32(r *rand.Rand, n int) []float32 {
	x := make([]float32, n)
	for i := range x {
		x[i] = float32(r.NormFloat64())
	}
	return x
}

func closeF32(a, b, scale float32) bool {
	return math.Abs(float64(a-b)) <= 1e-5*float64(scale)+1e-6
}

// The kernels must agree with the Go loops at every length (so every tail
// path runs) and at unaligned starting offsets.
func TestF32KernelsMatchGo(t *testing.T) {
	if !f32SIMD {
		t.Skip("no vector kernels on this machine / build")
	}
	r := rand.New(rand.NewSource(1))
	for n := 0; n <= 133; n++ {
		for _, off := range []int{0, 1, 3} {
			src := randF32(r, n+off)[off:]
			s1, s2, s3 := randF32(r, n), randF32(r, n), randF32(r, n)
			base := randF32(r, n+off)[off:]
			w := [4]float32{0.7, -1.3, 0.25, 2}

			want := append([]float32(nil), base...)
			got := append([]float32(nil), base...)
			withF32SIMD(false, func() { f32Axpy(want, src, -0.375) })
			f32Axpy(got, src, -0.375)
			for i := range got {
				if !closeF32(got[i], want[i], 4) {
					t.Fatalf("axpy n=%d off=%d [%d]: %v vs %v", n, off, i, got[i], want[i])
				}
			}

			want = append(want[:0], base...)
			got = append(got[:0], base...)
			withF32SIMD(false, func() { f32Axpy4(want, src, s1, s2, s3, &w) })
			f32Axpy4(got, src, s1, s2, s3, &w)
			for i := range got {
				if !closeF32(got[i], want[i], 16) {
					t.Fatalf("axpy4 n=%d off=%d [%d]: %v vs %v", n, off, i, got[i], want[i])
				}
			}

			var dw, dg float32
			withF32SIMD(false, func() { dw = f32Dot(src, s1) })
			dg = f32Dot(src, s1)
			if !closeF32(dg, dw, float32(n)+1) {
				t.Fatalf("dot n=%d off=%d: %v vs %v", n, off, dg, dw)
			}
		}
	}
}

func benchAxpy4(b *testing.B, simd bool) {
	r := rand.New(rand.NewSource(2))
	n := 32000 // one 64x500 feature map
	dst, s0, s1, s2, s3 := randF32(r, n), randF32(r, n), randF32(r, n), randF32(r, n), randF32(r, n)
	w := [4]float32{1, 2, 3, 4}
	withF32SIMD(simd && f32SIMD, func() {
		b.SetBytes(int64(n * 4 * 5))
		for i := 0; i < b.N; i++ {
			f32Axpy4(dst, s0, s1, s2, s3, &w)
		}
	})
}

func BenchmarkF32Axpy4SIMD(b *testing.B) { benchAxpy4(b, true) }
func BenchmarkF32Axpy4Go(b *testing.B)   { benchAxpy4(b, false) }
