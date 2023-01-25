package main

import (
	"flag"
	"log"

	"github.com/gokrazy/imaged/internal/imaged"
)

func main() {
	flag.Parse()
	if err := imaged.Main(); err != nil {
		log.Fatal(err)
	}
}
