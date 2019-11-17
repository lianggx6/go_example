package main

import (
	"fmt"
	"git.byted.org/ee/bear/corgi/protocol"
)

func main() {
	tabs, err := protocol.GetTabList("10.27.35.135:9261")
	if err != nil {
		fmt.Printf("error: %s\n", err)
		return
	}
	fmt.Println(len(tabs))
	for _, t := range tabs {
		fmt.Printf("tabs: %+v\n", t)
	}
}
