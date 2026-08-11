//go:build unix

package claudecode

import "os"

func runningAsRoot() bool { return os.Geteuid() == 0 }
