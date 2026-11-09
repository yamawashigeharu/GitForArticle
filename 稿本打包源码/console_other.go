//go:build !windows

package main

import (
	"bufio"
	"os"
	"strings"
)

var stdin = bufio.NewReader(os.Stdin)

func setupConsole() {}

func readLine() string {
	s, _ := stdin.ReadString('\n')
	return strings.TrimRight(s, "\r\n")
}
