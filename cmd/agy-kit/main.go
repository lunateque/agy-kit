package main

import (
	"fmt"
	"os"

	"github.com/lunateque/agy-kit/internal/advisor"
	"github.com/lunateque/agy-kit/internal/bridge"
	"github.com/lunateque/agy-kit/internal/telemetry"
)

const version = "0.2.0"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	var code int
	switch os.Args[1] {
	case "telemetry":
		code = telemetry.Run(os.Args[2:])
	case "statusline":
		code = telemetry.Run(append([]string{"statusline"}, os.Args[2:]...))
	case "stats":
		code = telemetry.Run(append([]string{"stats"}, os.Args[2:]...))
	case "bridge":
		code = bridge.Run()
	case "advisor":
		code = advisor.Run(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println("agy-kit", version)
		code = 0
	case "help", "--help", "-h":
		usage()
		code = 0
	default:
		fmt.Fprintln(os.Stderr, "unknown command:", os.Args[1])
		usage()
		code = 1
	}
	os.Exit(code)
}

func usage() {
	fmt.Println(`agy-kit — Zero-dependency toolkit for Antigravity CLI (agy)

Usage:
  agy-kit telemetry <hook|statusline|stats|config>   Telemetry hooks, stats, and statusline
  agy-kit statusline                                 Shorthand for 'telemetry statusline'
  agy-kit stats [today|all|last N]                   Shorthand for 'telemetry stats'
  agy-kit bridge                                     MCP stdio server (agy -> omp/claude)
  agy-kit advisor <hook|worker|status|enable|disable|reset|dump|doctor|config>
                                                     Shadow Opus advisor (omp-style)
  agy-kit version                                    Print version`)
}
