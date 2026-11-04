//go:build !unix

package main

func setLowPriority(pid int) {}
