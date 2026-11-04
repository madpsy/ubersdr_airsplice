//go:build arm64 && !purego

#include "textflag.h"

// NEON kernels for f32simd.go. Each handles n elements, n a multiple of 4;
// the Go wrappers do the tail.

// func f32AxpyAsm(dst, src *float32, n int, a float32)
TEXT ·f32AxpyAsm(SB), NOSPLIT, $0-28
	MOVD  dst+0(FP), R0
	MOVD  src+8(FP), R1
	MOVD  n+16(FP), R2
	FMOVS a+24(FP), F16
	VDUP  V16.S[0], V16.S4

axpy16:
	CMP    $16, R2
	BLT    axpy4
	VLD1   (R0), [V0.S4, V1.S4, V2.S4, V3.S4]
	VLD1.P 64(R1), [V4.S4, V5.S4, V6.S4, V7.S4]
	VFMLA  V16.S4, V4.S4, V0.S4
	VFMLA  V16.S4, V5.S4, V1.S4
	VFMLA  V16.S4, V6.S4, V2.S4
	VFMLA  V16.S4, V7.S4, V3.S4
	VST1.P [V0.S4, V1.S4, V2.S4, V3.S4], 64(R0)
	SUB    $16, R2
	B      axpy16

axpy4:
	CMP    $4, R2
	BLT    axpydone
	VLD1   (R0), [V0.S4]
	VLD1.P 16(R1), [V4.S4]
	VFMLA  V16.S4, V4.S4, V0.S4
	VST1.P [V0.S4], 16(R0)
	SUB    $4, R2
	B      axpy4

axpydone:
	RET

// func f32Axpy4Asm(dst, s0, s1, s2, s3 *float32, n int, w *[4]float32)
TEXT ·f32Axpy4Asm(SB), NOSPLIT, $0-56
	MOVD dst+0(FP), R0
	MOVD s0+8(FP), R1
	MOVD s1+16(FP), R2
	MOVD s2+24(FP), R3
	MOVD s3+32(FP), R4
	MOVD n+40(FP), R5
	MOVD w+48(FP), R6
	VLD1 (R6), [V20.S4]
	VDUP V20.S[0], V16.S4
	VDUP V20.S[1], V17.S4
	VDUP V20.S[2], V18.S4
	VDUP V20.S[3], V19.S4

axpy4x8:
	CMP    $8, R5
	BLT    axpy4x4
	VLD1   (R0), [V0.S4, V1.S4]
	VLD1.P 32(R1), [V2.S4, V3.S4]
	VFMLA  V16.S4, V2.S4, V0.S4
	VFMLA  V16.S4, V3.S4, V1.S4
	VLD1.P 32(R2), [V4.S4, V5.S4]
	VFMLA  V17.S4, V4.S4, V0.S4
	VFMLA  V17.S4, V5.S4, V1.S4
	VLD1.P 32(R3), [V6.S4, V7.S4]
	VFMLA  V18.S4, V6.S4, V0.S4
	VFMLA  V18.S4, V7.S4, V1.S4
	VLD1.P 32(R4), [V2.S4, V3.S4]
	VFMLA  V19.S4, V2.S4, V0.S4
	VFMLA  V19.S4, V3.S4, V1.S4
	VST1.P [V0.S4, V1.S4], 32(R0)
	SUB    $8, R5
	B      axpy4x8

axpy4x4:
	CMP    $4, R5
	BLT    axpy4done
	VLD1   (R0), [V0.S4]
	VLD1.P 16(R1), [V2.S4]
	VFMLA  V16.S4, V2.S4, V0.S4
	VLD1.P 16(R2), [V3.S4]
	VFMLA  V17.S4, V3.S4, V0.S4
	VLD1.P 16(R3), [V4.S4]
	VFMLA  V18.S4, V4.S4, V0.S4
	VLD1.P 16(R4), [V5.S4]
	VFMLA  V19.S4, V5.S4, V0.S4
	VST1.P [V0.S4], 16(R0)
	SUB    $4, R5
	B      axpy4x4

axpy4done:
	RET

// func f32DotAsm(a, b *float32, n int, part *[4]float32)
// The four lane sums are written to part; the caller adds them.
TEXT ·f32DotAsm(SB), NOSPLIT, $0-32
	MOVD  a+0(FP), R0
	MOVD  b+8(FP), R1
	MOVD  n+16(FP), R2
	MOVD  part+24(FP), R3
	VEOR  V0.B16, V0.B16, V0.B16
	VEOR  V1.B16, V1.B16, V1.B16
	VEOR  V2.B16, V2.B16, V2.B16
	VEOR  V3.B16, V3.B16, V3.B16

dot16:
	CMP    $16, R2
	BLT    dot4
	VLD1.P 64(R0), [V4.S4, V5.S4, V6.S4, V7.S4]
	VLD1.P 64(R1), [V8.S4, V9.S4, V10.S4, V11.S4]
	VFMLA  V4.S4, V8.S4, V0.S4
	VFMLA  V5.S4, V9.S4, V1.S4
	VFMLA  V6.S4, V10.S4, V2.S4
	VFMLA  V7.S4, V11.S4, V3.S4
	SUB    $16, R2
	B      dot16

dot4:
	CMP    $4, R2
	BLT    dotdone
	VLD1.P 16(R0), [V4.S4]
	VLD1.P 16(R1), [V8.S4]
	VFMLA  V4.S4, V8.S4, V0.S4
	SUB    $4, R2
	B      dot4

dotdone:
	WORD $0x4e21d400 // fadd v0.4s, v0.4s, v1.4s
	WORD $0x4e23d442 // fadd v2.4s, v2.4s, v3.4s
	WORD $0x4e22d400 // fadd v0.4s, v0.4s, v2.4s
	VST1  [V0.S4], (R3)
	RET
