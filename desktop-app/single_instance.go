package main

import "errors"

var errSingleInstanceAlreadyRunning = errors.New("moon bridge desktop is already running")

var errSingleInstanceUnsupported = errors.New("single instance is unsupported on this platform")
