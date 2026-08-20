package main

import "os"

func main() {
	os.Exit(Main(os.Args[1:], os.Stdout, os.Stderr))
}
