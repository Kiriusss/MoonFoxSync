package main

import (
	"fmt"
	"kirius.local/kiriusdrive/server"
	"os"
)

func main() {
	if e := server.Run(os.Args[1:], os.Stdout); e != nil {
		fmt.Fprintln(os.Stderr, "梦枢天罗:", e)
		os.Exit(1)
	}
}
