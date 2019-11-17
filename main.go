package main

import (
	"fmt"
	"git.byted.org/ee/bear/corgi/protocol"
	"os"
)

func main() {
	tabs, err := protocol.GetTabList("127.0.0.1:" + os.Args[1])
	if err != nil {
		fmt.Printf("error: %s\n", err)
		return
	}
	fmt.Println(len(tabs))
	for _, t := range tabs {
		fmt.Printf("tabs: %+v\n", t)
	}
}
