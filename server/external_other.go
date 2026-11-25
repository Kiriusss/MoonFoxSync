//go:build !darwin && !linux

package server

import (
	"errors"
	"os"
)

var errUnsupportedNAS = errors.New("external NAS roots require Linux or macOS")

func openRegisteredRoot(string) (*os.File, error)           { return nil, errUnsupportedNAS }
func fileState(*os.File) (nodeState, error)                 { return nodeState{}, errUnsupportedNAS }
func statChild(*os.File, string) (nodeState, error)         { return nodeState{}, errUnsupportedNAS }
func (x *externalRoot) open(string, bool) (*os.File, error) { return nil, errUnsupportedNAS }
