package main

import (
	"os"

	"github.com/denifilatoff/reviewctl/internal/reviewctl"
)

func main() {
	os.Exit(reviewctl.Main(os.Args[1:], os.Stdout, os.Stderr))
}
