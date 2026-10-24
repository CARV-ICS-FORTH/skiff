//go:build linux && arm64

package direct

import "golang.org/x/sys/unix"

// auditArch is the architecture the filter's system call numbers are for.
const auditArch = unix.AUDIT_ARCH_AARCH64
