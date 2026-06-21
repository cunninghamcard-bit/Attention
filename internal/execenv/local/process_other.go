//go:build !unix

package local

import "os/exec"

func configureProcessTreeCancel(*exec.Cmd) {}
