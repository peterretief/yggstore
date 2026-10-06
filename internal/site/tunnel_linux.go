package site

import (
	"os/exec"
	"syscall"
)

// setChildAttrs stops the connector if the node dies without cleaning up
// (say, it is killed), so it never outlives the web server.
func setChildAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
}
