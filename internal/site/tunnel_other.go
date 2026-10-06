//go:build !linux

package site

import "os/exec"

func setChildAttrs(cmd *exec.Cmd) {}
