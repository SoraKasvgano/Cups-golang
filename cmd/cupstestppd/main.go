package main

import (
	"flag"
	"fmt"
	"os"

	"cupsgolang/internal/config"
)

func main() {
	flag.Parse()
	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "cupstestppd: missing ppd file")
		os.Exit(1)
	}
	path := flag.Arg(0)
	ppd, err := config.LoadPPD(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cupstestppd:", err)
		os.Exit(1)
	}
	if ppd == nil {
		fmt.Fprintln(os.Stderr, "cupstestppd: invalid ppd")
		os.Exit(1)
	}
	fmt.Println("OK")
}
