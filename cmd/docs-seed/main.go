package main

import (
	"fmt"
	"os"

	"github.com/Makia9879/docs-seed/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}
