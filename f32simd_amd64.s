//go:build amd64 && !purego

#include "textflag.h"

// AVX2 + FMA kernels for f32simd.go. Each handles n elements, n a multiple of
// 8; the Go wrappers do the tail. Unaligned loads throughout: the slices come
// from make([]float32) at arbitrary offsets.

// func f32AxpyAsm(dst, src *float32, n int, a float32)
TEXT ·f32AxpyAsm(SB), NOSPLIT, $0-28
	MOVQ         dst+0(FP), DI
	MOVQ         src+8(FP), SI
	MOVQ         n+16(FP), CX
	VBROADCASTSS a+24(FP), Y0

axpy32:
	CMPQ        CX, $32
	JLT         axpy8
	VMOVUPS     (DI), Y1
	VMOVUPS     32(DI), Y2
	VMOVUPS     64(DI), Y3
	VMOVUPS     96(DI), Y4
	VFMADD231PS (SI), Y0, Y1
	VFMADD231PS 32(SI), Y0, Y2
	VFMADD231PS 64(SI), Y0, Y3
	VFMADD231PS 96(SI), Y0, Y4
	VMOVUPS     Y1, (DI)
	VMOVUPS     Y2, 32(DI)
	VMOVUPS     Y3, 64(DI)
	VMOVUPS     Y4, 96(DI)
	ADDQ        $128, DI
	ADDQ        $128, SI
	SUBQ        $32, CX
	JMP         axpy32

axpy8:
	CMPQ        CX, $8
	JLT         axpydone
	VMOVUPS     (DI), Y1
	VFMADD231PS (SI), Y0, Y1
	VMOVUPS     Y1, (DI)
	ADDQ        $32, DI
	ADDQ        $32, SI
	SUBQ        $8, CX
	JMP         axpy8

axpydone:
	VZEROUPPER
	RET

// func f32Axpy4Asm(dst, s0, s1, s2, s3 *float32, n int, w *[4]float32)
TEXT ·f32Axpy4Asm(SB), NOSPLIT, $0-56
	MOVQ         dst+0(FP), DI
	MOVQ         s0+8(FP), R8
	MOVQ         s1+16(FP), R9
	MOVQ         s2+24(FP), R10
	MOVQ         s3+32(FP), R11
	MOVQ         n+40(FP), CX
	MOVQ         w+48(FP), AX
	VBROADCASTSS 0(AX), Y0
	VBROADCASTSS 4(AX), Y1
	VBROADCASTSS 8(AX), Y2
	VBROADCASTSS 12(AX), Y3

axpy4x16:
	CMPQ        CX, $16
	JLT         axpy4x8
	VMOVUPS     (DI), Y4
	VMOVUPS     32(DI), Y5
	VFMADD231PS (R8), Y0, Y4
	VFMADD231PS 32(R8), Y0, Y5
	VFMADD231PS (R9), Y1, Y4
	VFMADD231PS 32(R9), Y1, Y5
	VFMADD231PS (R10), Y2, Y4
	VFMADD231PS 32(R10), Y2, Y5
	VFMADD231PS (R11), Y3, Y4
	VFMADD231PS 32(R11), Y3, Y5
	VMOVUPS     Y4, (DI)
	VMOVUPS     Y5, 32(DI)
	ADDQ        $64, DI
	ADDQ        $64, R8
	ADDQ        $64, R9
	ADDQ        $64, R10
	ADDQ        $64, R11
	SUBQ        $16, CX
	JMP         axpy4x16

axpy4x8:
	CMPQ        CX, $8
	JLT         axpy4done
	VMOVUPS     (DI), Y4
	VFMADD231PS (R8), Y0, Y4
	VFMADD231PS (R9), Y1, Y4
	VFMADD231PS (R10), Y2, Y4
	VFMADD231PS (R11), Y3, Y4
	VMOVUPS     Y4, (DI)
	ADDQ        $32, DI
	ADDQ        $32, R8
	ADDQ        $32, R9
	ADDQ        $32, R10
	ADDQ        $32, R11
	SUBQ        $8, CX
	JMP         axpy4x8

axpy4done:
	VZEROUPPER
	RET

// func f32DotAsm(a, b *float32, n int, part *[4]float32)
// Four partial sums are written to part; the caller adds them.
TEXT ·f32DotAsm(SB), NOSPLIT, $0-32
	MOVQ   a+0(FP), SI
	MOVQ   b+8(FP), DI
	MOVQ   n+16(FP), CX
	MOVQ   part+24(FP), AX
	VXORPS Y0, Y0, Y0
	VXORPS Y1, Y1, Y1
	VXORPS Y2, Y2, Y2
	VXORPS Y3, Y3, Y3

dot32:
	CMPQ        CX, $32
	JLT         dot8
	VMOVUPS     (SI), Y4
	VMOVUPS     32(SI), Y5
	VMOVUPS     64(SI), Y6
	VMOVUPS     96(SI), Y7
	VFMADD231PS (DI), Y4, Y0
	VFMADD231PS 32(DI), Y5, Y1
	VFMADD231PS 64(DI), Y6, Y2
	VFMADD231PS 96(DI), Y7, Y3
	ADDQ        $128, SI
	ADDQ        $128, DI
	SUBQ        $32, CX
	JMP         dot32

dot8:
	CMPQ        CX, $8
	JLT         dotdone
	VMOVUPS     (SI), Y4
	VFMADD231PS (DI), Y4, Y0
	ADDQ        $32, SI
	ADDQ        $32, DI
	SUBQ        $8, CX
	JMP         dot8

dotdone:
	VADDPS       Y1, Y0, Y0
	VADDPS       Y3, Y2, Y2
	VADDPS       Y2, Y0, Y0
	VEXTRACTF128 $1, Y0, X1
	VADDPS       X1, X0, X0
	VMOVUPS      X0, (AX)
	VZEROUPPER
	RET

// func f32HasAVX2FMA() bool
TEXT ·f32HasAVX2FMA(SB), NOSPLIT, $0-1
	// Leaf 7 must exist.
	XORL AX, AX
	CPUID
	CMPL AX, $7
	JLT  no

	// FMA, OSXSAVE and AVX (leaf 1 ECX bits 12, 27 and 28) ...
	MOVL $1, AX
	XORL CX, CX
	CPUID
	ANDL $0x18001000, CX
	CMPL CX, $0x18001000
	JNE  no

	// ... the OS saving the XMM and YMM state (XCR0 bits 1 and 2) ...
	XORL   CX, CX
	XGETBV
	ANDL   $6, AX
	CMPL   AX, $6
	JNE    no

	// ... and AVX2 (leaf 7 EBX bit 5).
	MOVL $7, AX
	XORL CX, CX
	CPUID
	BTL  $5, BX
	JCC  no

	MOVB $1, ret+0(FP)
	RET

no:
	MOVB $0, ret+0(FP)
	RET
