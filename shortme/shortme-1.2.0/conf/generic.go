package conf

import (
	"log"
	"runtime"
)

const Version = "1.2.0"

func init() {
	runtime.GOMAXPROCS(runtime.NumCPU())
	log.SetFlags(log.LstdFlags | log.Lshortfile)
}
