// console_windows.go — comportement « application » sous Windows.
//
// P4Relay.exe est lié en sous-système GUI (-H windowsgui, voir build.bat) :
// lancé par double-clic, il n'ouvre AUCUNE fenêtre de commande — seule
// l'interface apparaît. Un binaire GUI n'a pourtant pas de console, donc ses
// messages seraient perdus : on rattache explicitement celle du terminal
// appelant quand il y en a une (lancement depuis cmd.exe ou PowerShell), et on
// n'écrase jamais des descripteurs déjà valides (sortie redirigée vers un
// fichier ou un tube : `P4Relay.exe > journal.txt` continue de fonctionner).
//
// Sans console, une erreur de démarrage resterait invisible : notifyFatal
// affiche alors une boîte de dialogue Windows.

//go:build windows

package main

import (
	"fmt"
	"log"
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32          = syscall.NewLazyDLL("kernel32.dll")
	user32            = syscall.NewLazyDLL("user32.dll")
	procGetStdHandle  = kernel32.NewProc("GetStdHandle")
	procAttachConsole = kernel32.NewProc("AttachConsole")
	procMessageBoxW   = user32.NewProc("MessageBoxW")
)

const (
	stdOutputHandle     = ^uintptr(10) // STD_OUTPUT_HANDLE = -11
	stdErrorHandle      = ^uintptr(11) // STD_ERROR_HANDLE  = -12
	attachParentProcess = ^uintptr(0)  // ATTACH_PARENT_PROCESS = -1
	invalidHandleValue  = ^uintptr(0)
	mbIconError         = 0x00000010
)

// outputAvailable : y a-t-il un endroit où écrire les messages (console réelle
// ou sortie redirigée vers un fichier/un tube) ? Sinon, une erreur de démarrage
// doit passer par une boîte de dialogue.
var outputAvailable bool

// setupConsole prépare la sortie du programme. À appeler en tout premier dans
// main().
func setupConsole() {
	// Un binaire GUI démarré depuis un terminal n'hérite PAS de sa console :
	// ses descripteurs standard sont invalides, sauf si l'appelant les a
	// redirigés (`P4Relay.exe > journal.txt`). On ne touche donc à rien quand
	// un destinataire existe déjà — la redirection doit toujours gagner — et on
	// ne se raccorde à la console du parent que s'il n'y a rien.
	if shouldAttachConsole(handleValid(stdOutputHandle), handleValid(stdErrorHandle)) {
		if r, _, _ := procAttachConsole.Call(attachParentProcess); r != 0 {
			rebindStdio()
		}
	}
	outputAvailable = handleValid(stdErrorHandle)
	if outputAvailable {
		log.SetOutput(os.Stderr)
	}
}

// shouldAttachConsole : faut-il se raccorder à la console du processus parent ?
// Uniquement si aucun descripteur standard n'est utilisable. Dès qu'un
// destinataire existe (console héritée ou redirection vers un fichier/un tube),
// s'y raccorder écraserait la sortie demandée par l'appelant.
func shouldAttachConsole(stdoutValid, stderrValid bool) bool {
	return !stdoutValid && !stderrValid
}

// handleValid : le descripteur standard correspondant est-il utilisable ?
func handleValid(which uintptr) bool {
	h, _, _ := procGetStdHandle.Call(which)
	return h != 0 && h != invalidHandleValue
}

// rebindStdio rebranche les descripteurs standard sur la console rattachée :
// sans cela, os.Stdout pointerait toujours sur un handle invalide et les
// messages n'apparaîtraient nulle part.
func rebindStdio() {
	if f, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0); err == nil {
		os.Stdout, os.Stderr = f, f
	}
	if f, err := os.OpenFile("CONIN$", os.O_RDONLY, 0); err == nil {
		os.Stdin = f
	}
}

// notifyFatal rend visible une erreur de démarrage fatale : dans la console ou
// la sortie redirigée si elles existent, sinon dans une boîte de dialogue
// Windows. Sans cela, un exe lancé par double-clic mourrait en silence.
func notifyFatal(msg string) {
	if outputAvailable {
		fmt.Fprintln(os.Stderr, msg)
		return
	}
	showMessageBox("P4Relay", msg)
}

// showMessageBox affiche une boîte de dialogue d'erreur (bloquante).
func showMessageBox(title, msg string) {
	t, err := syscall.UTF16PtrFromString(title)
	if err != nil {
		return
	}
	m, err := syscall.UTF16PtrFromString(msg)
	if err != nil {
		return
	}
	procMessageBoxW.Call(0,
		uintptr(unsafe.Pointer(m)),
		uintptr(unsafe.Pointer(t)),
		mbIconError)
}
