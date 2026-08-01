//go:build windows

package jitmem

import (
	"fmt"
	"syscall"
	"unsafe"
)

// Win32 through syscall.LazyDLL rather than cgo. Go's own standard library
// reaches kernel32 this way, so a JIT that does the same keeps CGO_ENABLED=0 and
// stays cross-compilable from any host.
var (
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procVirtualAlloc          = kernel32.NewProc("VirtualAlloc")
	procVirtualProtect        = kernel32.NewProc("VirtualProtect")
	procVirtualFree           = kernel32.NewProc("VirtualFree")
	procFlushInstructionCache = kernel32.NewProc("FlushInstructionCache")
	procGetCurrentProcess     = kernel32.NewProc("GetCurrentProcess")
)

const (
	memCommit  = 0x1000
	memReserve = 0x2000
	memRelease = 0x8000

	pageReadWrite   = 0x04
	pageExecuteRead = 0x20
)

func allocRW(size int) ([]byte, error) {
	addr, _, err := procVirtualAlloc.Call(0, uintptr(size), memReserve|memCommit, pageReadWrite)
	if addr == 0 {
		return nil, fmt.Errorf("jitmem: VirtualAlloc: %w", err)
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(addr)), size), nil
}

func protectRX(mem []byte) error {
	var old uint32
	base := uintptr(unsafe.Pointer(&mem[0]))
	ok, _, err := procVirtualProtect.Call(base, uintptr(len(mem)), pageExecuteRead,
		uintptr(unsafe.Pointer(&old)))
	if ok == 0 {
		return fmt.Errorf("jitmem: VirtualProtect: %w", err)
	}
	return nil
}

func release(mem []byte) error {
	base := uintptr(unsafe.Pointer(&mem[0]))
	// MEM_RELEASE requires a zero size and the address VirtualAlloc returned.
	ok, _, err := procVirtualFree.Call(base, 0, memRelease)
	if ok == 0 {
		return fmt.Errorf("jitmem: VirtualFree: %w", err)
	}
	return nil
}

// flushWindowsICache asks the kernel to do the cache maintenance. On x64 this
// is close to free — the caches are coherent and the call exists so that code
// written for both architectures does not have to branch — and on arm64 it is
// the documented way to publish freshly written code.
func flushWindowsICache(mem []byte) {
	proc, _, _ := procGetCurrentProcess.Call()
	procFlushInstructionCache.Call(proc, uintptr(unsafe.Pointer(&mem[0])), uintptr(len(mem)))
}
