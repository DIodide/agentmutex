//go:build unix && !linux

package main

import "syscall"

// Darwin and the BSDs use TIOCGETA for the termios read ioctl.
const ioctlReadTermios = syscall.TIOCGETA
