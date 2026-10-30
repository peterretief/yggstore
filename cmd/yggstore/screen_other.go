//go:build !linux

package main

import (
	"errors"
	"os"
)

func rawTerminal(*os.File) (func(), error) {
	return nil, errors.New("the box's screen only runs on Linux; -once works anywhere")
}
