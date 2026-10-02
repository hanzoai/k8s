//go:build linux

package main

// sysSetns is setns(2), which the frozen syscall package does not number.
const sysSetns = 268
