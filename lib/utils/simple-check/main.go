// Package main ...
package main

import (
	"github.com/go-rod/rod/lib/utils"
)

func main() {
	utils.Exec("go run ./lib/utils/setup")

	utils.Exec("go run ./lib/utils/lint")

	utils.Exec("go test ./lib/launcher")

	utils.Exec("go test")
}
