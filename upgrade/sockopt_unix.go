//go:build linux || darwin

package upgrade

import "syscall"

// soReusePort is the SO_REUSEPORT socket option. The constant is available
// in the syscall package on both Linux and Darwin.
const soReusePort = syscall.SO_REUSEPORT
