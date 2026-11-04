// f32simd.go — float32 vector kernels for the analysis code.
//
// Three operations carry nearly all of the classifier's arithmetic:
//
//   - axpy:  dst += a*src           (depthwise convolution along time)
//   - axpy4: dst += Σ w[k]*src[k]   (pointwise convolution, four input
//     channels per pass so each output row is loaded and stored a quarter
//     as often)
//   - dot:   Σ a[i]*b[i]            (fully connected layers, resampler)
//
// Each has an assembly kernel — AVX2+FMA on amd64 (chosen at run time on
// CPUID), NEON on arm64 — and a pure Go loop for other platforms, for
// -tags purego, and for the tail a kernel's vector width leaves. The kernels
// fuse multiply and add and the dot product sums in a different order, so
// results differ from the Go loops in the last bits; nothing here depends on
// bit-exactness (the tests compare within float32 rounding).
package main

// f32Axpy computes dst[i] += a*src[i] over len(dst); src must be at least as long.
func f32Axpy(dst, src []float32, a float32) {
	n := len(dst)
	if n == 0 {
		return
	}
	_ = src[n-1]
	i := 0
	if f32SIMD && n >= f32Lanes {
		i = n &^ (f32Lanes - 1)
		f32AxpyAsm(&dst[0], &src[0], i, a)
	}
	for ; i < n; i++ {
		dst[i] += a * src[i]
	}
}

// f32Axpy4 computes dst[i] += w[0]*s0[i] + w[1]*s1[i] + w[2]*s2[i] + w[3]*s3[i].
func f32Axpy4(dst, s0, s1, s2, s3 []float32, w *[4]float32) {
	n := len(dst)
	if n == 0 {
		return
	}
	_, _, _, _ = s0[n-1], s1[n-1], s2[n-1], s3[n-1]
	i := 0
	if f32SIMD && n >= f32Lanes {
		i = n &^ (f32Lanes - 1)
		f32Axpy4Asm(&dst[0], &s0[0], &s1[0], &s2[0], &s3[0], i, w)
	}
	w0, w1, w2, w3 := w[0], w[1], w[2], w[3]
	for ; i < n; i++ {
		dst[i] += w0*s0[i] + w1*s1[i] + w2*s2[i] + w3*s3[i]
	}
}

// f32Dot returns Σ a[i]*b[i] over len(a); b must be at least as long.
func f32Dot(a, b []float32) float32 {
	n := len(a)
	if n == 0 {
		return 0
	}
	_ = b[n-1]
	var s float32
	i := 0
	if f32SIMD && n >= f32Lanes {
		i = n &^ (f32Lanes - 1)
		var part [4]float32
		f32DotAsm(&a[0], &b[0], i, &part)
		s = (part[0] + part[1]) + (part[2] + part[3])
	}
	for ; i < n; i++ {
		s += a[i] * b[i]
	}
	return s
}
