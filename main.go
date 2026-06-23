package main

import (
	"fmt"
	"os"

	"github.com/ylnhari/rover/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "rover:", err)
		os.Exit(1)
	}
}
