// console_other.go — hors Windows, le programme est un binaire console
// ordinaire : rien à préparer, et une erreur fatale s'écrit sur la sortie
// d'erreur standard.

//go:build !windows

package main

import (
	"fmt"
	"os"
)

// setupConsole : aucun aménagement nécessaire hors Windows.
func setupConsole() {}

// notifyFatal écrit l'erreur sur la sortie d'erreur standard.
func notifyFatal(msg string) {
	fmt.Fprintln(os.Stderr, msg)
}
