package main

import (
	"flag"
	"os"
	"fmt"

	"github.com/andyxning/shortme/conf"
	"github.com/andyxning/shortme/web"
	"github.com/andyxning/shortme/short"
)


func main() {
	cfgFile := flag.String("c", "config.conf", "configuration file")
	version := flag.Bool("v", false, "Version")

	flag.Parse()

	if *version {
		fmt.Println(conf.Version)
		os.Exit(0)
	}

	// parse config
	conf.MustParseConfig(*cfgFile)

	// short service
	short.Start()

	// api
	web.Start()
}
