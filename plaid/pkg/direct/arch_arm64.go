//go:build linux && arm64

package direct

import "golang.org/x/sys/unix"

// auditArch is the architecture the filter's system call numbers are for.
const auditArch = unix.AUDIT_ARCH_AARCH64

// abiBit marks the calls of a second ABI of this architecture with the same audit
// architecture: there is none.
const abiBit = 0
