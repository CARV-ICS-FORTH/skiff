//go:build linux && amd64

package direct

import "golang.org/x/sys/unix"

// auditArch is the architecture the filter's system call numbers are for.
const auditArch = unix.AUDIT_ARCH_X86_64

// abiBit marks the calls of x32, a second ABI of this architecture: the same audit
// architecture, other numbers.
const abiBit = 0x40000000
