// appwindow.go — ouvrir l'interface P4Relay dans sa PROPRE fenêtre, comme une
// application native.
//
// Plutôt qu'un onglet de plus dans le navigateur, on lance un navigateur
// Chromium (Edge, toujours présent sur Windows 10/11 ; Chrome, Brave, Chromium
// ailleurs) en mode application : `--app=URL` donne une fenêtre sans onglets ni
// barre d'adresse, avec sa propre entrée dans la barre des tâches / le Dock.
// Aucun CGO : les binaires Windows/Linux restent cross-compilés.
//
// Un profil dédié (--user-data-dir) sépare la fenêtre P4Relay du navigateur
// habituel : entrée distincte dans la barre des tâches, et relancer P4Relay
// rouvre la fenêtre au lieu d'un onglet dans une session existante.
// Sans navigateur Chromium, on retombe sur le navigateur par défaut.

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// Variables d'environnement qui pilotent l'ouverture de l'interface.
const (
	// envBrowser=1 : navigateur classique au lieu de la fenêtre dédiée.
	envBrowser = "P4RELAY_BROWSER"
	// envNoWindow=1 : n'ouvrir aucune interface (service, script, sans écran).
	envNoWindow = "P4RELAY_NO_WINDOW"
)

// openInterface ouvre l'interface de la passerelle : fenêtre d'application
// dédiée, et à défaut navigateur par défaut. Rien n'est ouvert si
// P4RELAY_NO_WINDOW=1 ou si l'option --no-window est passée, ni si une fenêtre
// P4Relay est déjà ouverte (on ne double pas la fenêtre de l'instance en cours).
func openInterface(url string) {
	if noWindowRequested() || appWindowAlive() {
		return
	}
	_ = openAppWindow(url)
}

// noWindowRequested : l'utilisateur a demandé à ne rien ouvrir.
func noWindowRequested() bool {
	if os.Getenv(envNoWindow) == "1" {
		return true
	}
	for _, a := range os.Args[1:] {
		if a == "--no-window" {
			return true
		}
	}
	return false
}

// openAppWindow ouvre l'URL dans une fenêtre d'application dédiée, ou à défaut
// dans le navigateur par défaut. P4RELAY_BROWSER=1 force le navigateur
// classique.
func openAppWindow(url string) error {
	if os.Getenv(envBrowser) == "1" {
		return openBrowser(url)
	}
	bin := findChromium()
	if bin == "" {
		return openBrowser(url)
	}
	args := []string{
		"--app=" + url,
		"--no-first-run",
		"--no-default-browser-check",
		"--window-size=1200,860",
	}
	if dir := appWindowProfileDir(); dir != "" {
		args = append(args, "--user-data-dir="+dir)
	}
	// Surtout pas hideCmd : HideWindow (SW_HIDE) est appliqué par Edge à sa
	// première fenêtre, qui reste alors un cadre gris inerte.
	if err := exec.Command(bin, args...).Start(); err != nil {
		return openBrowser(url)
	}
	return nil
}

// openBrowser ouvre l'URL dans le navigateur par défaut du système.
func openBrowser(url string) error {
	var name string
	var args []string
	switch runtime.GOOS {
	case "windows":
		// rundll32 plutôt que explorer.exe : même effet, sans dépendre du shell.
		name, args = "rundll32", []string{"url.dll,FileProtocolHandler", url}
	case "darwin":
		name, args = "open", []string{url}
	default:
		name, args = "xdg-open", []string{url}
	}
	return exec.Command(name, args...).Start()
}

// appWindowAlive : une fenêtre P4Relay (profil dédié) est-elle encore ouverte ?
// Chromium tient le fichier « lockfile » de son profil ouvert en exclusif tant
// qu'il tourne (Windows) : s'il résiste à la suppression, la fenêtre vit. Un
// verrou orphelin, lui, se supprime sans dommage. Hors Windows (lien
// symbolique SingletonLock, sémantique différente) on répond non : rouvrir
// reste sûr.
func appWindowAlive() bool {
	if runtime.GOOS != "windows" {
		return false
	}
	dir := appWindowProfileDir()
	if dir == "" {
		return false
	}
	lock := filepath.Join(dir, "lockfile")
	if _, err := os.Stat(lock); err != nil {
		return false
	}
	return os.Remove(lock) != nil
}

// appWindowProfileDir : profil propre à la fenêtre P4Relay, dans le dossier de
// configuration de l'UTILISATEUR (le dossier de données peut être machine ou
// root, et l'exécutable peut vivre dans un dossier en lecture seule).
func appWindowProfileDir() string {
	base, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	dir := filepath.Join(base, "p4relay", "appwindow")
	if os.MkdirAll(dir, 0o700) != nil {
		return ""
	}
	return dir
}

// findChromium cherche un navigateur qui sait faire `--app`, Edge en premier
// sous Windows (installé d'office), Chrome en premier ailleurs.
func findChromium() string {
	var cands []string
	switch runtime.GOOS {
	case "windows":
		var roots []string
		for _, e := range []string{"ProgramFiles(x86)", "ProgramFiles", "LOCALAPPDATA"} {
			if v := os.Getenv(e); v != "" {
				roots = append(roots, v)
			}
		}
		for _, rel := range []string{
			`Microsoft\Edge\Application\msedge.exe`,
			`Google\Chrome\Application\chrome.exe`,
			`BraveSoftware\Brave-Browser\Application\brave.exe`,
			`Chromium\Application\chrome.exe`,
		} {
			for _, r := range roots {
				cands = append(cands, filepath.Join(r, rel))
			}
		}
	case "darwin":
		for _, app := range []string{
			"Google Chrome.app/Contents/MacOS/Google Chrome",
			"Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
			"Brave Browser.app/Contents/MacOS/Brave Browser",
			"Chromium.app/Contents/MacOS/Chromium",
		} {
			cands = append(cands, "/Applications/"+app)
			if h, err := os.UserHomeDir(); err == nil {
				cands = append(cands, filepath.Join(h, "Applications", app))
			}
		}
	default:
		for _, name := range []string{
			"google-chrome", "google-chrome-stable", "chromium", "chromium-browser",
			"microsoft-edge", "microsoft-edge-stable", "brave-browser",
		} {
			if p, err := exec.LookPath(name); err == nil {
				return p
			}
		}
	}
	for _, c := range cands {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			return c
		}
	}
	return ""
}
