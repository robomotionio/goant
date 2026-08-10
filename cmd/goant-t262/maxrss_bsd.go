//go:build darwin || freebsd || netbsd || openbsd

package main

// maxRSSUnit converts ru_maxrss to bytes. The BSDs, macOS included, report it
// in bytes already.
const maxRSSUnit = 1
