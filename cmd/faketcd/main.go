// Command faketcd is a stand-in for the nostar tcd, for tests and for a
// tcdpool run with no vocoders. See internal/faketcd.
package main

import (
	"os"

	"github.com/dbehnke/tcdpool/internal/faketcd"
)

func main() { os.Exit(faketcd.Main(os.Args[1:])) }
