package fsatomic

import "syscall"

// sysRenameat2 is renameat2's system call number on linux/arm64.
const sysRenameat2 = syscall.SYS_RENAMEAT2
