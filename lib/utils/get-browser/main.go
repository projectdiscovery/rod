// Package main ...
package main

import (
	"fmt"

	"github.com/projectdiscovery/rod/lib/launcher"
	"github.com/projectdiscovery/rod/lib/utils"
)

func main() {
	p, err := launcher.NewBrowser().Get()
	utils.E(err)

	fmt.Println(p)
}
