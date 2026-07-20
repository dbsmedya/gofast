package main

import (
	"fmt"
	"os"

	"github.com/dbsmedya/gofast/internal/contracttest"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: genfixture <out-dir>")
		os.Exit(2)
	}
	if err := contracttest.WriteFixtureLogs(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
