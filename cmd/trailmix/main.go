package main

import (
	"fmt"
	"os"

	"github.com/cwebley/trailmix/internal/recorder"
)

// version is stamped by the Makefile with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) == 2 && (os.Args[1] == "version" || os.Args[1] == "--version") {
		fmt.Println("trailmix", version)
		return
	}
	if err := recorder.Run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "trailmix:", err)
		os.Exit(1)
	}
}
