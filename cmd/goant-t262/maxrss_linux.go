//go:build linux

package main

// maxRSSUnit converts ru_maxrss to bytes. Linux reports kilobytes.
const maxRSSUnit = 1024
